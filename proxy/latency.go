package proxy

import (
	"context"
	"crypto/tls"
	"math"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"
)

type latencyStage int

const (
	stageBodyRead latencyStage = iota
	stageConfigLockWait
	stageCacheLockWait
	stageAccountPoolWait
	stageConnect
	stageTLS
	stageUpstreamHeader
	stageUpstreamFirstEvent
	stageDownstreamPreludeFlush
	stageFirstVisibleTokenFlush
	latencyStageCount
)

var latencyStageNames = [...]string{
	"body_read",
	"config_lock_wait",
	"cache_lock_wait",
	"account_pool_wait",
	"connect",
	"tls",
	"upstream_header",
	"upstream_first_event",
	"downstream_prelude_flush",
	"first_visible_token_flush",
}

var latencyBucketUpperMicros = [...]uint64{
	1_000, 2_000, 5_000, 10_000, 20_000, 50_000, 100_000,
	250_000, 500_000, 1_000_000, 2_500_000, 5_000_000, 10_000_000,
	30_000_000, math.MaxUint64,
}

type latencyHistogram struct {
	count   atomic.Uint64
	totalUS atomic.Uint64
	maxUS   atomic.Uint64
	buckets [len(latencyBucketUpperMicros)]atomic.Uint64
}

type latencyTracker struct {
	stages [latencyStageCount]latencyHistogram
}

type LatencyStageSnapshot struct {
	Samples uint64  `json:"samples"`
	Average float64 `json:"avgMs"`
	P95     float64 `json:"p95Ms"`
	P99     float64 `json:"p99Ms"`
	Max     float64 `json:"maxMs"`
}

func (t *latencyTracker) observe(stage latencyStage, elapsed time.Duration) {
	if t == nil || stage < 0 || stage >= latencyStageCount {
		return
	}
	if elapsed < 0 {
		elapsed = 0
	}
	micros := uint64(elapsed / time.Microsecond)
	if micros == 0 && elapsed > 0 {
		micros = 1
	}
	h := &t.stages[stage]
	h.count.Add(1)
	h.totalUS.Add(micros)
	for {
		old := h.maxUS.Load()
		if micros <= old || h.maxUS.CompareAndSwap(old, micros) {
			break
		}
	}
	for i, upper := range latencyBucketUpperMicros {
		if micros <= upper {
			h.buckets[i].Add(1)
			break
		}
	}
}

func (t *latencyTracker) Snapshot() map[string]LatencyStageSnapshot {
	result := make(map[string]LatencyStageSnapshot, latencyStageCount)
	if t == nil {
		return result
	}
	for stage := latencyStage(0); stage < latencyStageCount; stage++ {
		h := &t.stages[stage]
		count := h.count.Load()
		if count == 0 {
			continue
		}
		result[latencyStageNames[stage]] = LatencyStageSnapshot{
			Samples: count,
			Average: float64(h.totalUS.Load()) / float64(count) / 1000,
			P95:     histogramQuantileMS(h, count, 0.95),
			P99:     histogramQuantileMS(h, count, 0.99),
			Max:     float64(h.maxUS.Load()) / 1000,
		}
	}
	return result
}

func histogramQuantileMS(h *latencyHistogram, count uint64, quantile float64) float64 {
	if h == nil || count == 0 {
		return 0
	}
	target := uint64(math.Ceil(float64(count) * quantile))
	var cumulative uint64
	for i, upper := range latencyBucketUpperMicros {
		cumulative += h.buckets[i].Load()
		if cumulative < target {
			continue
		}
		if upper == math.MaxUint64 {
			return float64(h.maxUS.Load()) / 1000
		}
		return float64(upper) / 1000
	}
	return float64(h.maxUS.Load()) / 1000
}

type requestTimingContextKey struct{}

type requestTiming struct {
	tracker *latencyTracker
	start   time.Time

	mu            sync.Mutex
	upstreamStart time.Time
	headerAt      time.Time
	connectStart  time.Time
	tlsStart      time.Time

	firstEventOnce          sync.Once
	preludeOnce             sync.Once
	visiblePending          atomic.Bool
	explicitVisibleTracking atomic.Bool
	visibleOnce             sync.Once
}

func withRequestTiming(ctx context.Context, tracker *latencyTracker) (context.Context, *requestTiming) {
	if ctx == nil {
		ctx = context.Background()
	}
	timing := &requestTiming{tracker: tracker, start: time.Now()}
	return context.WithValue(ctx, requestTimingContextKey{}, timing), timing
}

func requestTimingFromContext(ctx context.Context) *requestTiming {
	if ctx == nil {
		return nil
	}
	timing, _ := ctx.Value(requestTimingContextKey{}).(*requestTiming)
	return timing
}

func observeRequestStage(ctx context.Context, stage latencyStage, elapsed time.Duration) {
	if timing := requestTimingFromContext(ctx); timing != nil {
		timing.observe(stage, elapsed)
	}
}

func (t *requestTiming) observe(stage latencyStage, elapsed time.Duration) {
	if t != nil && t.tracker != nil {
		t.tracker.observe(stage, elapsed)
	}
}

func (t *requestTiming) beginUpstream(now time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.upstreamStart = now
	t.headerAt = time.Time{}
	t.mu.Unlock()
}

func (t *requestTiming) markUpstreamHeader(now time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if !t.headerAt.IsZero() {
		t.mu.Unlock()
		return
	}
	start := t.upstreamStart
	t.headerAt = now
	t.mu.Unlock()
	if !start.IsZero() {
		t.observe(stageUpstreamHeader, now.Sub(start))
	}
}

func (t *requestTiming) clientTrace() *httptrace.ClientTrace {
	if t == nil {
		return nil
	}
	return &httptrace.ClientTrace{
		ConnectStart: func(_, _ string) {
			t.mu.Lock()
			t.connectStart = time.Now()
			t.mu.Unlock()
		},
		ConnectDone: func(_, _ string, _ error) {
			now := time.Now()
			t.mu.Lock()
			started := t.connectStart
			t.connectStart = time.Time{}
			t.mu.Unlock()
			if !started.IsZero() {
				t.observe(stageConnect, now.Sub(started))
			}
		},
		TLSHandshakeStart: func() {
			t.mu.Lock()
			t.tlsStart = time.Now()
			t.mu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			now := time.Now()
			t.mu.Lock()
			started := t.tlsStart
			t.tlsStart = time.Time{}
			t.mu.Unlock()
			if !started.IsZero() {
				t.observe(stageTLS, now.Sub(started))
			}
		},
		GotFirstResponseByte: func() {
			t.markUpstreamHeader(time.Now())
		},
	}
}

func instrumentKiroCallbackTiming(timing *requestTiming, callback *KiroStreamCallback) *KiroStreamCallback {
	if timing == nil {
		return callback
	}
	if callback == nil {
		callback = &KiroStreamCallback{}
	}
	wired := *callback
	originalFirstEvent := wired.OnFirstEvent
	wired.OnFirstEvent = func() {
		timing.markFirstUpstreamEvent(time.Now())
		if originalFirstEvent != nil {
			originalFirstEvent()
		}
	}
	if original := wired.OnText; original != nil {
		wired.OnText = func(text string, isThinking bool) {
			if text != "" {
				timing.markUpstreamVisibleCandidate()
			}
			original(text, isThinking)
		}
	}
	if original := wired.OnReasoning; original != nil {
		wired.OnReasoning = func(event KiroReasoningContentEvent) {
			if event.Text != "" || event.Signature != "" || event.RedactedContent != "" {
				timing.markUpstreamVisibleCandidate()
			}
			original(event)
		}
	}
	if original := wired.OnToolUse; original != nil {
		wired.OnToolUse = func(toolUse KiroToolUse) {
			timing.markUpstreamVisibleCandidate()
			original(toolUse)
		}
	}
	return &wired
}

func (t *requestTiming) markFirstUpstreamEvent(now time.Time) {
	if t == nil {
		return
	}
	t.firstEventOnce.Do(func() {
		t.mu.Lock()
		start := t.upstreamStart
		t.mu.Unlock()
		if !start.IsZero() {
			t.observe(stageUpstreamFirstEvent, now.Sub(start))
		}
	})
}

func (t *requestTiming) markDownstreamPreludeFlushed(now time.Time) {
	if t == nil {
		return
	}
	t.preludeOnce.Do(func() {
		t.observe(stageDownstreamPreludeFlush, now.Sub(t.start))
	})
}

func (t *requestTiming) markVisibleCandidate() {
	if t != nil {
		t.visiblePending.Store(true)
	}
}

func (t *requestTiming) beginExplicitVisibleTracking() {
	if t != nil {
		t.explicitVisibleTracking.Store(true)
	}
}

func (t *requestTiming) markUpstreamVisibleCandidate() {
	if t == nil || t.explicitVisibleTracking.Load() {
		return
	}
	t.markVisibleCandidate()
}

func (t *requestTiming) markVisibleFlushed(now time.Time) {
	if t == nil || !t.visiblePending.Load() {
		return
	}
	t.visibleOnce.Do(func() {
		t.observe(stageFirstVisibleTokenFlush, now.Sub(t.start))
	})
}

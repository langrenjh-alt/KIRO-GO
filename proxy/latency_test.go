package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLatencyTrackerSnapshot(t *testing.T) {
	tracker := &latencyTracker{}
	tracker.observe(stageBodyRead, 1500*time.Microsecond)
	tracker.observe(stageBodyRead, 3*time.Millisecond)
	snapshot := tracker.Snapshot()["body_read"]
	if snapshot.Samples != 2 || snapshot.Average < 2.2 || snapshot.Average > 2.3 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.P95 != 5 || snapshot.Max != 3 {
		t.Fatalf("unexpected quantiles: %+v", snapshot)
	}
}

func TestBoundedResponseWriterCancelsOnWriteFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := newBoundedResponseWriter(failingResponseWriter{}, cancel, nil)
	if wrapped, _ := wrapBoundedResponseWriter(failingResponseWriter{}, cancel, nil); wrapped != nil {
		if _, ok := wrapped.(http.Flusher); ok {
			t.Fatal("wrapper invented Flusher support")
		}
	}
	if _, err := w.Write([]byte("data")); err == nil {
		t.Fatal("expected write error")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("write failure did not cancel request context")
	}
}

func TestBoundedResponseWriterFlushesFirstEventsImmediately(t *testing.T) {
	recorder := httptest.NewRecorder()
	wrapped, w := wrapBoundedResponseWriter(recorder, func() {}, nil)
	flusher, ok := wrapped.(http.Flusher)
	if !ok {
		t.Fatal("recorder wrapper lost Flusher")
	}
	w.flushWindow = time.Hour
	for i := 0; i < immediateSSEFlushes; i++ {
		_, _ = w.Write([]byte("x"))
		flusher.Flush()
	}
	if w.flushes != immediateSSEFlushes {
		t.Fatalf("immediate flushes=%d", w.flushes)
	}
	_, _ = w.Write([]byte("deferred"))
	flusher.Flush()
	if w.flushes != immediateSSEFlushes {
		t.Fatalf("flush was not coalesced: %d", w.flushes)
	}
	w.finish()
	if w.flushes != immediateSSEFlushes+1 {
		t.Fatalf("finish did not flush deferred data: %d", w.flushes)
	}
}

func TestResponseWritePolicyDefaultsToImmediateSSEFlush(t *testing.T) {
	t.Setenv("KIRO_SSE_FLUSH_WINDOW_MS", "")
	policy := responseWritePolicyFromEnv()
	if policy.flushWindow != 0 {
		t.Fatalf("default flush window=%s, want immediate flush", policy.flushWindow)
	}

	recorder := httptest.NewRecorder()
	wrapped, bounded := wrapBoundedResponseWriterWithPolicy(recorder, func() {}, nil, policy)
	flusher := wrapped.(http.Flusher)
	const events = immediateSSEFlushes + 3
	for i := 0; i < events; i++ {
		_, _ = wrapped.Write([]byte("event"))
		flusher.Flush()
	}
	bounded.finish()
	if bounded.flushes != events {
		t.Fatalf("default immediate flushes=%d, want %d", bounded.flushes, events)
	}

	t.Setenv("KIRO_SSE_FLUSH_WINDOW_MS", "25")
	if got := responseWritePolicyFromEnv().flushWindow; got != 25*time.Millisecond {
		t.Fatalf("explicit flush window=%s, want 25ms", got)
	}
}

func TestBoundedResponseWriterForcesOnlyFirstVisibleFlush(t *testing.T) {
	tracker := &latencyTracker{}
	_, timing := withRequestTiming(context.Background(), tracker)
	recorder := httptest.NewRecorder()
	wrapped, w := wrapBoundedResponseWriterWithPolicy(recorder, func() {}, timing, responseWritePolicy{
		writeTimeout: time.Second,
		flushWindow:  time.Hour,
	})
	flusher := wrapped.(http.Flusher)
	beginExplicitVisibleResponseTracking(wrapped)

	callback := instrumentKiroCallbackTiming(timing, &KiroStreamCallback{OnText: func(string, bool) {}})
	callback.OnText("upstream text before downstream framing", false)
	for i := 0; i < immediateSSEFlushes; i++ {
		_, _ = wrapped.Write([]byte("metadata"))
		flusher.Flush()
	}
	if _, ok := tracker.Snapshot()["first_visible_token_flush"]; ok {
		t.Fatal("upstream callback or metadata flush was counted as visible output")
	}

	_, _ = wrapped.Write([]byte("queued metadata"))
	flusher.Flush()
	if w.flushes != immediateSSEFlushes {
		t.Fatalf("metadata flush was not coalesced: %d", w.flushes)
	}

	_, _ = wrapped.Write([]byte("first visible delta"))
	markVisibleResponseWritten(wrapped)
	flusher.Flush()
	if w.flushes != immediateSSEFlushes+1 {
		t.Fatalf("first visible delta was not flushed immediately: %d", w.flushes)
	}
	if snapshot := tracker.Snapshot()["first_visible_token_flush"]; snapshot.Samples != 1 {
		t.Fatalf("first visible flush samples=%d, want 1", snapshot.Samples)
	}

	_, _ = wrapped.Write([]byte("second visible delta"))
	markVisibleResponseWritten(wrapped)
	flusher.Flush()
	if w.flushes != immediateSSEFlushes+1 {
		t.Fatalf("later visible delta bypassed the flush window: %d", w.flushes)
	}
	w.finish()
	if w.flushes != immediateSSEFlushes+2 {
		t.Fatalf("finish did not flush coalesced visible data: %d", w.flushes)
	}
}

func TestBoundedResponseWriterFlushWindowHasDeadline(t *testing.T) {
	recorder := httptest.NewRecorder()
	wrapped, w := wrapBoundedResponseWriterWithPolicy(recorder, func() {}, nil, responseWritePolicy{
		writeTimeout: time.Second,
		flushWindow:  20 * time.Millisecond,
	})
	flusher := wrapped.(http.Flusher)
	for i := 0; i < immediateSSEFlushes; i++ {
		_, _ = w.Write([]byte("x"))
		flusher.Flush()
	}
	_, _ = w.Write([]byte("deferred"))
	flusher.Flush()

	deadline := time.Now().Add(time.Second)
	for {
		w.mu.Lock()
		flushes := w.flushes
		w.mu.Unlock()
		if flushes == immediateSSEFlushes+1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deferred flush did not run: %d", flushes)
		}
		time.Sleep(time.Millisecond)
	}
	w.finish()
}

func TestBoundedResponseWriterTracksVisibleWriteDuringInFlightFlush(t *testing.T) {
	tracker := &latencyTracker{}
	_, timing := withRequestTiming(context.Background(), tracker)
	underlying := newBlockingFlushResponseWriter()
	wrapped, w := wrapBoundedResponseWriterWithPolicy(underlying, func() {}, timing, responseWritePolicy{
		writeTimeout: time.Second,
		flushWindow:  time.Hour,
	})
	beginExplicitVisibleResponseTracking(wrapped)

	if _, err := wrapped.Write([]byte("first visible delta")); err != nil {
		t.Fatalf("write visible delta: %v", err)
	}
	flushDone := make(chan struct{})
	go func() {
		wrapped.(http.Flusher).Flush()
		close(flushDone)
	}()
	select {
	case <-underlying.started:
	case <-time.After(time.Second):
		t.Fatal("underlying flush did not start")
	}

	// This is the production ordering when a deferred timer wins the race
	// between Write returning and the response encoder marking the frame visible.
	markVisibleResponseWritten(wrapped)
	close(underlying.release)
	select {
	case <-flushDone:
	case <-time.After(time.Second):
		t.Fatal("underlying flush did not finish")
	}
	w.finish()

	if snapshot := tracker.Snapshot()["first_visible_token_flush"]; snapshot.Samples != 1 {
		t.Fatalf("first visible flush samples=%d, want 1", snapshot.Samples)
	}
}

func TestBoundedResponseWriterTracksVisibleWriteAfterFlushWinsRace(t *testing.T) {
	tracker := &latencyTracker{}
	_, timing := withRequestTiming(context.Background(), tracker)
	recorder := httptest.NewRecorder()
	wrapped, w := wrapBoundedResponseWriterWithPolicy(recorder, func() {}, timing, responseWritePolicy{
		writeTimeout: time.Second,
		flushWindow:  time.Hour,
	})
	beginExplicitVisibleResponseTracking(wrapped)

	if _, err := wrapped.Write([]byte("first visible delta")); err != nil {
		t.Fatalf("write visible delta: %v", err)
	}
	wrapped.(http.Flusher).Flush()
	markVisibleResponseWritten(wrapped)
	w.finish()

	if snapshot := tracker.Snapshot()["first_visible_token_flush"]; snapshot.Samples != 1 {
		t.Fatalf("first visible flush samples=%d, want 1", snapshot.Samples)
	}
}

type blockingFlushResponseWriter struct {
	header  http.Header
	started chan struct{}
	release chan struct{}
}

func newBlockingFlushResponseWriter() *blockingFlushResponseWriter {
	return &blockingFlushResponseWriter{
		header:  make(http.Header),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *blockingFlushResponseWriter) Header() http.Header { return w.header }
func (w *blockingFlushResponseWriter) WriteHeader(int)     {}
func (w *blockingFlushResponseWriter) Write(p []byte) (int, error) {
	return len(p), nil
}
func (w *blockingFlushResponseWriter) Flush() {
	close(w.started)
	<-w.release
}

type failingResponseWriter struct{}

func (failingResponseWriter) Header() http.Header { return make(http.Header) }
func (failingResponseWriter) WriteHeader(int)     {}
func (failingResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

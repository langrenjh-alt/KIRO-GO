package proxy

import (
	"context"
	"net/http"
	"sync"
	"time"
)

const (
	defaultDownstreamWriteTimeout = 15 * time.Second
	defaultSSEFlushWindow         = 10 * time.Millisecond
	immediateSSEFlushes           = 3
)

type boundedResponseWriter struct {
	http.ResponseWriter
	cancel        context.CancelFunc
	timing        *requestTiming
	writeTimeout  time.Duration
	flushWindow   time.Duration
	supportsFlush bool

	ioMu sync.Mutex

	mu               sync.Mutex
	lastFlush        time.Time
	lastFlushDone    time.Time
	flushes          int
	dirty            bool
	visibleDirty     bool
	visibleFlushed   bool
	flushInFlight    bool
	inFlightVisible  bool
	closed           bool
	flushTimer       *time.Timer
	flushGeneration  uint64
	writeGeneration  uint64
	inFlightWrite    uint64
	lastFlushedWrite uint64
}

type responseWritePolicy struct {
	writeTimeout time.Duration
	flushWindow  time.Duration
}

func responseWritePolicyFromEnv() responseWritePolicy {
	writeTimeoutMs := positiveEnvInt("KIRO_DOWNSTREAM_WRITE_TIMEOUT_MS", int(defaultDownstreamWriteTimeout/time.Millisecond))
	flushWindowMs := nonNegativeEnvInt("KIRO_SSE_FLUSH_WINDOW_MS", int(defaultSSEFlushWindow/time.Millisecond))
	return responseWritePolicy{
		writeTimeout: time.Duration(writeTimeoutMs) * time.Millisecond,
		flushWindow:  time.Duration(flushWindowMs) * time.Millisecond,
	}
}

func newBoundedResponseWriter(w http.ResponseWriter, cancel context.CancelFunc, timing *requestTiming) *boundedResponseWriter {
	return newBoundedResponseWriterWithPolicy(w, cancel, timing, responseWritePolicyFromEnv())
}

func newBoundedResponseWriterWithPolicy(w http.ResponseWriter, cancel context.CancelFunc, timing *requestTiming, policy responseWritePolicy) *boundedResponseWriter {
	return &boundedResponseWriter{
		ResponseWriter: w,
		cancel:         cancel,
		timing:         timing,
		writeTimeout:   policy.writeTimeout,
		flushWindow:    policy.flushWindow,
		supportsFlush:  responseWriterCanFlush(w),
	}
}

type boundedFlushingResponseWriter struct {
	*boundedResponseWriter
}

func (w *boundedFlushingResponseWriter) Flush() {
	w.flush(false)
}

func wrapBoundedResponseWriter(w http.ResponseWriter, cancel context.CancelFunc, timing *requestTiming) (http.ResponseWriter, *boundedResponseWriter) {
	return wrapBoundedResponseWriterWithPolicy(w, cancel, timing, responseWritePolicyFromEnv())
}

func wrapBoundedResponseWriterWithPolicy(w http.ResponseWriter, cancel context.CancelFunc, timing *requestTiming, policy responseWritePolicy) (http.ResponseWriter, *boundedResponseWriter) {
	bounded := newBoundedResponseWriterWithPolicy(w, cancel, timing, policy)
	if bounded.supportsFlush {
		return &boundedFlushingResponseWriter{boundedResponseWriter: bounded}, bounded
	}
	return bounded, bounded
}

func responseWriterCanFlush(w http.ResponseWriter) bool {
	for w != nil {
		if _, ok := w.(http.Flusher); ok {
			return true
		}
		if _, ok := w.(interface{ FlushError() error }); ok {
			return true
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = unwrapper.Unwrap()
	}
	return false
}

func (w *boundedResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func beginExplicitVisibleResponseTracking(w http.ResponseWriter) {
	for w != nil {
		if tracker, ok := w.(interface{ beginExplicitVisibleTracking() }); ok {
			tracker.beginExplicitVisibleTracking()
			return
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = unwrapper.Unwrap()
	}
}

func markVisibleResponseWritten(w http.ResponseWriter) {
	for w != nil {
		if marker, ok := w.(interface{ markVisibleResponseWritten() }); ok {
			marker.markVisibleResponseWritten()
			return
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = unwrapper.Unwrap()
	}
}

func (w *boundedResponseWriter) beginExplicitVisibleTracking() {
	if w != nil && w.timing != nil {
		w.timing.beginExplicitVisibleTracking()
	}
}

func (w *boundedResponseWriter) markVisibleResponseWritten() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.closed || w.visibleFlushed {
		w.mu.Unlock()
		return
	}
	timing := w.timing
	if timing != nil {
		timing.markVisibleCandidate()
	}
	if !w.supportsFlush {
		w.visibleFlushed = true
		w.mu.Unlock()
		if timing != nil {
			timing.markVisibleFlushed(time.Now())
		}
		return
	}
	if w.dirty {
		w.visibleDirty = true
	} else if w.flushInFlight && w.inFlightWrite == w.writeGeneration {
		// The visible bytes are already in the underlying writer and belong to
		// the Flush currently in progress. Preserve that fact until Flush
		// completes so the TTFT sample cannot be lost in this narrow race.
		w.inFlightVisible = true
	} else if w.lastFlushedWrite == w.writeGeneration && !w.lastFlushDone.IsZero() {
		// A complete Flush can also win the entire race before the encoder calls
		// this marker. Since Write set dirty first, observing it cleared here
		// proves that the completed Flush included these visible bytes.
		w.visibleFlushed = true
		flushedAt := w.lastFlushDone
		w.mu.Unlock()
		if timing != nil {
			timing.markVisibleFlushed(flushedAt)
		}
		return
	}
	w.mu.Unlock()
}

func (w *boundedResponseWriter) setWriteDeadline() {
	if w == nil || w.ResponseWriter == nil || w.writeTimeout <= 0 {
		return
	}
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.writeTimeout))
}

func (w *boundedResponseWriter) Write(p []byte) (int, error) {
	w.ioMu.Lock()
	w.setWriteDeadline()
	n, err := w.ResponseWriter.Write(p)
	w.mu.Lock()
	if n > 0 {
		w.dirty = true
		w.writeGeneration++
	}
	w.mu.Unlock()
	w.ioMu.Unlock()
	if err != nil && w.cancel != nil {
		w.cancel()
	}
	if err == nil && n > 0 && !w.supportsFlush && w.timing != nil {
		w.timing.markVisibleFlushed(time.Now())
	}
	return n, err
}

func (w *boundedResponseWriter) flush(force bool) {
	if w == nil || w.ResponseWriter == nil {
		return
	}
	now := time.Now()
	w.mu.Lock()
	if w.closed || !w.dirty {
		w.mu.Unlock()
		return
	}
	if !force && !w.visibleDirty && w.flushes >= immediateSSEFlushes && w.flushWindow > 0 && now.Sub(w.lastFlush) < w.flushWindow {
		if w.flushTimer == nil {
			delay := w.flushWindow - now.Sub(w.lastFlush)
			w.flushGeneration++
			generation := w.flushGeneration
			w.flushTimer = time.AfterFunc(delay, func() {
				w.flushDeferred(generation)
			})
		}
		w.mu.Unlock()
		return
	}
	timer := w.flushTimer
	w.flushTimer = nil
	w.flushGeneration++
	w.dirty = false
	visible := w.visibleDirty
	w.visibleDirty = false
	w.flushInFlight = true
	w.inFlightVisible = visible
	w.inFlightWrite = w.writeGeneration
	w.flushes++
	w.lastFlush = now
	w.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	w.flushUnderlying()
}

func (w *boundedResponseWriter) flushDeferred(generation uint64) {
	if w == nil || w.ResponseWriter == nil {
		return
	}
	now := time.Now()
	w.mu.Lock()
	if w.closed || generation != w.flushGeneration || !w.dirty {
		w.mu.Unlock()
		return
	}
	w.flushTimer = nil
	w.flushGeneration++
	w.dirty = false
	visible := w.visibleDirty
	w.visibleDirty = false
	w.flushInFlight = true
	w.inFlightVisible = visible
	w.inFlightWrite = w.writeGeneration
	w.flushes++
	w.lastFlush = now
	w.mu.Unlock()
	w.flushUnderlying()
}

func (w *boundedResponseWriter) flushUnderlying() {
	w.ioMu.Lock()
	w.setWriteDeadline()
	err := http.NewResponseController(w.ResponseWriter).Flush()
	w.ioMu.Unlock()
	flushedAt := time.Now()
	w.mu.Lock()
	visible := w.inFlightVisible
	flushedWrite := w.inFlightWrite
	w.flushInFlight = false
	w.inFlightVisible = false
	w.inFlightWrite = 0
	if err == nil && visible {
		w.visibleFlushed = true
	}
	if err == nil {
		w.lastFlushDone = flushedAt
		w.lastFlushedWrite = flushedWrite
	}
	w.mu.Unlock()
	if err != nil {
		if w.cancel != nil {
			w.cancel()
		}
		return
	}
	if visible && w.timing != nil {
		w.timing.markVisibleFlushed(flushedAt)
	}
}

func (w *boundedResponseWriter) finish() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	timer := w.flushTimer
	w.flushTimer = nil
	w.flushGeneration++
	shouldFlush := w.supportsFlush && w.dirty
	if shouldFlush {
		visible := w.visibleDirty
		w.dirty = false
		w.visibleDirty = false
		w.flushInFlight = true
		w.inFlightVisible = visible
		w.inFlightWrite = w.writeGeneration
		w.flushes++
		w.lastFlush = time.Now()
		w.mu.Unlock()
		if timer != nil {
			timer.Stop()
		}
		w.flushUnderlying()
	} else {
		w.mu.Unlock()
		if timer != nil {
			timer.Stop()
		}
	}
	w.ioMu.Lock()
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Time{})
	w.ioMu.Unlock()
}

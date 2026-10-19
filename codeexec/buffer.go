package codeexec

import "sync"

// boundedBuffer accepts pipe writes without ever retaining more than limit.
// The first overflow cancels execution; later writes are drained/discarded
// until the killed CLI and container are reaped.
type boundedBuffer struct {
	mu       sync.Mutex
	data     []byte
	limit    int
	exceeded bool
	onLimit  func()
}

func newBoundedBuffer(limit int, onLimit func()) *boundedBuffer {
	return &boundedBuffer{limit: limit, onLimit: onLimit}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	remaining := b.limit - len(b.data)
	keep := len(p)
	if keep > remaining {
		keep = remaining
	}
	b.data = append(b.data, p[:keep]...)
	trigger := len(p) > remaining && !b.exceeded
	if trigger {
		b.exceeded = true
	}
	callback := b.onLimit
	b.mu.Unlock()
	if trigger && callback != nil {
		callback()
	}
	return len(p), nil
}

func (b *boundedBuffer) snapshot() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data), b.exceeded
}

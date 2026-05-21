package runevents

import (
	"sync"
	"time"
)

// Throttle wraps fn so that calls fire at most every minInterval *or*
// every minCount invocations — whichever boundary is crossed first. The
// terminal call (when Flush is invoked) always fires, regardless of how
// recent the last emit was. nil fn yields a no-op throttle so producers
// can blindly construct one even when no subscriber is attached.
//
// Concurrent calls are serialised via the embedded mutex; the producer
// side typically lives in a single goroutine but the index collector
// fans in worker results, so cheap locking is preferable to a sharded
// approach.
type Throttle struct {
	fn          func(Progress)
	minInterval time.Duration
	minCount    int64

	mu        sync.Mutex
	count     int64
	lastEmit  time.Time
	lastEvent Progress
}

func NewThrottle(fn func(Progress), minInterval time.Duration, minCount int64) *Throttle {
	return &Throttle{fn: fn, minInterval: minInterval, minCount: minCount}
}

// Emit records an event. fn is invoked iff the throttle boundary has
// been crossed since the last emit; otherwise the event is buffered and
// will surface on the next boundary or via Flush.
func (t *Throttle) Emit(p Progress) {
	if t == nil || t.fn == nil {
		return
	}
	t.mu.Lock()
	t.count++
	t.lastEvent = p
	shouldEmit := t.count >= t.minCount ||
		t.lastEmit.IsZero() ||
		time.Since(t.lastEmit) >= t.minInterval
	if !shouldEmit {
		t.mu.Unlock()
		return
	}
	t.count = 0
	t.lastEmit = time.Now()
	fn := t.fn
	t.mu.Unlock()
	fn(p)
}

// Flush forces the buffered last event (if any) through fn, even if no
// boundary has been crossed. Producers call this on a terminal handoff
// so the final pre-completion progress isn't lost.
func (t *Throttle) Flush() {
	if t == nil || t.fn == nil {
		return
	}
	t.mu.Lock()
	if t.count == 0 {
		t.mu.Unlock()
		return
	}
	p := t.lastEvent
	t.count = 0
	t.lastEmit = time.Now()
	fn := t.fn
	t.mu.Unlock()
	fn(p)
}

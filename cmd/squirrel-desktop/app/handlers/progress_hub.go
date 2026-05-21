package handlers

import (
	"sync"

	"github.com/mbertschler/squirrel/runevents"
)

// progressHub is the in-memory fan-out between long-running indexer or
// sync goroutines (the producers) and any SSE subscribers (the
// consumers, normally one browser tab per run). Producers publish via
// Publish; subscribers attach via Subscribe and receive both the
// last-seen event on attach (so a late opener still gets context) and
// every subsequent one. Close terminates all current subscribers.
//
// Lookup is keyed by runs.id. The runID is not known at goroutine
// launch — the index/sync packages allocate it inside BeginRun — so
// the producer calls Open before launch (returning a per-run handle
// the goroutine adopts via OnRunID).
type progressHub struct {
	mu   sync.Mutex
	runs map[int64]*runFeed
}

func newProgressHub() *progressHub {
	return &progressHub{runs: make(map[int64]*runFeed)}
}

// runFeed is one run's slot in the hub. It holds the live subscriber
// channels and the last published event so a freshly-opened SSE
// connection can render an immediate datapoint instead of staring at
// an empty pane until the next throttle tick.
type runFeed struct {
	mu      sync.Mutex
	subs    []chan runevents.Progress
	last    runevents.Progress
	hasLast bool
	closed  bool
}

// Publish records p as the latest event for runID and forwards it to
// every active subscriber. Non-blocking on the per-sub channel: if the
// subscriber is slow we drop intermediate events rather than stalling
// the producer. The last-event cache means the dropped events aren't
// invisible — the subscriber will catch up on the next forwarded one
// and the cache covers re-attachment.
func (h *progressHub) Publish(runID int64, p runevents.Progress) {
	feed := h.getOrCreate(runID)
	feed.mu.Lock()
	if feed.closed {
		feed.mu.Unlock()
		return
	}
	feed.last = p
	feed.hasLast = true
	subs := feed.subs
	feed.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- p:
		default:
		}
	}
}

// Subscribe returns a channel that receives Progress events for runID
// and a cancel func that detaches and drains the channel. If the feed
// has a cached last event it is delivered synchronously before
// Subscribe returns (the buffered channel has capacity 1 so the
// delivery does not block). If the feed is already closed, the
// returned channel is closed too — the caller's range loop exits
// immediately.
func (h *progressHub) Subscribe(runID int64) (<-chan runevents.Progress, func()) {
	feed := h.getOrCreate(runID)
	ch := make(chan runevents.Progress, 8)
	feed.mu.Lock()
	if feed.closed {
		feed.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	feed.subs = append(feed.subs, ch)
	if feed.hasLast {
		select {
		case ch <- feed.last:
		default:
		}
	}
	feed.mu.Unlock()
	return ch, func() { h.unsubscribe(runID, ch) }
}

// Close marks the run terminal: every active subscriber's channel is
// closed (signalling end-of-stream to range loops in SSE handlers) and
// the slot is removed from the registry so a future Subscribe against
// the same id sees a fresh feed.
func (h *progressHub) Close(runID int64) {
	h.mu.Lock()
	feed, ok := h.runs[runID]
	delete(h.runs, runID)
	h.mu.Unlock()
	if !ok {
		return
	}
	feed.mu.Lock()
	feed.closed = true
	subs := feed.subs
	feed.subs = nil
	feed.mu.Unlock()
	for _, c := range subs {
		close(c)
	}
}

func (h *progressHub) getOrCreate(runID int64) *runFeed {
	h.mu.Lock()
	defer h.mu.Unlock()
	if feed, ok := h.runs[runID]; ok {
		return feed
	}
	feed := &runFeed{}
	h.runs[runID] = feed
	return feed
}

func (h *progressHub) unsubscribe(runID int64, ch chan runevents.Progress) {
	h.mu.Lock()
	feed, ok := h.runs[runID]
	h.mu.Unlock()
	if !ok {
		return
	}
	feed.mu.Lock()
	for i, c := range feed.subs {
		if c == ch {
			feed.subs = append(feed.subs[:i], feed.subs[i+1:]...)
			break
		}
	}
	feed.mu.Unlock()
}

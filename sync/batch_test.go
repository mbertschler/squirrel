package sync

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestBatches: a batch closes at batchMaxFiles items or before the item
// that would take it past batchMaxBytes; an item larger than that goes
// alone.
func TestBatches(t *testing.T) {
	size := func(n int64) int64 { return n }
	cases := []struct {
		name  string
		items []int64
		want  []int
	}{
		{"none", nil, nil},
		{"by count", make([]int64, batchMaxFiles+1), []int{batchMaxFiles, 1}},
		{"by bytes", []int64{batchMaxBytes / 2, batchMaxBytes / 2, 1}, []int{2, 1}},
		{"too large alone", []int64{1, batchMaxBytes + 1, 1}, []int{1, 1, 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []int
			for _, b := range batches(c.items, size) {
				got = append(got, len(b))
			}
			if len(got) != len(c.want) {
				t.Fatalf("batch sizes = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("batch sizes = %v, want %v", got, c.want)
				}
			}
		})
	}
}

// TestInFlightBoundsTheCallsAtOnce: inFlight never runs more than n calls
// at once, runs every call, and starts none once stop reports true.
func TestInFlightBoundsTheCallsAtOnce(t *testing.T) {
	var running, peak, calls atomic.Int64
	inFlight(context.Background(), 3, make([]int, 20), func() bool { return false }, func(int) {
		now := running.Add(1)
		for p := peak.Load(); now > p && !peak.CompareAndSwap(p, now); p = peak.Load() {
		}
		time.Sleep(time.Millisecond)
		running.Add(-1)
		calls.Add(1)
	})
	if calls.Load() != 20 || peak.Load() > 3 {
		t.Fatalf("calls = %d, peak = %d; want 20 calls, at most 3 at once", calls.Load(), peak.Load())
	}
	var started atomic.Int64
	inFlight(context.Background(), 1, make([]int, 5), func() bool { return started.Load() == 2 }, func(int) {
		started.Add(1)
	})
	if started.Load() != 2 {
		t.Fatalf("started %d calls, want 2 before stop", started.Load())
	}
}

package sync

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hangingReads is a transport whose files read their first bytes, then
// hang until release is closed.
type hangingReads struct {
	transport
	release chan struct{}
}

func (h hangingReads) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(io.MultiReader(strings.NewReader("first"), blockUntil(h.release))), nil
}

type blockUntil chan struct{}

func (b blockUntil) Read([]byte) (int, error) {
	<-b
	return 0, io.EOF
}

// TestStallBoundsEveryRead: a file whose reads stop making progress fails
// with errTransportStalled once the allowance runs out, keeps failing,
// and counts as a call given up on until the read finally returns.
func TestStallBoundsEveryRead(t *testing.T) {
	release := make(chan struct{})
	stalled := new(atomic.Int64)
	s := stallTransport{transport: hangingReads{release: release}, timeout: 50 * time.Millisecond, stalled: stalled}
	rc, err := s.Get(context.Background(), "f")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	buf := make([]byte, 16)
	if n, err := rc.Read(buf); err != nil || string(buf[:n]) != "first" {
		t.Fatalf("first Read = %q, %v; want the bytes that were there", buf[:n], err)
	}
	if _, err := rc.Read(buf); !errors.Is(err, errTransportStalled) {
		t.Fatalf("hanging Read = %v, want errTransportStalled", err)
	}
	if _, err := rc.Read(buf); !errors.Is(err, errTransportStalled) {
		t.Fatalf("Read after a stall = %v, want the stall again", err)
	}
	if stalled.Load() != 1 {
		t.Fatalf("calls given up on = %d, want 1", stalled.Load())
	}
	_ = rc.Close()
	close(release)
	for deadline := time.Now().Add(5 * time.Second); stalled.Load() > 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if stalled.Load() != 0 {
		t.Fatal("the read that finally returned is still counted as outstanding")
	}
}

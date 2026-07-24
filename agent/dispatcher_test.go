package agent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestSyncDispatcherRunsDestinationsConcurrently proves the core F25 fix:
// two destinations run at the same time, so a slow offsite can never delay
// a LAN pair. Both runner invocations must be in flight before either is
// released.
func TestSyncDispatcherRunsDestinationsConcurrently(t *testing.T) {
	var active, maxActive atomic.Int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	run := func(ctx context.Context, vol *config.Volume, destName string) SyncRunReport {
		n := active.Add(1)
		for {
			old := maxActive.Load()
			if n <= old || maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		active.Add(-1)
		return SyncRunReport{Status: store.RunStatusSuccess}
	}
	d := newSyncDispatcher(run, discardLogger(), time.Now, 4)
	vol := &config.Volume{Name: "photos"}
	d.dispatch(context.Background(), vol, "cloudbox")
	d.dispatch(context.Background(), vol, "htpc")
	<-entered // both must be running before we release either
	<-entered
	close(release)
	d.wait()
	if got := maxActive.Load(); got != 2 {
		t.Fatalf("max concurrent syncs = %d; want 2 (different destinations run concurrently)", got)
	}
}

// TestSyncDispatcherSerializesSameDestination proves the concurrency unit
// is the destination: two volumes due to the same destination on one tick
// both run — the second is queued behind the first, never starved — while
// at most one is ever in flight against that destination.
func TestSyncDispatcherSerializesSameDestination(t *testing.T) {
	var active, maxActive atomic.Int32
	var photosRan, docsRan atomic.Bool
	entered := make(chan string, 2)
	release := make(chan struct{}) // one send per sync, in completion order
	run := func(ctx context.Context, vol *config.Volume, destName string) SyncRunReport {
		n := active.Add(1)
		for {
			old := maxActive.Load()
			if n <= old || maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		switch vol.Name {
		case "photos":
			photosRan.Store(true)
		case "docs":
			docsRan.Store(true)
		}
		entered <- vol.Name
		<-release
		active.Add(-1)
		return SyncRunReport{Status: store.RunStatusSuccess}
	}
	d := newSyncDispatcher(run, discardLogger(), time.Now, 4)
	d.dispatch(context.Background(), &config.Volume{Name: "photos"}, "cloudbox")
	d.dispatch(context.Background(), &config.Volume{Name: "docs"}, "cloudbox")

	first := <-entered // FIFO: photos was dispatched first
	if first != "photos" {
		t.Fatalf("first sync = %q; want photos (FIFO order)", first)
	}
	time.Sleep(20 * time.Millisecond) // give a wrongly-concurrent docs a chance to start
	if got := active.Load(); got != 1 {
		t.Fatalf("concurrent syncs to one destination = %d; want 1 (serialized)", got)
	}
	release <- struct{}{} // let photos finish; the worker then dequeues docs
	if second := <-entered; second != "docs" {
		t.Fatalf("second sync = %q; want docs (queued, not starved)", second)
	}
	release <- struct{}{} // let docs finish
	d.wait()

	if !photosRan.Load() || !docsRan.Load() {
		t.Fatalf("both volumes must run to the shared destination; photos=%v docs=%v",
			photosRan.Load(), docsRan.Load())
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("max concurrent syncs to one destination = %d; want 1", got)
	}
}

// TestSyncDispatcherBoundsOverallParallelism proves the semaphore ceiling:
// with a single slot, a second destination waits for the first to free the
// slot rather than running immediately.
func TestSyncDispatcherBoundsOverallParallelism(t *testing.T) {
	var bCalled atomic.Bool
	aEntered := make(chan struct{})
	release := make(chan struct{})
	run := func(ctx context.Context, vol *config.Volume, destName string) SyncRunReport {
		if destName == "a" {
			close(aEntered)
			<-release
		} else {
			bCalled.Store(true)
		}
		return SyncRunReport{Status: store.RunStatusSuccess}
	}
	d := newSyncDispatcher(run, discardLogger(), time.Now, 1)
	vol := &config.Volume{Name: "v"}
	d.dispatch(context.Background(), vol, "a")
	<-aEntered // a holds the only slot
	d.dispatch(context.Background(), vol, "b")
	time.Sleep(20 * time.Millisecond) // give b's worker time to reach the slot wait
	if bCalled.Load() {
		t.Fatal("b ran while the single parallelism slot was held by a")
	}
	close(release)
	d.wait()
	if !bCalled.Load() {
		t.Fatal("b never ran after the slot was freed")
	}
}

// TestSyncDispatcherNilRunnerSkips covers the index-only host: no runner
// wired, so a dispatch logs a skip and starts no worker.
func TestSyncDispatcherNilRunnerSkips(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, nil))
	d := newSyncDispatcher(nil, logger, time.Now, 4)
	d.dispatch(context.Background(), &config.Volume{Name: "v"}, "dest")
	d.wait()
	if !strings.Contains(buf.String(), "sync runner not configured") {
		t.Fatalf("expected a nil-runner skip log, got:\n%s", buf.String())
	}
}

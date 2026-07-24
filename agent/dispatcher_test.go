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

// TestSyncDispatcherOneInFlightPerDestination proves the concurrency unit
// is the destination: a second volume targeting a destination that already
// has a sync in flight is skipped with the per-pair "in-flight sync run"
// log, and its runner never fires while the first is running.
func TestSyncDispatcherOneInFlightPerDestination(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	run := func(ctx context.Context, vol *config.Volume, destName string) SyncRunReport {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return SyncRunReport{Status: store.RunStatusSuccess}
	}
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, nil))
	d := newSyncDispatcher(run, logger, time.Now, 4)

	d.dispatch(context.Background(), &config.Volume{Name: "photos"}, "cloudbox")
	<-entered // the first sync has claimed cloudbox and is running
	d.dispatch(context.Background(), &config.Volume{Name: "docs"}, "cloudbox")

	if got := calls.Load(); got != 1 {
		t.Fatalf("runner called %d times; want 1 (second dispatch to a busy destination must skip)", got)
	}
	log := buf.String()
	if !strings.Contains(log, `reason="in-flight sync run"`) || !strings.Contains(log, "volume=docs") {
		t.Fatalf("expected a per-pair in-flight skip for docs→cloudbox, got:\n%s", log)
	}
	close(release)
	d.wait()
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

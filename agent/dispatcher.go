package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// defaultMaxParallelSyncs bounds how many automatic syncs run at once
// across all destinations. Per-destination serialisation already caps
// parallelism at the number of configured destinations; this is the
// overall ceiling so a host with many destinations can't spawn one rclone
// per destination simultaneously and thrash its uplink and CPU. Four
// covers the reference household — a NAS pushing photos/docs/media to
// cloudbox + s3archive + kopia-mirror + htpc — without a LAN pair ever
// queueing behind a slow offsite, while still bounding a pathological
// config.
const defaultMaxParallelSyncs = 4

// syncDispatcher runs (volume, destination) syncs off the scheduler's tick
// goroutine so a slow or wedged destination can never delay the tick loop,
// index runs, peer syncs, or syncs to other destinations (#160, F25). The
// concurrency unit is the destination: at most one sync per destination is
// in flight at a time (inflight), and an overall semaphore (sem) bounds how
// many run concurrently across destinations.
//
// The pre-sync index ordering the scheduler guarantees is unaffected: the
// tick body still runs a volume's index to completion before it hands that
// volume's due syncs to the dispatcher.
type syncDispatcher struct {
	run    SyncRunner
	logger *slog.Logger
	now    func() time.Time

	sem chan struct{}

	mu       sync.Mutex
	inflight map[string]bool
	wg       sync.WaitGroup
}

func newSyncDispatcher(run SyncRunner, logger *slog.Logger, now func() time.Time, maxParallel int) *syncDispatcher {
	if maxParallel <= 0 {
		maxParallel = defaultMaxParallelSyncs
	}
	return &syncDispatcher{
		run:      run,
		logger:   logger,
		now:      now,
		sem:      make(chan struct{}, maxParallel),
		inflight: make(map[string]bool),
	}
}

// dispatch launches the sync for (vol, destName) on destName's worker,
// unless a sync to that destination is already in flight (a per-pair skip
// log, matching the CLI wording) or no runner is configured. It never
// blocks on the transfer: the rclone call runs in a goroutine bounded by
// the semaphore, so the caller — the tick loop — returns at once.
func (d *syncDispatcher) dispatch(ctx context.Context, vol *config.Volume, destName string) {
	if d.run == nil {
		d.logger.Info("scheduler.skipped",
			"kind", "sync", "volume", vol.Name, "destination", destName,
			"reason", "sync runner not configured")
		return
	}
	if !d.claim(destName) {
		d.logger.Info("scheduler.skipped",
			"kind", "sync", "volume", vol.Name, "destination", destName,
			"reason", "in-flight sync run")
		return
	}
	d.logger.Info("scheduler.kicked",
		"kind", "sync", "volume", vol.Name, "destination", destName)
	d.wg.Add(1)
	go d.runOne(ctx, vol, destName)
}

// runOne is the per-destination worker body: take an overall slot (or bail
// if the scheduler is shutting down), run the sync, and always release both
// the slot and the destination claim.
func (d *syncDispatcher) runOne(ctx context.Context, vol *config.Volume, destName string) {
	defer d.wg.Done()
	defer d.release(destName)
	select {
	case d.sem <- struct{}{}:
		defer func() { <-d.sem }()
	case <-ctx.Done():
		return
	}
	if ctx.Err() != nil {
		return
	}
	d.execute(ctx, vol, destName)
}

// execute invokes the sync runner and emits the finished/error logs,
// mirroring the scheduler's kicked/finished/error discipline. A sync that
// stalls is failed by the runner's transfer timeout (rclone is killed);
// its diagnosable error lands here and in the run row.
func (d *syncDispatcher) execute(ctx context.Context, vol *config.Volume, destName string) {
	start := d.now()
	rep := d.run(ctx, vol, destName)
	duration := d.now().Sub(start)
	status := rep.Status
	if status == "" {
		status = store.RunStatusFailed
	}
	d.logger.Info("scheduler.finished",
		"kind", "sync", "volume", vol.Name, "destination", destName,
		"run_id", rep.RunID, "status", status,
		"duration_ms", duration.Milliseconds(),
	)
	if rep.Err != nil {
		d.logger.Error("scheduler.error",
			"kind", "sync", "volume", vol.Name, "destination", destName,
			"run_id", rep.RunID, "err", rep.Err.Error())
	}
}

// claim marks destName in flight, returning false when a sync to it is
// already running. release clears the mark.
func (d *syncDispatcher) claim(destName string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inflight[destName] {
		return false
	}
	d.inflight[destName] = true
	return true
}

func (d *syncDispatcher) release(destName string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.inflight, destName)
}

// wait blocks until every dispatched sync has finished. The scheduler
// calls it during shutdown, after the run context is cancelled (which
// kills any in-flight rclone child), so it returns promptly.
func (d *syncDispatcher) wait() {
	d.wg.Wait()
}

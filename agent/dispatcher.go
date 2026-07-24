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
// index runs, peer syncs, or syncs to other destinations (#160, F25).
//
// The concurrency unit is the destination: each destination has a FIFO
// queue drained by a single worker, so at most one sync per destination is
// in flight while a second volume due to the same destination (photos and
// docs both push to cloudbox in the reference setup) waits its turn instead
// of being skipped and starved — issue #160 option 4b is per-destination
// queues, not skip-on-busy. An overall semaphore bounds how many
// destinations transfer at once.
//
// The pre-sync index ordering the scheduler guarantees is unaffected: the
// tick body still runs a volume's index to completion before it hands that
// volume's due syncs to the dispatcher.
type syncDispatcher struct {
	run    SyncRunner
	logger *slog.Logger
	now    func() time.Time

	sem chan struct{}

	mu    sync.Mutex
	dests map[string]*destQueue
	wg    sync.WaitGroup
}

// destQueue is one destination's serial pipeline: at most one sync active,
// the rest waiting in FIFO order. queued holds every volume name active or
// pending on this destination so a re-dispatch of a pair already in the
// pipeline — the scheduler re-evaluates a still-unfinished pair on every
// tick — is a quiet no-op rather than a duplicate enqueue.
type destQueue struct {
	pending []*config.Volume
	queued  map[string]bool
	active  bool
}

func newSyncDispatcher(run SyncRunner, logger *slog.Logger, now func() time.Time, maxParallel int) *syncDispatcher {
	if maxParallel <= 0 {
		maxParallel = defaultMaxParallelSyncs
	}
	return &syncDispatcher{
		run:    run,
		logger: logger,
		now:    now,
		sem:    make(chan struct{}, maxParallel),
		dests:  make(map[string]*destQueue),
	}
}

// dispatch enqueues the sync for (vol, destName) on destName's FIFO queue.
// An idle destination starts its worker immediately; a busy one takes the
// pair onto its queue to run when the current transfer finishes (deduped so
// re-evaluation on later ticks can't queue the same pair twice). It never
// blocks on the transfer — the tick loop returns at once, so a slow
// destination cannot delay any other.
func (d *syncDispatcher) dispatch(ctx context.Context, vol *config.Volume, destName string) {
	if d.run == nil {
		d.logger.Info("scheduler.skipped",
			"kind", "sync", "volume", vol.Name, "destination", destName,
			"reason", "sync runner not configured")
		return
	}
	d.mu.Lock()
	q := d.dests[destName]
	if q == nil {
		q = &destQueue{queued: make(map[string]bool)}
		d.dests[destName] = q
	}
	if q.queued[vol.Name] {
		d.mu.Unlock() // already active or queued for this destination
		return
	}
	q.queued[vol.Name] = true
	if q.active {
		q.pending = append(q.pending, vol)
		d.mu.Unlock()
		return
	}
	q.active = true
	d.mu.Unlock()
	d.wg.Add(1)
	go d.worker(ctx, destName, vol)
}

// worker drains destName's queue one sync at a time, so the destination
// never has two transfers in flight. It exits — freeing the destination —
// when the queue empties; on shutdown (ctx cancelled) it drains without
// running, leaving the unfinished pairs to be re-evaluated next start.
func (d *syncDispatcher) worker(ctx context.Context, destName string, vol *config.Volume) {
	defer d.wg.Done()
	for vol != nil {
		if ctx.Err() == nil {
			d.runOne(ctx, vol, destName)
		}
		vol = d.next(destName, vol.Name)
	}
}

// inFlight reports whether (destName, volName) is currently active or queued
// on destName's pipeline. The scheduler consults it alongside the runs table
// so a pair whose dispatch from an earlier tick is still working — but whose
// run row is not yet visible — is recognised as in flight and not dragged
// through a redundant pre-sync index every tick (#160). Safe for concurrent
// use; a nil-runner dispatcher (no dests) always reports false.
func (d *syncDispatcher) inFlight(destName, volName string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := d.dests[destName]
	if q == nil {
		return false
	}
	return q.queued[volName]
}

// next removes the just-finished volume from destName's pipeline and returns
// the next queued volume, or nil (dropping the now-idle destination) when
// none remain.
func (d *syncDispatcher) next(destName, done string) *config.Volume {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := d.dests[destName]
	if q == nil {
		return nil
	}
	delete(q.queued, done)
	if len(q.pending) == 0 {
		delete(d.dests, destName)
		return nil
	}
	next := q.pending[0]
	q.pending = q.pending[1:]
	return next
}

// runOne takes an overall parallelism slot (or bails on shutdown), runs the
// sync, and emits the kicked/finished/error logs. A sync that stalls is
// failed by the runner's transfer timeout (rclone is killed); its
// diagnosable error lands here and in the run row.
func (d *syncDispatcher) runOne(ctx context.Context, vol *config.Volume, destName string) {
	select {
	case d.sem <- struct{}{}:
		defer func() { <-d.sem }()
	case <-ctx.Done():
		return
	}
	if ctx.Err() != nil {
		return
	}
	d.logger.Info("scheduler.kicked",
		"kind", "sync", "volume", vol.Name, "destination", destName)
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

// wait blocks until every destination worker has drained. The scheduler
// calls it during shutdown, after the run context is cancelled (which kills
// any in-flight rclone child), so it returns promptly.
func (d *syncDispatcher) wait() {
	d.wg.Wait()
}

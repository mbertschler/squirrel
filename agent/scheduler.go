package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
)

// DefaultSchedulerTick is the cadence-evaluation period of the agent's
// scheduler. Half a minute is enough resolution for cadences in the
// minute-to-hour range (the realistic backup-frequency band) while
// keeping the runs-table chatter bounded.
const DefaultSchedulerTick = 30 * time.Second

// scheduler drives automatic index and sync runs on the cadences each
// volume declares (sync_every, index_every) in config. One scheduler
// per agent server; started by Serve when at least one volume opts in.
//
// One goroutine ticks at tickEvery and walks every configured volume
// per tick, kicking what's due based on `now - last_finished` from
// the runs table. There is no per-pair time.Ticker — a single tick
// keeps the "missed window = one run, not many" guarantee trivial.
type scheduler struct {
	store     *store.Store
	volumes   map[string]*config.Volume
	syncRun   SyncRunner
	logger    *slog.Logger
	locks     lockHolder
	tickEvery time.Duration
	now       func() time.Time
	// hooks runs the per-volume external-tool hooks (#84). May be nil in
	// tests that construct a bare scheduler; hookRunner methods tolerate a
	// nil receiver so the firing sites need no extra guard.
	hooks *hookRunner
}

// lockHolder is the subset of the peer-sync router the scheduler uses
// to coordinate with /v1/sync/* handlers (and the audit-scan loop) on
// per-volume access. The router implements it; tests pass a stub when
// no real peer-sync surface is wired up.
type lockHolder interface {
	acquireVolumeLock(volumeID int64) bool
	releaseVolumeLock(volumeID int64)
}

func newScheduler(srv *Server, tickEvery time.Duration, now func() time.Time) *scheduler {
	if tickEvery <= 0 {
		tickEvery = DefaultSchedulerTick
	}
	if now == nil {
		now = time.Now
	}
	return &scheduler{
		store:     srv.store,
		volumes:   srv.cfg.Volumes,
		syncRun:   srv.cfg.SyncRunner,
		logger:    srv.cfg.Logger,
		locks:     srv.router,
		tickEvery: tickEvery,
		now:       now,
		hooks:     newHookRunner(srv.store, srv.cfg.Logger),
	}
}

// anyScheduledVolume reports whether any configured volume has at
// least one cadence knob set. Serve skips starting the scheduler
// goroutine when this returns false so the agent has no idle goroutine
// when nothing is scheduled.
func (s *scheduler) anyScheduledVolume() bool {
	for _, v := range s.volumes {
		if v.SyncEvery > 0 || v.IndexEvery > 0 {
			return true
		}
	}
	return false
}

// run is the scheduler's main loop. Returns on ctx cancellation. One
// tick body never overlaps with the next: a slow tick simply drops the
// queued ticker fire (time.Ticker's 1-buffer channel discards values
// the receiver wasn't ready for).
func (s *scheduler) run(ctx context.Context) {
	t := time.NewTicker(s.tickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Drain in-flight hooks before returning so Serve's shutdown
			// wait doesn't race a hook goroutine writing its outcome.
			s.hooks.wait()
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// tick walks every configured volume once. Per-volume failures surface
// via the logger; we never abort the tick on one volume because the
// others still need their evaluations. Volume iteration is name-sorted
// so log output is deterministic across runs.
func (s *scheduler) tick(ctx context.Context) {
	for _, name := range sortedVolumeNames(s.volumes) {
		if ctx.Err() != nil {
			return
		}
		v := s.volumes[name]
		if v.SyncEvery == 0 && v.IndexEvery == 0 {
			continue
		}
		s.evaluateVolume(ctx, v)
	}
}

func sortedVolumeNames(m map[string]*config.Volume) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// evaluateVolume makes the scheduling decision for one volume on this
// tick. The sync-due branch runs first because a scheduled sync always
// indexes immediately before pushing; if that pre-sync index ran, it
// satisfies the standalone-index cadence too (both write kind='index'
// rows the same way) and the standalone branch naturally skips.
func (s *scheduler) evaluateVolume(ctx context.Context, vol *config.Volume) {
	v, err := s.resolveVolume(ctx, vol.Name, vol.Path)
	if err != nil {
		s.logger.Error("scheduler.error",
			"volume", vol.Name, "err", err.Error())
		return
	}
	syncFired := s.maybeRunSync(ctx, vol, v.ID)
	if !syncFired && vol.IndexEvery > 0 {
		s.maybeRunIndex(ctx, vol, v.ID, "standalone", vol.IndexEvery)
	}
}

// resolveVolume looks up (or creates) the volume row by name. Mirrors
// the audit-scan loop's behaviour so a fresh agent starts producing
// runs without requiring a prior `squirrel index`. A name match with a
// different path is a config drift we surface rather than silently
// proceed against the wrong row.
func (s *scheduler) resolveVolume(ctx context.Context, name, absPath string) (store.Volume, error) {
	v, err := s.store.GetVolumeByName(ctx, name)
	if err == nil {
		if v.Path != absPath {
			return store.Volume{}, fmt.Errorf("volume %q is at %q in the DB but config says %q", name, v.Path, absPath)
		}
		return v, nil
	}
	if !store.IsNotFound(err) {
		return store.Volume{}, fmt.Errorf("lookup volume: %w", err)
	}
	created, err := s.store.CreateVolume(ctx, name, absPath)
	if err != nil {
		return store.Volume{}, fmt.Errorf("create volume row: %w", err)
	}
	return created, nil
}

// maybeRunSync evaluates the sync_every cadence and, if any destination
// is due, runs a pre-sync index and then the per-destination syncs.
// Returns true when the scheduler took any sync-related action (kicked,
// skipped, or errored) so the caller can suppress a redundant
// standalone-index check.
//
// Invariant: a scheduled sync always runs an index immediately before
// pushing. If the pre-sync index fails or is skipped, no syncs run
// this tick — the next tick re-evaluates.
func (s *scheduler) maybeRunSync(ctx context.Context, vol *config.Volume, volumeID int64) bool {
	if vol.SyncEvery == 0 {
		return false
	}
	var dueDests []string
	for _, destName := range vol.SyncTo {
		due, err := s.syncDue(ctx, volumeID, destName, vol.SyncEvery)
		if err != nil {
			s.logger.Error("scheduler.error",
				"kind", "sync", "volume", vol.Name, "destination", destName,
				"err", err.Error())
			continue
		}
		if due {
			dueDests = append(dueDests, destName)
		}
	}
	if len(dueDests) == 0 {
		return false
	}
	if !s.maybeRunIndex(ctx, vol, volumeID, "pre-sync", 0) {
		return true
	}
	for _, destName := range dueDests {
		s.runSync(ctx, vol, volumeID, destName)
	}
	return true
}

// syncDue computes (now - last_finished) ≥ cadence for the (volume,
// destination) pair. A volume with no finished sync to this destination
// yet is always due.
func (s *scheduler) syncDue(ctx context.Context, volumeID int64, destination string, cadence time.Duration) (bool, error) {
	r, err := s.store.LatestFinishedRun(ctx, store.RunKindSync, volumeID, destination)
	if err != nil {
		if store.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("lookup last sync: %w", err)
	}
	return s.elapsedSince(r) >= cadence, nil
}

// indexDue mirrors syncDue for kind='index' runs. Pre-sync and
// standalone-index runs share kind='index', so a fresh pre-sync index
// naturally satisfies the standalone-index cadence without any
// special-casing in the decision logic.
func (s *scheduler) indexDue(ctx context.Context, volumeID int64, cadence time.Duration) (bool, error) {
	r, err := s.store.LatestFinishedRun(ctx, store.RunKindIndex, volumeID, "")
	if err != nil {
		if store.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("lookup last index: %w", err)
	}
	return s.elapsedSince(r) >= cadence, nil
}

// elapsedSince is time-since-the-run-completed, falling back to the
// start timestamp if ended_at_ns is unset (which the schema disallows
// for terminal-status rows but defensively handle so a future schema
// change doesn't silently break cadence math).
func (s *scheduler) elapsedSince(r store.Run) time.Duration {
	ts := r.StartedAtNs
	if r.EndedAtNs.Valid {
		ts = r.EndedAtNs.Int64
	}
	return s.now().Sub(time.Unix(0, ts))
}

// maybeRunIndex kicks an index pass when one is due. cadence==0 is the
// pre-sync path (always run regardless of last-index age); cadence>0 is
// the standalone-index path (run iff `now - last_finished >= cadence`).
// Returns true on success/partial, false on skipped or failure — the
// caller uses that to decide whether to proceed with downstream syncs.
func (s *scheduler) maybeRunIndex(ctx context.Context, vol *config.Volume, volumeID int64, reason string, cadence time.Duration) bool {
	if !s.indexGatePassed(ctx, vol, volumeID, cadence) {
		return false
	}
	if !s.locks.acquireVolumeLock(volumeID) {
		s.logger.Info("scheduler.skipped",
			"kind", "index", "volume", vol.Name,
			"reason", "volume busy")
		return false
	}
	defer s.locks.releaseVolumeLock(volumeID)
	return s.executeIndex(ctx, vol, volumeID, reason)
}

// indexGatePassed evaluates the cadence pre-check for an index kick.
// The in-flight check that used to live here was a TOCTOU pattern
// (HasRunningRun + insert) — it has moved into index.Index's atomic
// BeginIndexRunIfClear gate. A run that loses that gate surfaces here
// as an index.ErrAlreadyRunning from executeIndex and is logged as
// scheduler.skipped without going through indexGatePassed.
func (s *scheduler) indexGatePassed(ctx context.Context, vol *config.Volume, volumeID int64, cadence time.Duration) bool {
	if cadence > 0 {
		due, err := s.indexDue(ctx, volumeID, cadence)
		if err != nil {
			s.logger.Error("scheduler.error",
				"kind", "index", "volume", vol.Name, "err", err.Error())
			return false
		}
		if !due {
			return false
		}
	}
	return true
}

// executeIndex runs the index pass and emits kicked/finished/error
// logs. The caller owns the volume lock for the duration of the call.
// Returns true on success/partial, false on fatal failure.
//
// When index.Index fails before it can allocate a runs row (e.g. the
// root path stat fails in newIndexer), the report carries RunID == 0
// and no row was written. In that case we synthesise a failed
// kind='index' row ourselves so the cadence math has a watermark to
// compute against (otherwise the next tick re-kicks the same broken
// volume and the failure is invisible in `squirrel runs`).
func (s *scheduler) executeIndex(ctx context.Context, vol *config.Volume, volumeID int64, reason string) bool {
	s.logger.Info("scheduler.kicked",
		"kind", "index", "volume", vol.Name, "reason", reason)
	start := s.now()
	rep, err := index.Index(ctx, s.store, vol.Path, index.Options{
		Name:    vol.Name,
		Kind:    store.RunKindIndex,
		Shallow: true,
	})
	// A polite refusal from the new atomic gate (concurrent CLI invocation,
	// stale 'running' row, audit in flight) is not an error worth recording
	// as a failed run — the conflicting run is still progressing and the
	// next tick re-evaluates fresh.
	if inFlight, ok := errors.AsType[*index.ErrAlreadyRunning](err); ok {
		s.logger.Info("scheduler.skipped",
			"kind", "index", "volume", vol.Name,
			"reason", fmt.Sprintf("in-flight %s run", inFlight.Blocker.Kind),
			"blocker_run_id", inFlight.Blocker.ID,
			"blocker_kind", inFlight.Blocker.Kind)
		return false
	}
	duration := s.now().Sub(start)
	if err != nil && rep.RunID == 0 {
		rep.RunID = s.recordFailedIndex(ctx, vol, volumeID, err)
	}
	status, ok := indexRunStatus(rep, err)
	s.logger.Info("scheduler.finished",
		"kind", "index", "volume", vol.Name,
		"run_id", rep.RunID, "status", status,
		"duration_ms", duration.Milliseconds(),
	)
	if err != nil {
		s.logger.Error("scheduler.error",
			"kind", "index", "volume", vol.Name,
			"run_id", rep.RunID, "err", err.Error())
	}
	if ok {
		s.fireChangeHook(ctx, vol, volumeID, rep)
	}
	return ok
}

// fireChangeHook nudges the volume's external-tool hook after a successful
// index run (#85). Per the issue's lean default it fires on every
// completed run (success or partial) and passes SQUIRREL_CHANGED so the
// consumer can cheaply no-op when nothing moved — keying off "content
// settled" rather than off a sync to a remote, since a volume need not
// have any sync_to destination for the hook to be useful. The fire is
// best-effort and asynchronous, so it never affects the run that
// triggered it.
//
// "Changed" counts additions, modifications, and disappearances: a file
// going missing is as much a content change the backup should capture as
// a new or rewritten one.
func (s *scheduler) fireChangeHook(ctx context.Context, vol *config.Volume, volumeID int64, rep index.Report) {
	changed := rep.Added+rep.Modified+rep.Missing > 0
	s.hooks.fire(ctx, vol, volumeID, store.HookTriggerChange, rep.RunID, changed)
}

// recordFailedIndex inserts and immediately finishes a failed
// kind='index' run for volumeID, carrying runErr as the row's error
// message. Used when index.Index couldn't even allocate its own
// run row (typically a pre-walk stat failure). A failure of the
// BeginRun/FinishRun pair itself surfaces via scheduler.error so an
// operator notices that the watermark write also failed; the function
// still returns the (best-effort) run id so the kicked/finished log
// pair carries a non-zero correlation when possible.
func (s *scheduler) recordFailedIndex(ctx context.Context, vol *config.Volume, volumeID int64, runErr error) int64 {
	// Scheduler index runs are always shallow (see executeIndex); the
	// synthesised failed row carries the same flag so cadence math
	// and run history don't lie about the attempted mode.
	id, err := s.store.BeginIndexRun(ctx, store.RunKindIndex, volumeID, true)
	if err != nil {
		s.logger.Error("scheduler.error",
			"kind", "index", "volume", vol.Name,
			"err", fmt.Sprintf("record failed index: %v", err))
		return 0
	}
	if err := s.store.FinishRun(ctx, id, store.RunStatusFailed, runErr.Error(), 0); err != nil {
		s.logger.Error("scheduler.error",
			"kind", "index", "volume", vol.Name, "run_id", id,
			"err", fmt.Sprintf("finish failed index: %v", err))
	}
	return id
}

// indexRunStatus derives a terminal status string from an index report.
// ok=true means "the index ran cleanly enough to chain downstream
// syncs"; ok=false means the run was fatal and the scheduler should
// skip dependent work this tick.
func indexRunStatus(rep index.Report, err error) (string, bool) {
	if err != nil {
		return store.RunStatusFailed, false
	}
	if rep.Errors > 0 {
		return store.RunStatusPartial, true
	}
	return store.RunStatusSuccess, true
}

// runSync kicks one (volume, destination) sync via the configured
// SyncRunner. The pre-check against the runs table is the in-flight
// detection the issue specifies; the SyncRunner's downstream call
// (sync.RunPair) adds its own atomic BeginSyncRunIfClear gate, so a
// race that sneaks past the pre-check still produces a clean skip
// rather than a duplicate run.
//
// A nil SyncRunner surfaces as a clean skip rather than an error so
// an agent running pure index-only schedules can omit the sync wiring
// (and the rclone dependency that comes with it) without the
// scheduler logging at error level on each tick.
func (s *scheduler) runSync(ctx context.Context, vol *config.Volume, volumeID int64, destName string) {
	running, err := s.store.HasRunningRun(ctx, store.RunKindSync, volumeID, destName)
	if err != nil {
		s.logger.Error("scheduler.error",
			"kind", "sync", "volume", vol.Name, "destination", destName,
			"err", err.Error())
		return
	}
	if running {
		s.logger.Info("scheduler.skipped",
			"kind", "sync", "volume", vol.Name, "destination", destName,
			"reason", "in-flight sync run")
		return
	}
	if s.syncRun == nil {
		s.logger.Info("scheduler.skipped",
			"kind", "sync", "volume", vol.Name, "destination", destName,
			"reason", "sync runner not configured")
		return
	}
	s.logger.Info("scheduler.kicked",
		"kind", "sync", "volume", vol.Name, "destination", destName)
	start := s.now()
	rep := s.syncRun(ctx, vol, destName)
	duration := s.now().Sub(start)
	status := rep.Status
	if status == "" {
		status = store.RunStatusFailed
	}
	s.logger.Info("scheduler.finished",
		"kind", "sync", "volume", vol.Name, "destination", destName,
		"run_id", rep.RunID, "status", status,
		"duration_ms", duration.Milliseconds(),
	)
	if rep.Err != nil {
		s.logger.Error("scheduler.error",
			"kind", "sync", "volume", vol.Name, "destination", destName,
			"run_id", rep.RunID, "err", rep.Err.Error())
	}
}

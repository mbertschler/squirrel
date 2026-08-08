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
	store *store.Store
	// live is where the agent's configuration comes from, and loaded is the
	// snapshot volumes, verifyEvery, and pullEvery were last derived from.
	// refresh re-derives them once per reload (#204) rather than once per
	// tick: the derivation allocates two maps, and comparing the snapshot
	// pointer answers "has anything changed" exactly.
	live      *config.Live
	loaded    *config.Config
	volumes   map[string]*config.Volume
	dispatch  *syncDispatcher
	logger    *slog.Logger
	locks     lockHolder
	tickEvery time.Duration
	now       func() time.Time
	// hooks runs the per-volume external-tool hooks (#84). May be nil in
	// tests that construct a bare scheduler; hookRunner methods tolerate a
	// nil receiver so the firing sites need no extra guard.
	hooks *hookRunner
	// verifyRun and durabilityPull are the injected hooks for the two
	// non-volume cadences (F32/F33); nil disables the respective cadence.
	// verifyEvery maps a verifiable destination name to its resolved verify
	// cadence (per-destination verify_every or the [agent] default);
	// pullEvery maps a peer name to its pull_durability_every. lastVerify
	// and lastPull are in-memory watermarks (destination name, and
	// volume+peer key, → last attempt) — see runVerify for why they need
	// not persist.
	verifyRun      VerifyRunner
	durabilityPull DurabilityPuller
	verifyEvery    map[string]time.Duration
	pullEvery      map[string]time.Duration
	lastVerify     map[string]time.Time
	lastPull       map[string]time.Time
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
	s := &scheduler{
		store:          srv.store,
		live:           srv.live,
		dispatch:       newSyncDispatcher(srv.cfg.SyncRunner, srv.cfg.Logger, now, defaultMaxParallelSyncs),
		logger:         srv.cfg.Logger,
		locks:          srv.router,
		tickEvery:      tickEvery,
		now:            now,
		hooks:          newHookRunner(srv.store, srv.cfg.Logger),
		verifyRun:      srv.cfg.VerifyRunner,
		durabilityPull: srv.cfg.DurabilityPuller,
		lastVerify:     map[string]time.Time{},
		lastPull:       map[string]time.Time{},
	}
	s.refresh()
	return s
}

// refresh re-derives the scheduler's view of config when the agent has
// reloaded since the last look, and is a pointer comparison otherwise. The
// in-memory watermarks (lastVerify, lastPull) deliberately survive: a
// destination whose cadence changed keeps its place in the rotation rather
// than becoming instantly due, and one that left config simply stops being
// visited.
func (s *scheduler) refresh() {
	cur := s.live.Get()
	if cur == s.loaded {
		return
	}
	s.loaded = cur
	s.volumes = cur.Volumes
	s.verifyEvery = resolveVerifyCadences(cur.Destinations, cur.AgentVerifyEvery())
	s.pullEvery = resolvePullCadences(cur.Nodes)
}

// resolveVerifyCadences maps each verifiable (content-addressed or packed)
// destination to its effective verify cadence: the destination's own
// verify_every when set, otherwise the [agent] verify_every default. Only
// destinations with a positive resulting cadence are included, so a config
// with neither knob set yields an empty map and no verify activity.
func resolveVerifyCadences(dests map[string]*config.Destination, def time.Duration) map[string]time.Duration {
	out := make(map[string]time.Duration)
	for name, d := range dests {
		if d.Layout != config.LayoutContentAddressed && d.Layout != config.LayoutPacked {
			continue
		}
		eff := d.VerifyEvery
		if eff == 0 {
			eff = def
		}
		if eff > 0 {
			out[name] = eff
		}
	}
	return out
}

// resolvePullCadences maps each peer node declaring pull_durability_every to
// that cadence. Nodes without the knob are omitted, so a config with no pull
// cadence yields an empty map and no durability-pull activity.
func resolvePullCadences(nodes map[string]*config.Node) map[string]time.Duration {
	out := make(map[string]time.Duration)
	for name, n := range nodes {
		if n.PullDurabilityEvery > 0 {
			out[name] = n.PullDurabilityEvery
		}
	}
	return out
}

// anyScheduledVolume reports whether any configured volume has at
// least one cadence knob set.
func (s *scheduler) anyScheduledVolume() bool {
	for _, v := range s.volumes {
		if volumeHasCadence(v) {
			return true
		}
	}
	return false
}

// anyScheduledWork reports whether the scheduler has anything to do at all:
// a volume cadence, a destination verify cadence (F32), or a peer durability
// cadence (F33). Serve skips starting the scheduler goroutine when this
// returns false so the agent has no idle goroutine when nothing is
// scheduled. The verify/durability cadences are decisive on their own — a
// receive-only node (the reference htpc) declares no volume cadence yet must
// still run the scheduler purely to keep its offload-gate evidence fresh.
func (s *scheduler) anyScheduledWork() bool {
	return s.anyScheduledVolume() || len(s.verifyEvery) > 0 || len(s.pullEvery) > 0
}

// ScheduledWorkInConfig answers anyScheduledWork's question straight from a
// config, before any Server exists: does this config give the schedulers
// anything to do — a drift scan, a volume cadence, a destination verify
// cadence (F32), or a peer durability-pull cadence (F33)?
//
// The CLI consults it to refuse a listener-less agent that would idle
// silently (F35). It reads the same cadences through the same resolvers as
// the scheduler's own gate, so the two cannot drift: a receive-only machine
// whose only work is a pull_durability_every has real work and must start,
// even though it declares no volume cadence at all.
func ScheduledWorkInConfig(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	if cfg.Agent != nil && cfg.Agent.ScanInterval > 0 {
		return true
	}
	for _, v := range cfg.Volumes {
		if volumeHasCadence(v) {
			return true
		}
	}
	return len(resolveVerifyCadences(cfg.Destinations, cfg.AgentVerifyEvery())) > 0 ||
		len(resolvePullCadences(cfg.Nodes)) > 0
}

// volumeHasCadence reports whether a volume opts into any scheduler-driven
// cadence: a sync, a standalone index, or an interval hook. The interval
// hook counts on its own — a volume can want periodic verification of its
// external backup without any squirrel index/sync schedule.
func volumeHasCadence(v *config.Volume) bool {
	if v.SyncEvery > 0 || v.IndexEvery > 0 {
		return true
	}
	return v.Hook != nil && v.Hook.Interval > 0
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
			// Drain in-flight hooks and per-destination sync workers before
			// returning so Serve's shutdown wait doesn't race a goroutine
			// writing its outcome. ctx is already cancelled, so any rclone
			// child has been killed and the workers return promptly.
			s.hooks.wait()
			s.dispatch.wait()
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
//
// The tick boundary is where a reloaded config takes effect: cadences are
// re-derived here and then held for the whole tick, so a swap can never
// leave one phase evaluating the new volumes against the old destinations.
func (s *scheduler) tick(ctx context.Context) {
	s.refresh()
	for _, name := range sortedVolumeNames(s.volumes) {
		if ctx.Err() != nil {
			return
		}
		v := s.volumes[name]
		if !volumeHasCadence(v) {
			continue
		}
		s.evaluateVolume(ctx, v)
	}
	// The verify and durability-pull cadences key on destinations and peer
	// nodes rather than volumes, so they run after the per-volume loop as
	// their own phases. Both are read-only / metadata-only and take no
	// volume lock: verify touches only the remote-object bookkeeping, and a
	// durability pull only merges peer-supplied vectors — neither races the
	// per-volume index/sync/audit work the loop above coordinates.
	s.evaluateVerify(ctx)
	s.evaluateDurabilityPulls(ctx)
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
//
// The interval hook is evaluated last, after any index/sync this tick.
// That ordering means a tick that re-indexed the source (verifying HDD1)
// also fires the external check (verifying HDD2) right after — verify the
// source, then verify the backup — without coupling the two cadences:
// each runs on its own clock and the interval hook fires regardless of
// whether an index ran.
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
	s.maybeFireIntervalHook(ctx, vol, v.ID)
}

// maybeFireIntervalHook fires the volume's hook on its interval cadence
// (#86), independent of any change. It is the time-driven counterpart to
// the on-change fire in executeIndex: verification is orthogonal to change
// (bitrot hits static data), so it must run on a clock. The fire reuses
// the foundation's exec/env/best-effort/don't-stack/timeout path with the
// trigger set to interval; SQUIRREL_CHANGED is always false here (no run
// observed anything) and there is no triggering run.
func (s *scheduler) maybeFireIntervalHook(ctx context.Context, vol *config.Volume, volumeID int64) {
	if vol.Hook == nil || vol.Hook.Interval <= 0 {
		return
	}
	due, err := s.intervalHookDue(ctx, volumeID, vol.Hook.Interval)
	if err != nil {
		s.logger.Error("scheduler.error",
			"kind", "hook", "volume", vol.Name, "err", err.Error())
		return
	}
	if !due {
		return
	}
	s.hooks.fire(ctx, vol, volumeID, store.HookTriggerInterval, 0, false)
}

// intervalHookDue reports whether `now - last_interval_hook >= cadence`
// for the volume. A volume with no interval hook run yet is always due.
// The last run's end timestamp anchors the cadence (falling back to its
// start, mirroring elapsedSince); a still-running hook is handled by the
// don't-stack guard inside fire, not here.
func (s *scheduler) intervalHookDue(ctx context.Context, volumeID int64, cadence time.Duration) (bool, error) {
	r, err := s.store.LatestHookRun(ctx, volumeID, store.HookTriggerInterval)
	if err != nil {
		if store.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("lookup last interval hook: %w", err)
	}
	ts := r.StartedAtNs
	if r.EndedAtNs.Valid {
		ts = r.EndedAtNs.Int64
	}
	return s.now().Sub(time.Unix(0, ts)) >= cadence, nil
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

// maybeRunSync evaluates the sync_every cadence and, for the destinations
// that are due and not already syncing, runs a pre-sync index and then hands
// each pair to the per-destination dispatcher. Returns true when the
// scheduler took any sync-related action (kicked, skipped, or errored) so the
// caller can suppress a redundant standalone-index check.
//
// Invariant: a scheduled sync always runs an index immediately before
// pushing. If the pre-sync index fails or is skipped, no syncs run this tick
// — the next tick re-evaluates.
//
// A due destination whose (volume, destination) sync from an earlier tick is
// still in flight is dropped before the pre-sync index runs. Dispatch is
// asynchronous (#160), so without this the tick loop would re-index the source
// on every 30s tick for the entire duration of a slow multi-hour push —
// burning I/O and flooding the never-pruned runs audit trail with a
// kind='index' row per tick. When every due destination is already syncing,
// the pre-sync index is skipped entirely: there is no new push for it to
// precede, and the in-flight pushes were each already preceded by their own.
func (s *scheduler) maybeRunSync(ctx context.Context, vol *config.Volume, volumeID int64) bool {
	if vol.SyncEvery == 0 {
		return false
	}
	dueDests := s.dueSyncDests(ctx, vol, volumeID)
	if len(dueDests) == 0 {
		return false
	}
	dispatchable := s.dispatchableSyncDests(ctx, vol, volumeID, dueDests)
	if len(dispatchable) == 0 {
		// Every due destination is already syncing (each skip logged per
		// pair). Report the sync action so the standalone-index branch stays
		// suppressed — otherwise its own re-index would reintroduce the exact
		// per-tick flood this guard exists to prevent.
		return true
	}
	if !s.maybeRunIndex(ctx, vol, volumeID, "pre-sync", 0) {
		return true
	}
	for _, destName := range dispatchable {
		s.dispatch.dispatch(ctx, vol, destName)
	}
	return true
}

// dueSyncDests returns the destinations on the volume's sync_to whose
// sync_every cadence has elapsed. A per-destination lookup error is logged and
// that destination is dropped from this tick's evaluation (the next tick
// re-evaluates), never aborting the others.
func (s *scheduler) dueSyncDests(ctx context.Context, vol *config.Volume, volumeID int64) []string {
	var due []string
	for _, destName := range vol.SyncTo {
		ok, err := s.syncDue(ctx, volumeID, destName, vol.SyncEvery)
		if err != nil {
			s.logger.Error("scheduler.error",
				"kind", "sync", "volume", vol.Name, "destination", destName,
				"err", err.Error())
			continue
		}
		if ok {
			due = append(due, destName)
		}
	}
	return due
}

// dispatchableSyncDests filters dueDests to the pairs the dispatcher can start
// now, dropping any whose sync is already in flight and logging the same
// per-pair scheduler.skipped the serial scheduler emitted — so an operator
// still sees why a due destination isn't moving, it just no longer drags a
// redundant pre-sync index along with it.
func (s *scheduler) dispatchableSyncDests(ctx context.Context, vol *config.Volume, volumeID int64, dueDests []string) []string {
	var dispatchable []string
	for _, destName := range dueDests {
		if s.syncInFlight(ctx, vol, volumeID, destName) {
			s.logger.Info("scheduler.skipped",
				"kind", "sync", "volume", vol.Name, "destination", destName,
				"reason", "in-flight sync run")
			continue
		}
		dispatchable = append(dispatchable, destName)
	}
	return dispatchable
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

// syncInFlight reports whether a sync of this exact (volume, destination) pair
// is already active — a stale row, a concurrent CLI `squirrel sync`, or (the
// common case) a dispatch from an earlier tick still working. It consults two
// sources so neither a not-yet-visible run row nor a just-drained queue slot
// can fool it: the dispatcher's in-memory queue (set the instant a pair is
// enqueued, before its run row exists) and the runs table (authoritative once
// the row is written, and the signal that also catches a stale row or a
// concurrent CLI sync). The downstream dispatch and sync.RunPair's atomic
// BeginSyncRunIfClear gate remain the final guard against a duplicate run, so
// this check only needs to be good enough to suppress a redundant pre-sync
// index. A store error is treated as in-flight so a transient DB hiccup skips
// this tick rather than racing a duplicate push.
func (s *scheduler) syncInFlight(ctx context.Context, vol *config.Volume, volumeID int64, destName string) bool {
	if s.dispatch.inFlight(destName, vol.Name) {
		return true
	}
	running, err := s.store.HasRunningRun(ctx, store.RunKindSync, volumeID, destName)
	if err != nil {
		s.logger.Error("scheduler.error",
			"kind", "sync", "volume", vol.Name, "destination", destName,
			"err", err.Error())
		return true
	}
	return running
}

// evaluateVerify walks every destination with a resolved verify cadence
// (F32) and kicks its verify pass when due, in name order for deterministic
// logs. A nil verifyRun (no rclone wired, or no verifiable destination)
// disables the phase. The pass is the same one `squirrel verify` runs and
// records its own kind='audit' run; the agent never bootstraps or writes to
// the destination, so the marker/init boundary is respected.
func (s *scheduler) evaluateVerify(ctx context.Context) {
	if s.verifyRun == nil {
		return
	}
	for _, destName := range sortedCadenceNames(s.verifyEvery) {
		if ctx.Err() != nil {
			return
		}
		if !s.verifyDue(destName) {
			continue
		}
		s.runVerify(ctx, destName)
	}
}

// verifyDue reports whether `now - last_verify >= cadence` for the
// destination. A destination not yet verified this process lifetime is due.
func (s *scheduler) verifyDue(destName string) bool {
	last, ok := s.lastVerify[destName]
	if !ok {
		return true
	}
	return s.now().Sub(last) >= s.verifyEvery[destName]
}

// runVerify kicks one destination's verify pass and emits the
// kicked/finished/error log triple. The in-memory watermark is advanced up
// front so a failed or empty pass consumes the cadence window exactly like
// the volume cadences (no special retry — the next window re-evaluates). It
// need not persist: verify is read-only and idempotent, so re-running once
// after an agent restart is harmless and, for evidence freshness, desirable.
func (s *scheduler) runVerify(ctx context.Context, destName string) {
	s.lastVerify[destName] = s.now()
	s.logger.Info("scheduler.kicked", "kind", "verify", "destination", destName)
	start := s.now()
	rep := s.verifyRun(ctx, destName)
	duration := s.now().Sub(start)
	status := rep.Status
	if status == "" {
		// No recorded objects or packs: nothing to re-check and no audit
		// run written (run_id stays 0). Reported distinctly so the log
		// doesn't imply a verification actually ran.
		status = "no-op"
	}
	s.logger.Info("scheduler.finished",
		"kind", "verify", "destination", destName,
		"run_id", rep.RunID, "status", status,
		"duration_ms", duration.Milliseconds())
	if rep.Err != nil {
		s.logger.Error("scheduler.error",
			"kind", "verify", "destination", destName,
			"run_id", rep.RunID, "err", rep.Err.Error())
	}
}

// evaluateDurabilityPulls walks every peer with a pull_durability_every
// cadence (F33) and, for each locally-configured volume with a stake in the
// peer's evidence, kicks a durability pull when due. A nil durabilityPull
// disables the phase. Peers and volumes are visited in name order so the log
// is deterministic.
func (s *scheduler) evaluateDurabilityPulls(ctx context.Context) {
	if s.durabilityPull == nil {
		return
	}
	for _, peer := range sortedCadenceNames(s.pullEvery) {
		for _, volName := range sortedVolumeNames(s.volumes) {
			if ctx.Err() != nil {
				return
			}
			vol := s.volumes[volName]
			if !volumeHasDurabilityStake(vol) {
				continue
			}
			if !s.pullDue(volName, peer) {
				continue
			}
			s.runDurabilityPull(ctx, vol, peer)
		}
	}
}

// volumeHasDurabilityStake reports whether a durability pull could advance
// anything for this volume. The merge keeps only components for destinations
// the volume references (offload_requires ∪ sync_to — see
// sync.acceptedDestinations), so a volume naming neither has nothing to gain
// and is skipped. This is what lets the reference htpc pull its media
// volume's relayed s3archive evidence while skipping the photos volume it
// merely plays back.
func volumeHasDurabilityStake(v *config.Volume) bool {
	return len(v.OffloadRequires) > 0 || len(v.SyncTo) > 0
}

// pullDue reports whether `now - last_pull >= cadence` for the (volume,
// peer) pair. A pair not yet pulled this process lifetime is due.
func (s *scheduler) pullDue(volume, peer string) bool {
	last, ok := s.lastPull[pullKey(volume, peer)]
	if !ok {
		return true
	}
	return s.now().Sub(last) >= s.pullEvery[peer]
}

// runDurabilityPull kicks one (volume, peer) durability pull and emits the
// kicked/finished/error log triple. Watermark advanced up front, same policy
// and rationale as runVerify. The injected puller never rewinds a watermark
// — the agent does not escalate.
func (s *scheduler) runDurabilityPull(ctx context.Context, vol *config.Volume, peer string) {
	s.lastPull[pullKey(vol.Name, peer)] = s.now()
	s.logger.Info("scheduler.kicked", "kind", "pull-durability", "volume", vol.Name, "peer", peer)
	start := s.now()
	rep := s.durabilityPull(ctx, vol, peer)
	duration := s.now().Sub(start)
	status := rep.Status
	if status == "" {
		status = store.RunStatusFailed
	}
	s.logger.Info("scheduler.finished",
		"kind", "pull-durability", "volume", vol.Name, "peer", peer,
		"run_id", rep.RunID, "status", status,
		"fetched", rep.Fetched, "applied", rep.Applied, "dropped", rep.Dropped,
		"rewinds", rep.Rewinds,
		"duration_ms", duration.Milliseconds())
	if rep.Err != nil {
		s.logger.Error("scheduler.error",
			"kind", "pull-durability", "volume", vol.Name, "peer", peer,
			"run_id", rep.RunID, "err", rep.Err.Error())
	}
}

// pullKey is the composite watermark key for a (volume, peer) durability
// pull. The NUL separator can't appear in a validated volume or node name,
// so distinct pairs never collide.
func pullKey(volume, peer string) string {
	return volume + "\x00" + peer
}

// sortedCadenceNames returns the keys of a name→cadence map in sorted order,
// so the verify and durability phases visit their subjects deterministically.
func sortedCadenceNames(m map[string]time.Duration) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/mbertschler/squirrel/agent"
	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/sync"
)

// schedulerTools owns the state the agent's scheduled work needs that is
// derived from config but lives outside the agent process's own memory: the
// located rclone binary and the squirrel-managed rclone.conf that names
// every destination's remote.
//
// It exists because the agent reloads config (#204, F9). A destination
// added to the file while the agent runs is only reachable if the
// rclone.conf grew a section for it, so the agent hands each config it is
// about to adopt to rebuild first; a rebuild that fails abandons the reload
// and the agent keeps running the configuration it has.
//
// The pointer is atomic because a scheduled sync may be reading it while a
// reload replaces it. The rclone.conf file itself is rewritten in place,
// which an in-flight transfer does not notice: rclone parses its config
// once at process start.
type schedulerTools struct {
	// out receives the rclone version preflight's advisories.
	out io.Writer
	rcl atomic.Pointer[sync.Rclone]
}

// rclone returns the wrapper scheduled work should use, or nil when the
// configuration in force needs none.
func (t *schedulerTools) rclone() *sync.Rclone { return t.rcl.Load() }

// rebuild re-derives the tools from cfg. It locates rclone only when the
// schedule needs it — at least one volume declares a sync_every cadence
// with a destination on sync_to, or at least one verifiable destination
// carries a verify cadence (F32, which reads provider checksums through
// rclone). Pure index-only schedules (or a receive-only node whose only
// cadence is a durability pull) skip the lookup, so a host without rclone
// installed can still run the agent for its peer-sync surface, its index
// cadences, or its durability pulls.
//
// The version preflight mirrors what scheduled syncs will invoke: they run
// with the default sync.Options{} (Shallow=false), so `--hash blake3`
// requires rclone ≥ MinRcloneVersion unless every configured target is a
// crypt destination, which forces shallow. Failing here means the operator
// gets a clear startup error — or, on a reload, a latch naming the reason —
// rather than a midnight pager when the first scheduled sync fires and
// rclone rejects the flag.
func (t *schedulerTools) rebuild(ctx context.Context, cfg *config.Config) error {
	needsSync := anyVolumeNeedsScheduledSync(cfg)
	needsVerify := anyDestinationNeedsScheduledVerify(cfg)
	if !needsSync && !needsVerify {
		t.rcl.Store(nil)
		return nil
	}
	rcl, err := sync.Find()
	if err != nil {
		return fmt.Errorf("scheduler needs rclone for scheduled syncs/verifies: %w", err)
	}
	// Bound every automatic transfer by the no-progress guard so a wedged
	// endpoint fails its own run instead of hanging forever (#160, F25).
	// Foreground `squirrel sync` leaves this unset — a human can interrupt.
	rcl.StallTimeout = sync.DefaultStallTimeout
	// The version preflight (`--hash blake3`) is a sync concern; a
	// verify-only schedule reads provider checksums and doesn't need it.
	if needsSync {
		pairs, err := sync.PairsFor(cfg, "", "")
		if err != nil {
			return fmt.Errorf("scheduler rclone preflight: %w", err)
		}
		if err := sync.EnsureMinVersion(ctx, rcl, t.out, sync.ShallowForPairs(pairs, false)); err != nil {
			return fmt.Errorf("scheduler rclone preflight: %w", err)
		}
	}
	if _, err := rcl.WriteRcloneConfig(rcloneConfigPathFor(cfg), cfg.Destinations); err != nil {
		return fmt.Errorf("write rclone config: %w", err)
	}
	t.rcl.Store(rcl)
	return nil
}

func anyVolumeNeedsScheduledSync(cfg *config.Config) bool {
	for _, v := range cfg.Volumes {
		if v.SyncEvery > 0 && len(v.SyncTo) > 0 {
			return true
		}
	}
	return false
}

// anyDestinationNeedsScheduledVerify reports whether any verifiable
// destination has an effective verify cadence — its own verify_every, or the
// [agent] verify_every default. Mirrors the scheduler's own resolution so
// the rclone/runner wiring lines up with what the scheduler will actually
// fire.
func anyDestinationNeedsScheduledVerify(cfg *config.Config) bool {
	agentDefault := cfg.AgentVerifyEvery() > 0
	for _, d := range cfg.Destinations {
		if d.Layout != config.LayoutContentAddressed && d.Layout != config.LayoutPacked {
			continue
		}
		if d.VerifyEvery > 0 || agentDefault {
			return true
		}
	}
	return false
}

func anyNodeNeedsScheduledPull(cfg *config.Config) bool {
	for _, n := range cfg.Nodes {
		if n.PullDurabilityEvery > 0 {
			return true
		}
	}
	return false
}

// buildSchedulerSyncRunner returns the closure the agent's cadence
// scheduler invokes when it kicks a (volume, destination) sync. It resolves
// the destination/node by name against the configuration in force at the
// moment of the kick — the same config the CLI's `squirrel sync` would use
// — and delegates to sync.RunPair so the two surfaces share one code path.
//
// Reading the config per kick rather than capturing it is what makes a
// destination added while the agent runs actually syncable: the scheduler
// hands over a name, and this resolves it against whatever the agent is
// running now.
func buildSchedulerSyncRunner(live *config.Live, s *store.Store, tools *schedulerTools) agent.SyncRunner {
	return func(ctx context.Context, vol *config.Volume, destName string) agent.SyncRunReport {
		cfg := live.Get()
		rcl := tools.rclone()
		if rcl == nil {
			return agent.SyncRunReport{Err: errors.New("scheduled sync needs rclone, which the configuration in force did not call for")}
		}
		pair, err := schedulerPairFor(cfg, vol, destName)
		if err != nil {
			return agent.SyncRunReport{Err: err}
		}
		// Per-kick because the kopia lookup belongs to the kicks that
		// target a kopia destination: a host whose schedule never
		// touches one runs fine without the binary installed.
		syncTools, err := sync.ToolsFor(cfg, []sync.Pair{pair}, rcl)
		if err != nil {
			return agent.SyncRunReport{Err: err}
		}
		// Snapshot-on-sync fires on each node's scheduled syncs too (#75):
		// this is the operating cadence the catalog churns on. Each kick is
		// a single pair, so a fresh Snapshotter per kick is the right unit.
		opts := sync.Options{}
		if cfg.Backups.Enabled {
			opts.Snapshot = sync.NewSnapshotter(s, rcl, snapshotConfig(cfg, s.Path()))
		}
		rep, runErr := sync.RunPair(ctx, s, syncTools, pair, opts)
		return agent.SyncRunReport{
			RunID:     rep.RunID,
			Status:    rep.Status,
			Err:       runErr,
			Conflicts: len(rep.NodeConflicts),
			Contested: len(rep.NodeContested),
		}
	}
}

func schedulerPairFor(cfg *config.Config, vol *config.Volume, destName string) (sync.Pair, error) {
	if d, ok := cfg.Destinations[destName]; ok {
		return sync.Pair{Volume: vol, Destination: d}, nil
	}
	if n, ok := cfg.Nodes[destName]; ok {
		return sync.Pair{Volume: vol, Node: n}, nil
	}
	return sync.Pair{}, fmt.Errorf("destination %q is not declared in config", destName)
}

// buildSchedulerVerifyRunner returns the closure the scheduler invokes to
// verify one destination (F32). It runs sync.VerifyRemote — the same pass as
// `squirrel verify <destination>`, recording its own kind='audit' run — so
// the two surfaces share one code path. The scheduler only calls it for
// destinations that carry a resolved verify cadence, so a config with none
// never reaches it.
func buildSchedulerVerifyRunner(live *config.Live, s *store.Store, tools *schedulerTools) agent.VerifyRunner {
	return func(ctx context.Context, destName string) agent.VerifyRunReport {
		rcl := tools.rclone()
		if rcl == nil {
			return agent.VerifyRunReport{Status: store.RunStatusFailed, Err: errors.New("scheduled verify needs rclone, which the configuration in force did not call for")}
		}
		dest, ok := live.Get().Destinations[destName]
		if !ok {
			return agent.VerifyRunReport{Status: store.RunStatusFailed, Err: fmt.Errorf("destination %q is not declared in config", destName)}
		}
		rep, err := sync.VerifyRemote(ctx, s, rcl, dest)
		return agent.VerifyRunReport{RunID: rep.RunID, Status: verifyRunStatus(rep, err), Err: err}
	}
}

// verifyRunStatus maps a VerifyRemote outcome to the status the scheduler
// logs, matching sync.recordVerifyOutcome. An empty string means "nothing
// recorded to verify" (no audit run written); the scheduler renders it as a
// no-op rather than a run.
func verifyRunStatus(rep sync.RemoteVerifyReport, err error) string {
	switch {
	case err != nil:
		return store.RunStatusFailed
	case rep.RunID == 0:
		return ""
	case !rep.Clean():
		return store.RunStatusPartial
	default:
		return store.RunStatusSuccess
	}
}

// buildSchedulerDurabilityPuller returns the closure the scheduler invokes to
// pull one peer's durability vectors for a volume (F33). It runs
// sync.PullDurability — the same metadata-only merge the CLI and the
// post-sync pull share — with allowRewind always false (the agent never
// escalates), and wraps it in a kind='audit' run so the refresh appears in
// `squirrel runs`.
func buildSchedulerDurabilityPuller(live *config.Live, s *store.Store) agent.DurabilityPuller {
	return func(ctx context.Context, vol *config.Volume, peerName string) agent.DurabilityPullReport {
		node, ok := live.Get().Nodes[peerName]
		if !ok {
			return agent.DurabilityPullReport{Status: store.RunStatusFailed, Err: fmt.Errorf("node %q is not declared in config", peerName)}
		}
		runID, err := s.BeginDurabilityPullRun(ctx)
		if err != nil {
			return agent.DurabilityPullReport{Status: store.RunStatusFailed, Err: err}
		}
		rep, pullErr := sync.PullDurability(ctx, s, vol, node, false)
		return finishDurabilityPullRun(ctx, s, runID, vol.Name, peerName, rep, pullErr)
	}
}

// finishDurabilityPullRun records the pull's 'pull-durability' audit note and
// finishes its run, then returns the scheduler-facing report. A refused
// rewind lands as 'partial' (surfaced, but never applied — the agent does not
// escalate); a transport/merge failure as 'failed'. The pull error and any
// bookkeeping errors (audit-note append, run finish) are joined so a failure
// to record or finish the run — which would otherwise strand a 'running' row
// — always reaches the scheduler's error log rather than hiding behind an
// already-set pull error.
func finishDurabilityPullRun(ctx context.Context, s *store.Store, runID int64, volume, peer string, rep sync.DurabilityPullReport, pullErr error) agent.DurabilityPullReport {
	status, errMsg := durabilityPullStatus(rep, pullErr)
	note := fmt.Sprintf("volume=%s peer=%s fetched=%d applied=%d dropped=%d rewinds=%d",
		volume, peer, rep.Fetched, rep.Applied, rep.Dropped, len(rep.Rewinds))
	auditErr := s.AppendRunAudit(ctx, store.RunAuditEntry{RunID: runID, Transition: store.TransitionPullDurability, Note: note})
	// Applied is both counts: a pull that merged no durability rows changed
	// nothing locally, so it folds out of the steady-state noise (#182).
	finErr := s.FinishRunChanged(ctx, runID, status, errMsg, int64(rep.Applied), int64(rep.Applied))
	return agent.DurabilityPullReport{
		RunID:   runID,
		Status:  status,
		Err:     errors.Join(pullErr, auditErr, finErr),
		Fetched: rep.Fetched, Applied: rep.Applied, Dropped: rep.Dropped, Rewinds: len(rep.Rewinds),
	}
}

// durabilityPullStatus maps a pull outcome to (run status, run error
// message). Refused rewinds are the designed safe behaviour, not a failure,
// but are surfaced as 'partial' so they don't hide behind a green 'success'.
func durabilityPullStatus(rep sync.DurabilityPullReport, pullErr error) (string, string) {
	switch {
	case pullErr != nil:
		return store.RunStatusFailed, pullErr.Error()
	case len(rep.Rewinds) > 0:
		return store.RunStatusPartial, fmt.Sprintf("%d component(s) refused as rewinds (not applied — the agent does not escalate)", len(rep.Rewinds))
	default:
		return store.RunStatusSuccess, ""
	}
}

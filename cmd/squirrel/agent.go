package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/agent"
	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/sync"
)

// agentVersion is the value reported by GET /v1/health. A real release
// would inject this via -ldflags at build time; the placeholder is
// adequate for the unreleased peer-sync work.
const agentVersion = "0.0.0-dev"

// newAgentCmd returns the `squirrel agent` cobra command. It starts the
// HTTP server declared by the `[agent]` config block and blocks until
// the cobra context (wired to SIGINT/SIGTERM in main) is cancelled.
func newAgentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "agent",
		Short: "Run the squirrel agent (HTTP server + scheduled audits + cadence-driven index/sync)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgent(cmd)
		},
	}
}

func runAgent(cmd *cobra.Command) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	if cfg.Agent == nil {
		return fmt.Errorf("no [agent] block in %s", cfg.Path)
	}
	s, err := openAgentStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	rcl, err := resolveSchedulerRclone(cmd, cfg)
	if err != nil {
		return err
	}
	srv, err := agent.New(agent.Config{
		Listen:           cfg.Agent.Listen,
		Token:            cfg.Agent.Token,
		PeerTokens:       cfg.Agent.PeerTokens,
		TLSCert:          cfg.Agent.TLSCert,
		TLSKey:           cfg.Agent.TLSKey,
		Version:          agentVersion,
		Volumes:          cfg.Volumes,
		Destinations:     cfg.Destinations,
		Nodes:            cfg.Nodes,
		VerifyEvery:      cfg.Agent.VerifyEvery,
		SyncRunner:       buildSchedulerSyncRunner(cfg, s, rcl),
		VerifyRunner:     buildSchedulerVerifyRunner(cfg, s, rcl),
		DurabilityPuller: buildSchedulerDurabilityPuller(cfg, s),
		ScanInterval:     cfg.Agent.ScanInterval,
		ScanStrategy:     cfg.Agent.ScanStrategy,
		ScanLogger:       cmd.ErrOrStderr(),
		Logger:           logger,
	}, s)
	if err != nil {
		return err
	}
	// Bind first so a port-in-use (or any other listen failure) surfaces
	// as a CLI error and never logs a misleading "agent listening" line.
	// We also log the listener's resolved Addr so `:0` (and other
	// kernel-assigned ports) shows the actual port, not the configured
	// placeholder.
	ln, err := net.Listen("tcp", cfg.Agent.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Agent.Listen, err)
	}
	logAgentStartup(logger, srv, ln.Addr().String())
	return srv.Serve(cmd.Context(), ln)
}

// openAgentStore extends the standard resolveDBPath precedence with the
// agent-specific override: --db > cfg.Agent.DB > cfg.DB > default. We
// can't reuse openStore directly because it doesn't know about the
// agent block. The orphan-volume warning is intentionally skipped here
// — the agent process is long-running and a single-shot stderr advisory
// at startup would either spam or get lost in journald output.
func openAgentStore(cmd *cobra.Command, cfg *config.Config) (*store.Store, error) {
	dbPath, err := resolveAgentDBPath(cmd, cfg)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}
	return store.OpenWithOptions(dbPath, store.OpenOptions{NodeName: cfg.NodeName})
}

func resolveAgentDBPath(cmd *cobra.Command, cfg *config.Config) (string, error) {
	flag, err := cmd.Flags().GetString("db")
	if err != nil {
		return "", err
	}
	if flag != "" {
		return flag, nil
	}
	if cfg.Agent != nil && cfg.Agent.DB != "" {
		return cfg.Agent.DB, nil
	}
	if cfg.DB != "" {
		return cfg.DB, nil
	}
	def, err := defaultDBPath()
	if err != nil {
		return "", fmt.Errorf("resolve default db path: %w", err)
	}
	if def == "" {
		return "", errors.New("no agent db path configured and no default available")
	}
	return def, nil
}

// resolveSchedulerRclone locates the rclone binary and writes the
// squirrel-managed rclone.conf when the schedule needs rclone: at least one
// volume declares a sync_every cadence with a destination on sync_to, or at
// least one verifiable destination carries a verify cadence (F32, which
// reads provider checksums through rclone). Pure index-only schedules (or a
// receive-only node whose only cadence is a durability pull) skip the lookup
// so a host without rclone installed can still run the agent for its
// peer-sync surface, its index cadences, or its durability pulls.
//
// The version preflight mirrors what scheduled syncs will invoke: they
// run with the default sync.Options{} (Shallow=false), so `--hash blake3`
// requires rclone ≥ MinRcloneVersion unless every configured target is a
// crypt destination, which forces shallow. Failing here means the
// operator gets a clear startup error rather than a midnight pager when
// the first scheduled sync fires and rclone rejects the flag.
func resolveSchedulerRclone(cmd *cobra.Command, cfg *config.Config) (*sync.Rclone, error) {
	needsSync := anyVolumeNeedsScheduledSync(cfg)
	needsVerify := anyDestinationNeedsScheduledVerify(cfg)
	if !needsSync && !needsVerify {
		return nil, nil
	}
	rcl, err := sync.Find()
	if err != nil {
		return nil, fmt.Errorf("scheduler needs rclone for scheduled syncs/verifies: %w", err)
	}
	// The version preflight (`--hash blake3`) is a sync concern; a
	// verify-only schedule reads provider checksums and doesn't need it.
	if needsSync {
		pairs, err := sync.PairsFor(cfg, "", "")
		if err != nil {
			return nil, fmt.Errorf("scheduler rclone preflight: %w", err)
		}
		if err := sync.EnsureMinVersion(cmd.Context(), rcl, cmd.ErrOrStderr(), sync.ShallowForPairs(pairs, false)); err != nil {
			return nil, fmt.Errorf("scheduler rclone preflight: %w", err)
		}
	}
	if _, err := rcl.WriteRcloneConfig(rcloneConfigPathFor(cfg), cfg.Destinations); err != nil {
		return nil, fmt.Errorf("write rclone config: %w", err)
	}
	return rcl, nil
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
	agentDefault := cfg.Agent != nil && cfg.Agent.VerifyEvery > 0
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
// scheduler invokes when it kicks a (volume, destination) sync. It
// resolves the destination/node by name against the same config the
// CLI's `squirrel sync` uses and delegates to sync.RunPair so the two
// surfaces share one code path. A nil rcl (no volume needs scheduled
// sync) returns a nil runner, which the scheduler interprets as
// "sync-kicking disabled".
func buildSchedulerSyncRunner(cfg *config.Config, s *store.Store, rcl *sync.Rclone) agent.SyncRunner {
	if rcl == nil {
		return nil
	}
	return func(ctx context.Context, vol *config.Volume, destName string) agent.SyncRunReport {
		pair, err := schedulerPairFor(cfg, vol, destName)
		if err != nil {
			return agent.SyncRunReport{Err: err}
		}
		// Per-kick because the kopia lookup belongs to the kicks that
		// target a kopia destination: a host whose schedule never
		// touches one runs fine without the binary installed.
		tools, err := sync.ToolsFor(cfg, []sync.Pair{pair}, rcl)
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
		rep, runErr := sync.RunPair(ctx, s, tools, pair, opts)
		return agent.SyncRunReport{RunID: rep.RunID, Status: rep.Status, Err: runErr}
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
// the two surfaces share one code path. A nil rcl (no destination needs a
// verify cadence) returns a nil runner, which the scheduler treats as
// "verify-kicking disabled".
func buildSchedulerVerifyRunner(cfg *config.Config, s *store.Store, rcl *sync.Rclone) agent.VerifyRunner {
	if rcl == nil || !anyDestinationNeedsScheduledVerify(cfg) {
		return nil
	}
	return func(ctx context.Context, destName string) agent.VerifyRunReport {
		dest, ok := cfg.Destinations[destName]
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
// `squirrel runs`. A config with no pull cadence returns a nil puller.
func buildSchedulerDurabilityPuller(cfg *config.Config, s *store.Store) agent.DurabilityPuller {
	if !anyNodeNeedsScheduledPull(cfg) {
		return nil
	}
	return func(ctx context.Context, vol *config.Volume, peerName string) agent.DurabilityPullReport {
		node, ok := cfg.Nodes[peerName]
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
// escalate); a transport/merge failure as 'failed'. Bookkeeping errors are
// folded into the report so the scheduler logs them without masking a pull
// error that already occurred.
func finishDurabilityPullRun(ctx context.Context, s *store.Store, runID int64, volume, peer string, rep sync.DurabilityPullReport, pullErr error) agent.DurabilityPullReport {
	status, errMsg := durabilityPullStatus(rep, pullErr)
	note := fmt.Sprintf("volume=%s peer=%s fetched=%d applied=%d dropped=%d rewinds=%d",
		volume, peer, rep.Fetched, rep.Applied, rep.Dropped, len(rep.Rewinds))
	out := agent.DurabilityPullReport{
		RunID: runID, Status: status, Err: pullErr,
		Fetched: rep.Fetched, Applied: rep.Applied, Dropped: rep.Dropped, Rewinds: len(rep.Rewinds),
	}
	if auditErr := s.AppendRunAudit(ctx, store.RunAuditEntry{RunID: runID, Transition: store.TransitionPullDurability, Note: note}); auditErr != nil && out.Err == nil {
		out.Err = auditErr
	}
	if finErr := s.FinishRun(ctx, runID, status, errMsg, int64(rep.Applied)); finErr != nil && out.Err == nil {
		out.Err = finErr
	}
	return out
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

// logAgentStartup emits a single structured startup line via slog so a
// systemd unit's journal (or a developer running in the foreground) can
// see what's listening where. addr is the resolved listener address (so
// `:0` reports the kernel-assigned port). We deliberately avoid
// per-request logging — HTTP middleware is out of scope here; the
// cadence scheduler shares this same logger handle to emit its own
// scheduler.kicked / scheduler.skipped / scheduler.finished events.
func logAgentStartup(logger *slog.Logger, srv *agent.Server, addr string) {
	scheme := "http"
	if srv.HasTLS() {
		scheme = "https"
	}
	logger.Info("agent listening",
		"addr", addr,
		"scheme", scheme,
		"version", agentVersion,
	)
}

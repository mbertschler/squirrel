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
)

// newAgentCmd returns the `squirrel agent` cobra command. It starts the
// HTTP server declared by the `[agent]` config block and blocks until
// the cobra context (wired to SIGINT/SIGTERM in main) is cancelled. The
// `cert` child is a one-shot bootstrap helper and does not start a server.
func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the squirrel agent (HTTP server + scheduled audits + cadence-driven index/sync)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgent(cmd)
		},
	}
	cmd.AddCommand(newAgentCertCmd())
	return cmd
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
	if err := reapOrphanedRunsAtStartup(cmd.Context(), s, logger); err != nil {
		return err
	}
	agentCfg, err := buildAgentConfig(cmd, cfg, s, logger)
	if err != nil {
		return err
	}
	srv, err := agent.New(agentCfg, s)
	if err != nil {
		return err
	}
	return serveAgent(cmd, cfg, srv, logger)
}

// buildAgentConfig assembles the agent's configuration from the loaded
// config file, and locates the tools its scheduled work needs. The two
// belong together because both sides of the reload path are wired here: the
// live holder the agent swaps into, and the rebuild hook that re-derives
// rclone's view of the destinations from whatever it swaps in (#204).
func buildAgentConfig(cmd *cobra.Command, cfg *config.Config, s *store.Store, logger *slog.Logger) (agent.Config, error) {
	live := config.NewLive(cfg)
	tools := &schedulerTools{out: cmd.ErrOrStderr()}
	if err := tools.rebuild(cmd.Context(), cfg); err != nil {
		return agent.Config{}, err
	}
	return agent.Config{
		Listen:              cfg.Agent.Listen,
		Token:               cfg.Agent.Token,
		PeerTokens:          cfg.Agent.PeerTokens,
		TLSCert:             cfg.Agent.TLSCert,
		TLSKey:              cfg.Agent.TLSKey,
		Version:             version,
		Live:                live,
		ConfigReloadPrepare: tools.rebuild,
		SyncRunner:          buildSchedulerSyncRunner(live, s, tools),
		VerifyRunner:        buildSchedulerVerifyRunner(live, s, tools),
		DurabilityPuller:    buildSchedulerDurabilityPuller(live, s),
		ScanInterval:        cfg.Agent.ScanInterval,
		ScanStrategy:        cfg.Agent.ScanStrategy,
		ScanLogger:          cmd.ErrOrStderr(),
		Logger:              logger,
		// The file this process parsed and the digest of its bytes: the
		// agent re-reads the same path on a cadence, adopts what it can, and
		// says which of the rest still wants a restart (F9).
		ConfigPath:   cfg.Path,
		ConfigDigest: cfg.Digest,
	}, nil
}

// serveAgent dispatches the built server to its run path: the listener-less
// scheduler-only run (F35) when [agent] listen is empty, or the HTTP server
// otherwise. Split from runAgent so the setup phase stays compact.
func serveAgent(cmd *cobra.Command, cfg *config.Config, srv *agent.Server, logger *slog.Logger) error {
	// Listener-less mode (F35): no `listen`, so run the schedulers without an
	// HTTP server. Refuse an agent that would do nothing at all — a
	// scheduler-only agent with no cadences and no scan is silent
	// degradation, not a valid config.
	if cfg.Agent.Listen == "" {
		if !agent.ScheduledWorkInConfig(cfg) {
			return fmt.Errorf("listener-less agent has nothing to run: set [agent] listen to receive peer syncs, or configure a cadence (a volume's sync_every, index_every, or hook.interval, a destination's verify_every, a node's pull_durability_every, or [agent] scan_interval or verify_every) in %s", cfg.Path)
		}
		logSchedulerOnlyStartup(logger)
		return srv.RunSchedulers(cmd.Context())
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

// reapOrphanedRunsAtStartup transitions any 'running' run left behind by a
// previously-killed agent to 'aborted' before the schedulers start (#157,
// F14). A freshly started agent has kicked nothing yet, so every 'running'
// row necessarily predates it; leaving them would block the run gates
// forever and render as a live, elapsed-ticking banner in the TUI hours
// later. The reap is loud — one info line naming the reaped ids — because
// automatic recovery must never be invisible (ux-principle 5).
func reapOrphanedRunsAtStartup(ctx context.Context, s *store.Store, logger *slog.Logger) error {
	ids, err := s.AbortRunningRuns(ctx, "reaped at agent startup: the agent that owned this run was killed mid-run")
	if err != nil {
		return fmt.Errorf("reap orphaned runs: %w", err)
	}
	if len(ids) > 0 {
		logger.Warn("reaped orphaned runs",
			"count", len(ids), "run_ids", ids, "status", store.RunStatusAborted)
	}
	return nil
}

// logSchedulerOnlyStartup emits the listener-less counterpart of the
// "agent listening" banner so a journal shows the agent came up in
// scheduler-only mode with no bound port.
func logSchedulerOnlyStartup(logger *slog.Logger) {
	logger.Info("agent scheduler running", "listener", "disabled", "version", version)
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
	attrs := []any{
		"addr", addr,
		"scheme", scheme,
		"version", version,
	}
	// Surfacing the fingerprint at startup (F1) lets an operator read the
	// pin peers must put in [nodes.X.tls] straight from the agent's log,
	// without a separate command. A read failure is non-fatal — the agent
	// still serves — so it is logged as a warning attribute, not an error.
	if srv.HasTLS() {
		if fp, err := srv.CertFingerprint(); err == nil {
			attrs = append(attrs, "cert_fingerprint", fp)
		} else {
			attrs = append(attrs, "cert_fingerprint_error", err.Error())
		}
	}
	logger.Info("agent listening", attrs...)
}

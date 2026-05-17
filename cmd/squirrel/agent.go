package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/agent"
	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
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
		Short: "Run the squirrel agent (HTTP server + scheduled audits)",
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

	srv, err := agent.New(agent.Config{
		Listen:       cfg.Agent.Listen,
		Token:        cfg.Agent.Token,
		TLSCert:      cfg.Agent.TLSCert,
		TLSKey:       cfg.Agent.TLSKey,
		Version:      agentVersion,
		Volumes:      cfg.Volumes,
		ScanInterval: cfg.Agent.ScanInterval,
		ScanStrategy: cfg.Agent.ScanStrategy,
		ScanLogger:   cmd.ErrOrStderr(),
	}, s)
	if err != nil {
		return err
	}
	printAgentBanner(cmd, srv)
	return srv.ListenAndServe(cmd.Context())
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

// printAgentBanner emits a single startup line to stdout so the user
// (or a systemd unit's journal) can see what's listening where. We
// deliberately avoid logging during request handling — that belongs to
// the http handler middleware once we have one in PR 3.
func printAgentBanner(cmd *cobra.Command, srv *agent.Server) {
	scheme := "http"
	if srv.HasTLS() {
		scheme = "https"
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"squirrel agent listening on %s://%s (version %s)\n",
		scheme, srv.Addr(), agentVersion)
}

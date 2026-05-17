package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/daemon"
	"github.com/mbertschler/squirrel/store"
)

// daemonVersion is the value reported by GET /v1/health. A real release
// would inject this via -ldflags at build time; the placeholder is
// adequate for the unreleased peer-sync work.
const daemonVersion = "0.0.0-dev"

// newDaemonCmd returns the `squirrel daemon` cobra command. It starts the
// HTTP server declared by the `[daemon]` config block and blocks until
// the cobra context (wired to SIGINT/SIGTERM in main) is cancelled.
func newDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run the squirrel daemon HTTP server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDaemon(cmd)
		},
	}
}

func runDaemon(cmd *cobra.Command) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	if cfg.Daemon == nil {
		return fmt.Errorf("no [daemon] block in %s", cfg.Path)
	}
	s, err := openDaemonStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	srv, err := daemon.New(daemon.Config{
		Listen:  cfg.Daemon.Listen,
		Token:   cfg.Daemon.Token,
		TLSCert: cfg.Daemon.TLSCert,
		TLSKey:  cfg.Daemon.TLSKey,
		Version: daemonVersion,
	}, s)
	if err != nil {
		return err
	}
	printDaemonBanner(cmd, srv)
	return srv.ListenAndServe(cmd.Context())
}

// openDaemonStore extends the standard resolveDBPath precedence with the
// daemon-specific override: --db > cfg.Daemon.DB > cfg.DB > default. We
// can't reuse openStore directly because it doesn't know about the
// daemon block. The orphan-volume warning is intentionally skipped here
// — the daemon process is long-running and a single-shot stderr advisory
// at startup would either spam or get lost in journald output.
func openDaemonStore(cmd *cobra.Command, cfg *config.Config) (*store.Store, error) {
	dbPath, err := resolveDaemonDBPath(cmd, cfg)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}
	return store.Open(dbPath)
}

func resolveDaemonDBPath(cmd *cobra.Command, cfg *config.Config) (string, error) {
	flag, err := cmd.Flags().GetString("db")
	if err != nil {
		return "", err
	}
	if flag != "" {
		return flag, nil
	}
	if cfg.Daemon != nil && cfg.Daemon.DB != "" {
		return cfg.Daemon.DB, nil
	}
	if cfg.DB != "" {
		return cfg.DB, nil
	}
	def, err := defaultDBPath()
	if err != nil {
		return "", fmt.Errorf("resolve default db path: %w", err)
	}
	if def == "" {
		return "", errors.New("no daemon db path configured and no default available")
	}
	return def, nil
}

// printDaemonBanner emits a single startup line to stdout so the user
// (or a systemd unit's journal) can see what's listening where. We
// deliberately avoid logging during request handling — that belongs to
// the http handler middleware once we have one in PR 3.
func printDaemonBanner(cmd *cobra.Command, srv *daemon.Server) {
	scheme := "http"
	if srv.HasTLS() {
		scheme = "https"
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"squirrel daemon listening on %s://%s (version %s)\n",
		scheme, srv.Addr(), daemonVersion)
}

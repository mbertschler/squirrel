package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// newRootCmd builds the top-level `squirrel` cobra command and attaches
// the subcommands. Each subcommand lives in its own file in this package.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "squirrel",
		Short:        "Local content-addressed file indexer",
		SilenceUsage: true,
	}

	// --db default is left empty: the resolved value comes from (in order)
	// the flag, the config file's db field, and finally defaultDBPath().
	// Leaving it empty here lets us tell "the user passed --db" apart from
	// "use the built-in default."
	root.PersistentFlags().String("db", "", "SQLite database path (overrides config and default)")

	defaultCfg, _ := config.DefaultPath()
	root.PersistentFlags().String("config", defaultCfg, "squirrel config file path")

	root.AddCommand(newIndexCmd())
	root.AddCommand(newQueryCmd())
	root.AddCommand(newRunsCmd())
	root.AddCommand(newVolumesCmd())
	root.AddCommand(newSyncCmd())
	return root
}

// defaultDBPath returns the built-in default location for the index
// database, $HOME/.squirrel/index.db, used when neither --db nor config.DB
// supplies one. The parent directory is created lazily by openStore.
func defaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".squirrel", "index.db"), nil
}

// requireConfig is the helper for subcommands that cannot function without
// a config (sync, restore, index). On the MissingError path it produces a
// user-facing error pointing at the resolved path, so users without a
// config file get told where to create one rather than a bare ENOENT.
func requireConfig(cmd *cobra.Command) (*config.Config, error) {
	path, err := cmd.Flags().GetString("config")
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, nil
	}
	if missing, ok := errors.AsType[*config.MissingError](err); ok {
		return nil, fmt.Errorf("no config at %s — create one (see README for an example)", missing.Path)
	}
	return nil, err
}

// tryLoadConfig returns the parsed Config or nil if the file does not
// exist. Real parse failures (syntactically invalid TOML, unknown fields)
// still propagate as errors. Subcommands that operate on the DB without
// needing config — query, runs, volumes — call this so the user can pick
// up config.DB if a config exists, without making the config mandatory.
func tryLoadConfig(cmd *cobra.Command) (*config.Config, error) {
	path, err := cmd.Flags().GetString("config")
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		if config.IsMissing(err) {
			return nil, nil
		}
		return nil, err
	}
	return cfg, nil
}

// openStore resolves the database path with --db > config.db > default
// precedence, ensures the parent directory exists, and opens the SQLite
// store. cfg may be nil for subcommands that don't load the config.
func openStore(cmd *cobra.Command, cfg *config.Config) (*store.Store, error) {
	dbPath, err := resolveDBPath(cmd, cfg)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}
	return store.Open(dbPath)
}

func resolveDBPath(cmd *cobra.Command, cfg *config.Config) (string, error) {
	if flag, err := cmd.Flags().GetString("db"); err != nil {
		return "", err
	} else if flag != "" {
		return flag, nil
	}
	if cfg != nil && cfg.DB != "" {
		return cfg.DB, nil
	}
	def, err := defaultDBPath()
	if err != nil {
		return "", fmt.Errorf("resolve default db path: %w", err)
	}
	return def, nil
}

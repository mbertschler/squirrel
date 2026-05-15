package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

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

	defaultDB, _ := defaultDBPath()
	root.PersistentFlags().String("db", defaultDB, "SQLite database path")

	root.AddCommand(newIndexCmd())
	root.AddCommand(newQueryCmd())
	return root
}

// defaultDBPath returns the default location for the index database,
// $HOME/.squirrel/index.db. The parent directory is created lazily by
// openStore the first time the DB is opened.
func defaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".squirrel", "index.db"), nil
}

// openStore reads --db from cmd's flags, ensures the parent directory exists,
// and opens the SQLite store. Subcommands call this from their RunE.
func openStore(cmd *cobra.Command) (*store.Store, error) {
	dbPath, err := cmd.Flags().GetString("db")
	if err != nil {
		return nil, err
	}
	if dbPath == "" {
		dbPath, err = defaultDBPath()
		if err != nil {
			return nil, fmt.Errorf("resolve default db path: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}
	return store.Open(dbPath)
}

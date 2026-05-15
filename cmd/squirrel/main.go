package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "squirrel",
		Short:         "Local content-addressed file indexer",
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	defaultDB, _ := defaultDBPath()
	root.PersistentFlags().String("db", defaultDB, "SQLite database path")

	root.AddCommand(newIndexCmd())
	root.AddCommand(newQueryCmd())
	return root
}

func defaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".squirrel", "index.db"), nil
}

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

func newIndexCmd() *cobra.Command {
	var (
		shallow bool
		dryRun  bool
		workers int
	)
	cmd := &cobra.Command{
		Use:   "index <path>",
		Short: "Walk a directory, hash regular files, and update the index",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore(cmd)
			if err != nil {
				return err
			}
			defer s.Close()

			rep, err := index.Index(cmd.Context(), s, args[0], index.Options{
				Shallow: shallow,
				DryRun:  dryRun,
				Workers: workers,
			})
			if err != nil {
				return err
			}

			prefix := ""
			if dryRun {
				prefix = "(dry-run) "
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%sadded=%d modified=%d unchanged=%d missing=%d errors=%d\n",
				prefix, rep.Added, rep.Modified, rep.Unchanged, rep.Missing, rep.Errors)
			if rep.Errors > 0 {
				return fmt.Errorf("encountered %d error(s) during walk", rep.Errors)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&shallow, "shallow", false, "skip rehash when (size, mtime) match the stored row")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without writing to the database")
	cmd.Flags().IntVar(&workers, "workers", 0, "number of hashing workers (0 = runtime.NumCPU())")
	return cmd
}

func newQueryCmd() *cobra.Command {
	var (
		duplicates bool
		missing    bool
	)
	cmd := &cobra.Command{
		Use:   "query [<hash-or-path>]",
		Short: "Look up the index by hash, path, or list duplicates/missing",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore(cmd)
			if err != nil {
				return err
			}
			defer s.Close()

			switch {
			case duplicates:
				if len(args) != 0 {
					return errors.New("--duplicates does not take a positional argument")
				}
				return queryDuplicates(cmd, s)
			case missing:
				if len(args) != 0 {
					return errors.New("--missing does not take a positional argument")
				}
				return queryMissing(cmd, s)
			default:
				if len(args) != 1 {
					return errors.New("query requires <hash>, <path>, --duplicates, or --missing")
				}
				return queryArg(cmd, s, args[0])
			}
		},
	}
	cmd.Flags().BoolVar(&duplicates, "duplicates", false, "list hashes that appear at more than one path")
	cmd.Flags().BoolVar(&missing, "missing", false, "list previously-indexed paths no longer on disk")
	return cmd
}

func queryArg(cmd *cobra.Command, s *store.Store, arg string) error {
	out := cmd.OutOrStdout()
	if isHashLike(arg) {
		digest, err := hex.DecodeString(arg)
		if err != nil {
			return fmt.Errorf("decode hash: %w", err)
		}
		rows, err := s.GetByBlake3(cmd.Context(), digest)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("no rows for blake3 %s", arg)
		}
		for _, r := range rows {
			fmt.Fprintf(out, "%s\t%s\t%d\n", r.Status, joinRootPath(r.Root, r.Path), r.SizeBytes)
		}
		return nil
	}

	absPath, err := filepath.Abs(arg)
	if err != nil {
		return err
	}
	row, err := s.GetByAbsolutePath(cmd.Context(), absPath)
	if err != nil {
		if store.IsNotFound(err) {
			return fmt.Errorf("no row for path %s (not under any indexed root)", absPath)
		}
		return err
	}
	fmt.Fprintf(out, "root:        %s\n", row.Root)
	fmt.Fprintf(out, "path:        %s\n", row.Path)
	fmt.Fprintf(out, "full:        %s\n", joinRootPath(row.Root, row.Path))
	fmt.Fprintf(out, "blake3:      %s\n", hex.EncodeToString(row.Blake3))
	fmt.Fprintf(out, "size_bytes:  %d\n", row.SizeBytes)
	fmt.Fprintf(out, "mtime_ns:    %d\n", row.MtimeNs)
	fmt.Fprintf(out, "status:      %s\n", row.Status)
	fmt.Fprintf(out, "last_seen:   %d\n", row.LastSeenAt)
	fmt.Fprintf(out, "indexed_at:  %d\n", row.IndexedAt)
	return nil
}

func queryDuplicates(cmd *cobra.Command, s *store.Store) error {
	rows, err := s.ListDuplicates(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	var lastHex string
	for _, r := range rows {
		h := hex.EncodeToString(r.Blake3)
		if h != lastHex {
			if lastHex != "" {
				fmt.Fprintln(out)
			}
			fmt.Fprintf(out, "%s\n", h)
			lastHex = h
		}
		fmt.Fprintf(out, "  %s\n", joinRootPath(r.Root, r.Path))
	}
	return nil
}

func queryMissing(cmd *cobra.Command, s *store.Store) error {
	rows, err := s.ListMissing(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	for _, r := range rows {
		fmt.Fprintf(out, "%s\t%s\n", hex.EncodeToString(r.Blake3), joinRootPath(r.Root, r.Path))
	}
	return nil
}

func joinRootPath(root, rel string) string {
	if rel == "" || rel == "." {
		return root
	}
	return filepath.Join(root, rel)
}

func isHashLike(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/index"
)

// newIndexCmd returns the `squirrel index <path>` cobra command. The command
// walks the given directory, hashes regular files with BLAKE3, and updates
// the SQLite index incrementally.
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
			return runIndex(cmd, args[0], index.Options{
				Shallow: shallow,
				DryRun:  dryRun,
				Workers: workers,
			})
		},
	}
	cmd.Flags().BoolVar(&shallow, "shallow", false, "skip rehash when (size, mtime) match the stored row")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without writing to the database")
	cmd.Flags().IntVar(&workers, "workers", 0, "number of hashing workers (0 = runtime.NumCPU())")
	return cmd
}

func runIndex(cmd *cobra.Command, path string, opts index.Options) error {
	s, err := openStore(cmd)
	if err != nil {
		return err
	}
	defer s.Close()

	rep, err := index.Index(cmd.Context(), s, path, opts)
	if err != nil {
		return err
	}

	for _, e := range rep.ErrorList {
		fmt.Fprintln(cmd.ErrOrStderr(), "error:", e)
	}
	prefix := ""
	if opts.DryRun {
		prefix = "(dry-run) "
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%sadded=%d modified=%d unchanged=%d missing=%d errors=%d\n",
		prefix, rep.Added, rep.Modified, rep.Unchanged, rep.Missing, rep.Errors)
	if rep.Errors > 0 {
		return fmt.Errorf("encountered %d error(s) during walk", rep.Errors)
	}
	return nil
}

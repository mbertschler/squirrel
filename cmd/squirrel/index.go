package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/index"
)

// newIndexCmd returns the `squirrel index <volume>` cobra command. The
// volume name is looked up in the config to resolve the absolute path
// that will be walked. Indexing by path is no longer supported — declare
// the volume in config first.
func newIndexCmd() *cobra.Command {
	var (
		shallow bool
		dryRun  bool
		workers int
	)
	cmd := &cobra.Command{
		Use:   "index <volume>",
		Short: "Walk a config-declared volume, hash regular files, and update the index",
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

func runIndex(cmd *cobra.Command, volumeName string, opts index.Options) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	vol, ok := cfg.Volumes[volumeName]
	if !ok {
		return fmt.Errorf("unknown volume %q (declare it in %s)", volumeName, cfg.Path)
	}
	opts.Name = vol.Name

	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	rep, err := index.Index(cmd.Context(), s, vol.Path, opts)
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

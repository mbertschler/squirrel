package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
)

// newIndexCmd returns the `squirrel index <volume>` cobra command. The
// volume name is looked up in the config to resolve the absolute path
// that will be walked. Indexing by path is no longer supported — declare
// the volume in config first.
func newIndexCmd() *cobra.Command {
	var (
		shallow  bool
		dryRun   bool
		workers  int
		progress bool
	)
	cmd := &cobra.Command{
		Use:   "index <volume>",
		Short: "Walk a config-declared volume, hash regular files, and update the index",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIndex(cmd, args[0], progress, index.Options{
				Shallow: shallow,
				DryRun:  dryRun,
				Workers: workers,
			})
		},
	}
	cmd.Flags().BoolVar(&shallow, "shallow", false, "skip rehash when (size, mtime) match the stored row")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without writing to the database")
	cmd.Flags().IntVar(&workers, "workers", 0, "number of hashing workers (0 = runtime.NumCPU())")
	cmd.Flags().BoolVarP(&progress, "progress", "P", false, "show live progress (auto-enabled on a terminal; use --progress=false to force off)")
	return cmd
}

func runIndex(cmd *cobra.Command, volumeName string, progress bool, opts index.Options) error {
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

	// Captured before the walk (so it reflects the prior run, not this
	// one): its presence tells a first index from a re-index, and its
	// timestamp feeds the catch-up note.
	priorNs, hadPrior := priorIndexRun(cmd, s, vol.Name)

	// Progress renders to stderr so a redirected stdout still captures only
	// the final summary line. The line is erased right after Index returns
	// (both paths) so any error list and the summary print on a clean line.
	var pp *progressPrinter
	if progressEnabled(cmd, progress) {
		pp = newProgressPrinter(cmd.ErrOrStderr())
		opts.Progress = pp.update
	}

	rep, err := index.Index(cmd.Context(), s, vol.Path, opts)
	if pp != nil {
		pp.clear()
	}
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
	// Naming the volume keeps back-to-back index runs legible instead of
	// three anonymous count lines (friction F11a).
	fmt.Fprintf(cmd.OutOrStdout(), "%s%s: added=%d modified=%d unchanged=%d missing=%d errors=%d\n",
		prefix, vol.Name, rep.Added, rep.Modified, rep.Unchanged, rep.Missing, rep.Errors)
	printIndexAdvisories(cmd, vol, rep, priorNs, hadPrior)
	if rep.Errors > 0 {
		return fmt.Errorf("encountered %d error(s) during walk", rep.Errors)
	}
	return nil
}

// printIndexAdvisories surfaces the two affirmative-feedback lines the
// friction walk asked for: an empty-path warning on a first index (F8)
// and a catch-up note after a burst of new files following a quiet gap
// (F24). Both are advisory — neither changes the exit code.
func printIndexAdvisories(cmd *cobra.Command, vol *config.Volume, rep index.Report, priorNs int64, hadPrior bool) {
	if !hadPrior && rep.Errors == 0 && !rep.SawFiles() {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: volume %q at %s: no files found — new volume, empty directory, or wrong mount?\n",
			vol.Name, vol.Path)
		return
	}
	if hadPrior && rep.SawFiles() {
		if note, ok := catchUpNote(rep.Added, time.Since(time.Unix(0, priorNs))); ok {
			fmt.Fprintln(cmd.OutOrStdout(), note)
		}
	}
}

// priorIndexRun returns the start time of the most recent successful index
// run of the volume, and whether one exists. Best-effort: any lookup miss
// (volume never indexed, store error) reports "no prior", which is the safe
// default for both advisories.
func priorIndexRun(cmd *cobra.Command, s *store.Store, volName string) (int64, bool) {
	v, err := s.GetVolumeByName(cmd.Context(), volName)
	if err != nil {
		return 0, false
	}
	run, err := s.LatestSuccessfulIndexRun(cmd.Context(), v.ID)
	if err != nil {
		return 0, false
	}
	return run.StartedAtNs, true
}

// Catch-up heuristic thresholds: a burst of new files after a long quiet
// gap is the moment squirrel earns the most trust (a trip's worth of
// photos landing in one tick), and it should not blend into routine churn.
// The bounds are deliberately conservative so ordinary cadence runs stay
// silent.
const (
	catchUpMinFiles = 25
	catchUpMinQuiet = 7 * 24 * time.Hour
)

// catchUpNote returns a one-line "N new files after D days" summary when a
// run added enough files after a long enough quiet gap, and whether it
// applies (friction F24).
func catchUpNote(added int, quiet time.Duration) (string, bool) {
	if added < catchUpMinFiles || quiet < catchUpMinQuiet {
		return "", false
	}
	return fmt.Sprintf("catch-up: %d new files after %d days of quiet", added, int(quiet.Hours())/24), true
}

package main

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/offload"
)

// newOffloadCmd returns the `squirrel offload <volume> [path...]` cobra
// command: delete the local bytes of files whose content is provably
// durable on every target the volume's offload_requires policy names.
// Paths are volume-relative files or directory prefixes; --older-than
// narrows by indexed mtime and combines with paths. At least one of the
// two selectors is required, and a volume without an offload_requires
// policy is refused outright.
func newOffloadCmd() *cobra.Command {
	var (
		olderThan string
		dryRun    bool
	)
	cmd := &cobra.Command{
		Use:   "offload <volume> [path...]",
		Short: "Delete local bytes whose content is durable on every required target",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOffload(cmd, args[0], args[1:], olderThan, dryRun)
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "", "only files whose indexed mtime is older than this duration (Go durations like 720h, or whole days like 90d)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the per-file durability gate decisions without deleting anything")
	return cmd
}

func runOffload(cmd *cobra.Command, volumeName string, paths []string, olderThanStr string, dryRun bool) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	vol, ok := cfg.Volumes[volumeName]
	if !ok {
		return fmt.Errorf("unknown volume %q (declare it in %s)", volumeName, cfg.Path)
	}
	if len(vol.OffloadRequires) == 0 {
		return fmt.Errorf("volume %q has no offload_requires policy in %s; offload refuses to delete without an explicit list of required targets", volumeName, cfg.Path)
	}
	olderThan, err := parseOlderThan(olderThanStr)
	if err != nil {
		return err
	}

	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	rep, err := offload.Offload(cmd.Context(), s, vol.Path, offload.Options{
		Name:           volumeName,
		Paths:          paths,
		OlderThan:      olderThan,
		Require:        vol.OffloadRequires,
		RequireDests:   requiredDestinations(cfg, vol.OffloadRequires),
		MaxEvidenceAge: vol.OffloadMaxEvidenceAge,
		DryRun:         dryRun,
	})
	printOffloadReport(cmd.OutOrStdout(), cmd.ErrOrStderr(), rep, dryRun)
	if err != nil {
		return err
	}
	if rep.Errors > 0 {
		return fmt.Errorf("%d file(s) failed during offload", rep.Errors)
	}
	return nil
}

// requiredDestinations resolves the volume's offload_requires target names
// to their local destination configs, for the offload capability
// pre-check. Names with no local destination — peer-relayed targets this
// node cannot see — are omitted; the per-file gate handles those.
func requiredDestinations(cfg *config.Config, require []string) map[string]*config.Destination {
	out := make(map[string]*config.Destination, len(require))
	for _, name := range require {
		if d, ok := cfg.Destinations[name]; ok {
			out[name] = d
		}
	}
	return out
}

// olderThanDaysRE accepts the whole-day shorthand (e.g. "90d") that
// time.ParseDuration lacks.
var olderThanDaysRE = regexp.MustCompile(`^(\d+)d$`)

// parseOlderThan parses the --older-than flag: empty means no age
// selector, "<n>d" means n whole days, anything else must parse as a
// positive Go duration.
func parseOlderThan(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	var d time.Duration
	if m := olderThanDaysRE.FindStringSubmatch(s); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, fmt.Errorf("--older-than %q: %w", s, err)
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("--older-than %q: %w (use a Go duration like 720h, or whole days like 90d)", s, err)
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("--older-than must be a positive duration, got %s", s)
	}
	return d, nil
}

// printOffloadReport renders the per-file lines and the summary. Skips
// (gate failures, drift) go to stdout — they are decisions, part of the
// normal report — while selector warnings and per-file errors go to
// stderr.
func printOffloadReport(out, errOut io.Writer, rep offload.Report, dryRun bool) {
	for _, miss := range rep.SelectorMisses {
		fmt.Fprintf(errOut, "warning: selector %q matched no present files\n", miss)
	}
	verb := "offloaded"
	if dryRun {
		verb = "would offload"
	}
	for _, r := range rep.Results {
		switch r.Outcome {
		case offload.OutcomeOffloaded:
			fmt.Fprintf(out, "%s %s\n", verb, r.Path)
		case offload.OutcomeNotDurable:
			fmt.Fprintf(out, "skipped %s: not durable\n", r.Path)
			for _, reason := range r.Reasons {
				fmt.Fprintf(out, "  %s\n", reason)
			}
		case offload.OutcomeDrift:
			fmt.Fprintf(out, "skipped %s: disk differs from index: %s\n", r.Path, r.Reasons[0])
		case offload.OutcomeError:
			fmt.Fprintf(errOut, "error %s: %s\n", r.Path, r.Reasons[0])
		}
	}
	if rep.FinishErr != nil {
		fmt.Fprintf(errOut, "warning: failed to record terminal run state: %v\n", rep.FinishErr)
	}
	prefix := ""
	if dryRun {
		prefix = "(dry-run) "
	}
	fmt.Fprintf(out, "%soffloaded=%d not_durable=%d drift=%d errors=%d", prefix,
		rep.Offloaded, rep.NotDurable, rep.Drift, rep.Errors)
	if rep.RunID != 0 {
		fmt.Fprintf(out, " run=%d", rep.RunID)
	}
	fmt.Fprintln(out)
}

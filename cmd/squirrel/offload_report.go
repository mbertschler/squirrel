package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/mbertschler/squirrel/offload"
)

// printOffloadReport renders one offload invocation. Decisions — what
// moved, what the gate refused, and which requirements it already
// satisfies — go to stdout as the normal report; selector warnings and
// per-file operational errors go to stderr.
//
// Gate refusals are reported in aggregate: one entry per (target, cause)
// with a file count, rather than the same explanation repeated once per
// file. perFile additionally lists every refused path with its own
// reasons.
func printOffloadReport(out, errOut io.Writer, rep offload.Report, dryRun, perFile bool) {
	for _, miss := range rep.SelectorMisses {
		fmt.Fprintf(errOut, "warning: selector %q matched no present files\n", miss)
	}
	printOffloadFiles(out, errOut, rep, dryRun, perFile)
	printOffloadBlocked(out, rep, perFile)
	printOffloadRequirements(out, rep)
	if rep.FinishErr != nil {
		fmt.Fprintf(errOut, "warning: failed to record terminal run state: %v\n", rep.FinishErr)
	}
	printOffloadTotals(out, rep, dryRun)
}

// printOffloadFiles prints the per-file lines: everything that moved (or
// would move), every drift skip, every operational error — and, only
// under perFile, each gate refusal with its per-target reasons and their
// exact vector coordinates.
func printOffloadFiles(out, errOut io.Writer, rep offload.Report, dryRun, perFile bool) {
	verb := "offloaded"
	if dryRun {
		verb = "would offload"
	}
	for _, r := range rep.Results {
		switch r.Outcome {
		case offload.OutcomeOffloaded:
			fmt.Fprintf(out, "%s %s\n", verb, r.Path)
		case offload.OutcomeNotDurable:
			if !perFile {
				continue
			}
			fmt.Fprintf(out, "skipped %s: not durable\n", r.Path)
			for _, f := range r.Failures {
				fmt.Fprintf(out, "  %s\n", f)
				fmt.Fprintf(out, "      %s\n", f.Detail)
			}
		case offload.OutcomeDrift:
			fmt.Fprintf(out, "skipped %s: disk differs from index: %s\n", r.Path, r.Detail)
		case offload.OutcomeError:
			fmt.Fprintf(errOut, "error %s: %s\n", r.Path, r.Detail)
		}
	}
}

// printOffloadBlocked prints the aggregated gate refusals: per (target,
// cause), how many files it blocked, why in self-explaining words, what
// would clear it, and — one line below — the durability-vector
// coordinates that produced the decision, so the precise numbers stay
// available without a second run.
func printOffloadBlocked(out io.Writer, rep offload.Report, perFile bool) {
	if len(rep.Blocked) == 0 {
		return
	}
	fmt.Fprintf(out, "\n%s blocked by the durability gate (of %d selected):\n",
		fileCount(rep.NotDurable), len(rep.Results))
	for _, g := range rep.Blocked {
		fmt.Fprintf(out, "  %s: %s — %s\n", g.Target, fileCount(g.Files), g.Summary)
		fmt.Fprintf(out, "      next: %s\n", g.Advice)
		fmt.Fprintf(out, "      %s\n", blockedCoordinates(g))
	}
	if !perFile {
		fmt.Fprintln(out, "  (--per-file lists every blocked path with its own reasons)")
	}
}

// blockedCoordinates renders a group's vector detail, naming the file it
// belongs to whenever the group's members do not all share it.
func blockedCoordinates(g offload.BlockedGroup) string {
	if g.Files == 1 {
		return fmt.Sprintf("coordinates (%s): %s", g.ExamplePath, g.ExampleDetail)
	}
	if g.UniformDetail {
		return fmt.Sprintf("coordinates (all %s): %s", fileCount(g.Files), g.ExampleDetail)
	}
	return fmt.Sprintf("coordinates (e.g. %s): %s", g.ExamplePath, g.ExampleDetail)
}

// printOffloadRequirements reports every required target affirmatively —
// how many of the evaluated files it already covers, and how many it
// blocks. Without it a refusal cannot be read: one satisfied requirement
// short of an offload looks exactly like nothing being durable anywhere.
func printOffloadRequirements(out io.Writer, rep offload.Report) {
	if len(rep.Requirements) == 0 || len(rep.Results) == 0 {
		return
	}
	total := len(rep.Results)
	fmt.Fprintf(out, "\ndurability requirements (%s checked):\n", fileCount(total))
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, r := range rep.Requirements {
		line := fmt.Sprintf("  %s\tsatisfied for %d of %d", r.Target, r.Satisfied, total)
		if r.Blocked > 0 {
			line += fmt.Sprintf("\tblocks %s", fileCount(r.Blocked))
		}
		fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()
}

// fileCount renders a count with its noun, so the report reads "1 file"
// rather than "1 file(s)".
func fileCount(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

// printOffloadTotals prints the machine-readable summary line that closes
// every invocation.
func printOffloadTotals(out io.Writer, rep offload.Report, dryRun bool) {
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

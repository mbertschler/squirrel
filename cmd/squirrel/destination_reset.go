package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/store"
)

// newDestinationResetCmd returns `squirrel destination reset <destination>`:
// the explicit operator verb that forgets a destination's recorded upload and
// durability state so the next sync treats it as fresh (friction log F20).
// It is a *change* command in the ux-principles sense — weighty and
// irreversible in effect — so it defaults to refusing, offers a --dry-run
// preview, and requires an explicit --yes to proceed. The runs table and the
// append-only durability advance log are preserved; the reset itself is
// recorded as an audit run.
func newDestinationResetCmd() *cobra.Command {
	var (
		yes    bool
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "reset <destination>",
		Short: "Forget a destination's recorded upload and durability state (audit-preserving)",
		Long: `Forget everything the index records about a destination's remote state:
its per-content and per-pack upload ledgers, its live durability vector, and
its push-freshness maxima. The next sync then treats the destination as fresh
and re-uploads.

Use it to recover a wrecked or repointed destination — e.g. after wiping the
remote root, or pointing an existing destination name at a fresh root, where
the layout guard would otherwise refuse on name-keyed run history alone.

This clears derived state only. The runs table and the append-only durability
advance log are untouched, so the audit trail survives; the reset is itself
recorded as an audit run. The remote bytes are not deleted — wipe or repoint
the remote root separately if you want the destination genuinely empty.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDestinationReset(cmd, args[0], yes, dryRun)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the reset; required to actually clear recorded state")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be cleared without changing anything")
	return cmd
}

func runDestinationReset(cmd *cobra.Command, name string, yes, dryRun bool) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	if _, ok := cfg.Destinations[name]; !ok {
		return fmt.Errorf("unknown destination %q (declare it in %s); destination reset clears a bucket destination's recorded state — a peer node's durability assertions are revoked separately", name, cfg.Path)
	}

	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	counts, err := s.CountDestinationRecordedState(cmd.Context(), name)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if counts.Empty() {
		fmt.Fprintf(out, "destination %q has no recorded state — nothing to reset\n", name)
		return nil
	}
	if dryRun {
		printResetPreview(out, name, counts, true)
		return nil
	}
	if !yes {
		printResetPreview(out, name, counts, false)
		return fmt.Errorf("refusing to clear recorded state without confirmation; re-run with --yes (or --dry-run to preview)")
	}

	runID, cleared, err := s.ResetDestination(cmd.Context(), name)
	if err != nil {
		return err
	}
	printResetResult(out, name, cleared, runID)
	return nil
}

// printResetPreview renders the per-category counts that a reset would clear.
// dryRun toggles only the leading verb so the same table serves both the
// explicit --dry-run preview and the confirmation gate.
func printResetPreview(out io.Writer, name string, c store.DestinationResetCounts, dryRun bool) {
	prefix := "reset would clear"
	if !dryRun {
		prefix = "this will clear"
	}
	fmt.Fprintf(out, "%s destination %q recorded state:\n", prefix, name)
	printResetCounts(out, c)
	fmt.Fprintln(out, "  (runs history and the durability advance log are preserved; remote bytes are untouched)")
}

// printResetResult renders the loud confirmation after a reset ran, naming
// the audit run that recorded it and what to do next.
func printResetResult(out io.Writer, name string, c store.DestinationResetCounts, runID int64) {
	fmt.Fprintf(out, "reset destination %q (run %d):\n", name, runID)
	printResetCounts(out, c)
	fmt.Fprintf(out, "next: the next sync re-uploads to %q; for a content-addressed or packed layout, wipe or repoint the remote root so the layout guard sees a fresh start\n", name)
}

func printResetCounts(out io.Writer, c store.DestinationResetCounts) {
	fmt.Fprintf(out, "  upload records: %d objects, %d packs\n", c.RemoteObjects, c.RemotePacks)
	fmt.Fprintf(out, "  durability vector: %d component(s)\n", c.VectorComponents)
	fmt.Fprintf(out, "  push freshness: %d row(s)\n", c.FreshnessRows)
}

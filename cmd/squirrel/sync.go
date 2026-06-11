package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/sync"
)

// newSyncCmd returns the `squirrel sync` cobra command. With no arguments
// it syncs every (volume, destination) pair declared in config. With one
// positional argument it syncs every destination for that volume; with
// --to it narrows to a single pair.
func newSyncCmd() *cobra.Command {
	var (
		to      string
		shallow bool
		dryRun  bool
		initDst bool
	)
	cmd := &cobra.Command{
		Use:   "sync [<volume>]",
		Short: "Push configured volumes to their rclone destinations",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			volumeName := ""
			if len(args) == 1 {
				volumeName = args[0]
			}
			return runSync(cmd, volumeName, to, sync.Options{
				Shallow: shallow,
				DryRun:  dryRun,
				Init:    initDst,
			})
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "limit to this destination name (default: every destination declared on the volume)")
	cmd.Flags().BoolVar(&shallow, "shallow", false, "skip BLAKE3 verification; trust rclone's default size+mtime comparison")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview rclone actions without transferring; no runs row is written")
	cmd.Flags().BoolVar(&initDst, "init", false, "bootstrap a .squirrel-volume marker at the destination on first sync (refused subsequently if the marker mismatches)")
	return cmd
}

func runSync(cmd *cobra.Command, volumeName, destinationName string, opts sync.Options) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	pairs, err := sync.PairsFor(cfg, volumeName, destinationName)
	if err != nil {
		return err
	}
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	rcl, err := sync.Find()
	if err != nil {
		return err
	}
	tools, err := sync.ToolsFor(cfg, pairs, rcl)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if opts.Shallow {
		fmt.Fprintln(out, shallowSyncWarning)
	}
	if err := sync.EnsureMinVersion(cmd.Context(), rcl, out, sync.ShallowForPairs(pairs, opts.Shallow)); err != nil {
		return err
	}
	if err := writeRcloneConfigLogged(out, rcl, cfg); err != nil {
		return err
	}
	// One snapshotter shared across every pair: the VACUUM INTO snapshot
	// is taken once per invocation and fanned out (decision #1). Disabled
	// for dry-run (no run rows to snapshot against) and when [backups] is
	// turned off.
	if !opts.DryRun && cfg.Backups.Enabled {
		opts.Snapshot = sync.NewSnapshotter(s, rcl, snapshotConfig(cfg, s.Path()))
	}

	var anyFailed bool
	for _, p := range pairs {
		rep, err := sync.RunPair(cmd.Context(), s, tools, p, opts)
		printSyncReport(out, rep, err)
		if err != nil || rep.Status != "success" {
			anyFailed = true
		}
	}
	if anyFailed {
		return fmt.Errorf("one or more sync runs did not succeed")
	}
	return nil
}

// snapshotConfig resolves the [backups] config into the sync package's
// SnapshotConfig, filling an empty backups dir with the default sibling
// backups/ directory next to the live DB (the same tier `db backup` and
// the pre-migration snapshots use).
func snapshotConfig(cfg *config.Config, dbPath string) sync.SnapshotConfig {
	dir := cfg.Backups.Dir
	if dir == "" {
		dir = defaultBackupsDir(dbPath)
	}
	return sync.SnapshotConfig{
		Dir:       dir,
		Keep:      cfg.Backups.Keep,
		Cloud:     cfg.Backups.Cloud,
		CloudKeep: cfg.Backups.CloudKeep,
	}
}

// shallowSyncWarning is printed at the CLI layer when a sync or restore
// runs with --shallow. It spells out the safety trade so the operator
// knows a destination whose size and mtime happen to match the source
// won't be re-verified by content hash on this run.
const shallowSyncWarning = "warning: shallow mode: skipping BLAKE3 verification; destination drift with matching size/mtime will not be detected"

// rcloneConfigPathFor co-locates the rclone.conf next to the squirrel
// config so a user inspecting ~/.squirrel/ finds both files. Its contents
// are derived from the squirrel config and should not be edited by hand;
// squirrel rewrites the file only when that derived content changes.
func rcloneConfigPathFor(cfg *config.Config) string {
	return filepath.Join(filepath.Dir(cfg.Path), "rclone.conf")
}

// writeRcloneConfigLogged renders the rclone.conf and logs a single line
// when the file actually changed. An unexpected rewrite is worth
// surfacing: the config is derived deterministically from squirrel's
// config, so a rewrite between two otherwise-identical invocations means a
// destination's resolved credentials or layout shifted underfoot.
func writeRcloneConfigLogged(out io.Writer, rcl *sync.Rclone, cfg *config.Config) error {
	wrote, err := rcl.WriteRcloneConfig(rcloneConfigPathFor(cfg), cfg.Destinations)
	if err != nil {
		return err
	}
	if wrote {
		fmt.Fprintf(out, "rclone.conf updated at %s\n", rcloneConfigPathFor(cfg))
	}
	return nil
}

func printSyncReport(w io.Writer, rep sync.Report, runErr error) {
	r := rep.RcloneResult
	for _, msg := range rep.Warnings {
		fmt.Fprintf(w, "warning: %s\n", msg)
	}
	for _, msg := range rep.NodePendingWarnings {
		fmt.Fprintf(w, "warning: peer reports %s\n", msg)
	}
	// Kopia pushes have no rclone counters; render the snapshot's own
	// numbers instead.
	if rep.Verification.Method == sync.VerifyMethodKopia {
		fmt.Fprintf(w, "%s → %s  status=%s files=%d bytes=%d snapshot=%s verified=%t run=%d\n",
			rep.Volume, rep.Destination, rep.Status,
			rep.Verification.Files, rep.Verification.Bytes,
			rep.Verification.SnapshotID, rep.Verification.Verified(), rep.RunID,
		)
	} else {
		fmt.Fprintf(w, "%s → %s  status=%s transferred=%d checked=%d errors=%d bytes=%d run=%d\n",
			rep.Volume, rep.Destination, rep.Status,
			r.Transferred, r.Checked, r.Errors, r.Bytes, rep.RunID,
		)
	}
	if rep.NodeReceiverRunID != 0 {
		fmt.Fprintf(w, "  receiver_run=%d matched=%d mismatched=%d missing=%d conflicts=%d\n",
			rep.NodeReceiverRunID,
			len(rep.NodeVerify.Matched),
			len(rep.NodeVerify.Mismatched),
			len(rep.NodeVerify.Missing),
			len(rep.NodeConflicts),
		)
		for _, m := range rep.NodeVerify.Mismatched {
			fmt.Fprintf(w, "    mismatched %s: expected %s, actual %s\n", m.Path, m.ExpectedHex, m.ActualHex)
		}
		if rep.DurabilityPull.Fetched > 0 {
			fmt.Fprintf(w, "  durability: applied %d/%d peer components\n",
				rep.DurabilityPull.Applied, rep.DurabilityPull.Fetched)
		}
	}
	for _, c := range rep.NodeConflicts {
		fmt.Fprintf(w, "  conflict %s: %s — was %s, now %s\n",
			c.Path, c.Reason, c.ReceiverBlake3Hex, c.InitiatorBlake3Hex)
		if c.PreservedAtPath != "" {
			fmt.Fprintf(w, "    preserved at %s:%s\n", rep.Destination, c.PreservedAtPath)
		}
	}
	for _, ff := range r.FailedFiles {
		// Some rclone errors (auth, listing, fatal copy) have no Object.
		// Render those as a bare "error: ..." rather than "error : ...".
		if ff.Object == "" {
			fmt.Fprintf(w, "  error: %s\n", ff.Message)
		} else {
			fmt.Fprintf(w, "  error %s: %s\n", ff.Object, ff.Message)
		}
	}
	if rep.FinishErr != nil {
		// Distinct from rclone errors: the data is at the destination, but
		// the runs row is stuck in 'running' until manually reconciled.
		fmt.Fprintf(w, "  warning: failed to record terminal run state: %v\n", rep.FinishErr)
	}
	if rep.SnapshotErr != nil {
		// Defense-in-depth only: the sync itself succeeded; the index
		// snapshot or its ride-along did not. Surface it without colouring
		// the run's status.
		fmt.Fprintf(w, "  warning: index snapshot: %v\n", rep.SnapshotErr)
	}
	if runErr != nil {
		fmt.Fprintf(w, "  %v\n", runErr)
	}
}

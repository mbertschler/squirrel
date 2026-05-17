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
			})
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "limit to this destination name (default: every destination declared on the volume)")
	cmd.Flags().BoolVar(&shallow, "shallow", false, "skip BLAKE3 verification; trust rclone's default size+mtime comparison")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview rclone actions without transferring; no runs row is written")
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
	out := cmd.OutOrStdout()
	if err := sync.EnsureMinVersion(cmd.Context(), rcl, out, opts.Shallow); err != nil {
		return err
	}
	if err := rcl.WriteRcloneConfig(rcloneConfigPathFor(cfg), cfg.Destinations); err != nil {
		return err
	}

	var anyFailed bool
	for _, p := range pairs {
		rep, err := sync.RunPair(cmd.Context(), s, rcl, p, opts)
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

// rcloneConfigPathFor co-locates the rclone.conf next to the squirrel
// config so a user inspecting ~/.squirrel/ finds both files. The file is
// fully rewritten on every sync invocation; its contents are derived from
// the squirrel config and should not be edited by hand.
func rcloneConfigPathFor(cfg *config.Config) string {
	return filepath.Join(filepath.Dir(cfg.Path), "rclone.conf")
}

func printSyncReport(w io.Writer, rep sync.Report, runErr error) {
	r := rep.RcloneResult
	for _, msg := range rep.Warnings {
		fmt.Fprintf(w, "warning: %s\n", msg)
	}
	fmt.Fprintf(w, "%s → %s  status=%s transferred=%d checked=%d errors=%d bytes=%d run=%d\n",
		rep.Volume, rep.Destination, rep.Status,
		r.Transferred, r.Checked, r.Errors, r.Bytes, rep.RunID,
	)
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
	if runErr != nil {
		fmt.Fprintf(w, "  %v\n", runErr)
	}
}

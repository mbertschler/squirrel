package main

import (
	"encoding/hex"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/store"
)

// newConflictsResolveCmd returns `squirrel conflicts resolve <volume>
// <path>`: the explicit human act that clears a contested freeze (#158,
// F27). Resolution is deliberately a human decision — squirrel never
// resolves a conflict on its own ("irreversible acts remain human"). The
// command clears the standing freeze so syncs flow again; it does not
// itself move bytes between the two versions. Both remain preserved: the
// winner live at the path, the loser under `.squirrel-conflicts/`. To
// adopt the *other* version, restore the preserved copy first — the output
// names exactly where it is.
//
// Clearing loses no history: every conflict run survives in the runs
// table, the raise and this clear survive in runs_audit, and the preserved
// bytes stay on disk. If a peer still holds the divergent version, its
// next sync will re-freeze the path — a fresh, singly-preserved, human-
// notified event, not the unbounded ping-pong F27 described.
func newConflictsResolveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resolve <volume> <path>",
		Short: "Clear a contested freeze so syncs flow again (explicit human act)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConflictsResolve(cmd, args[0], args[1])
		},
	}
}

func runConflictsResolve(cmd *cobra.Command, volumeName, relPath string) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	vol, err := s.GetVolumeByName(cmd.Context(), volumeName)
	if err != nil {
		if store.IsNotFound(err) {
			return fmt.Errorf("no volume named %q", volumeName)
		}
		return fmt.Errorf("lookup volume %q: %w", volumeName, err)
	}
	latch, err := s.GetContestedPath(cmd.Context(), vol.ID, relPath)
	if err != nil {
		if store.IsNotFound(err) {
			return fmt.Errorf("path %q in volume %q is not contested", relPath, volumeName)
		}
		return fmt.Errorf("look up contested %q: %w", relPath, err)
	}
	cleared, err := s.ClearContested(cmd.Context(), vol.ID, relPath, ackOperator())
	if err != nil {
		return err
	}
	if !cleared {
		// Raced with another resolve between the read and the write — the
		// freeze is gone either way, the outcome the operator asked for.
		return fmt.Errorf("path %q in volume %q is not contested", relPath, volumeName)
	}
	printResolved(cmd, volumeName, relPath, latch)
	return nil
}

// printResolved confirms the clear and points the operator at both
// preserved versions so adopting the non-live one stays a deliberate,
// informed act rather than a surprise the resolve silently performed.
func printResolved(cmd *cobra.Command, volumeName, relPath string, latch store.ContestedPath) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "resolved %s %s: contested freeze cleared — syncs may flow again\n", volumeName, relPath)
	fmt.Fprintf(out, "  live version kept: %s\n", digestOrUnknown(latch.LiveBlake3))
	if latch.PreservedAtPath != "" {
		fmt.Fprintf(out, "  preserved version %s remains at %s (restore it to adopt that version instead)\n",
			digestOrUnknown(latch.PreservedBlake3), latch.PreservedAtPath)
	}
}

// digestOrUnknown renders a digest as full hex, or a placeholder when the
// latch recorded no digest (an older or partial freeze record).
func digestOrUnknown(b []byte) string {
	if len(b) == 0 {
		return "(unknown)"
	}
	return hex.EncodeToString(b)
}

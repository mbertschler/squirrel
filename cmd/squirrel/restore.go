package main

import (
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/sync"
)

// newRestoreCmd returns the `squirrel restore <volume>` cobra command. By
// default it restores to the volume's declared path, overwriting whatever
// is there on a hash mismatch. Use --to to point at a scratch directory.
// --from picks which destination to pull from when the volume has more
// than one; it is required only when sync_to has multiple entries.
func newRestoreCmd() *cobra.Command {
	var (
		from    string
		to      string
		shallow bool
		dryRun  bool
	)
	cmd := &cobra.Command{
		Use:   "restore <volume>",
		Short: "Pull a volume back from one of its rclone destinations",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRestore(cmd, args[0], from, sync.RestoreOptions{
				ToPath:  to,
				Shallow: shallow,
				DryRun:  dryRun,
			})
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "destination name to restore from (required when the volume syncs to multiple destinations)")
	cmd.Flags().StringVar(&to, "to", "", "local target path (default: the volume's declared path)")
	cmd.Flags().BoolVar(&shallow, "shallow", false, "skip BLAKE3 verification on the way down")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview rclone actions without transferring")
	return cmd
}

func runRestore(cmd *cobra.Command, volumeName, destName string, opts sync.RestoreOptions) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	vol, ok := cfg.Volumes[volumeName]
	if !ok {
		return fmt.Errorf("unknown volume %q (declare it in %s)", volumeName, cfg.Path)
	}
	dest, err := pickRestoreDestination(cfg, vol, destName)
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

	rep, runErr := sync.Restore(cmd.Context(), s, rcl, vol, dest, opts)
	printSyncReport(out, rep, runErr)
	if runErr != nil || rep.Status != "success" {
		return fmt.Errorf("restore did not complete cleanly")
	}
	return nil
}

// pickRestoreDestination resolves the --from flag against the volume's
// sync_to list. With no --from and exactly one destination on the volume,
// that single destination is used. With multiple destinations and no
// --from, we error rather than guess — the user must be explicit.
func pickRestoreDestination(cfg *config.Config, vol *config.Volume, destName string) (*config.Destination, error) {
	if destName != "" {
		dest, ok := cfg.Destinations[destName]
		if !ok {
			return nil, fmt.Errorf("unknown destination %q", destName)
		}
		// The named destination must be one the volume actually syncs to,
		// otherwise the per-volume tree at the destination is empty.
		if !slices.Contains(vol.SyncTo, destName) {
			return nil, fmt.Errorf("destination %q is not in sync_to for volume %q", destName, vol.Name)
		}
		return dest, nil
	}
	if len(vol.SyncTo) == 0 {
		return nil, fmt.Errorf("volume %q has no sync_to destinations", vol.Name)
	}
	if len(vol.SyncTo) > 1 {
		return nil, fmt.Errorf("volume %q has multiple destinations (%v) — pick one with --from", vol.Name, vol.SyncTo)
	}
	return cfg.Destinations[vol.SyncTo[0]], nil
}
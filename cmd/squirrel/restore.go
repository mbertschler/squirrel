package main

import (
	"database/sql"
	"fmt"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/sync"
)

// newRestoreCmd returns the `squirrel restore <volume>` cobra command. By
// default it restores to the volume's declared path, overwriting whatever
// is there on a hash mismatch. Use --to to point at a scratch directory.
// --from accepts either a destination name (pick which bucket to pull
// from when the volume has multiple) or a node name (filter to paths
// whose source_node_id matches that node — handy on a receiver that
// holds files pushed by multiple peers). Destination and node names
// share one namespace (config validation enforces uniqueness), so the
// argument disambiguates by lookup, with no extra flag needed.
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
	cmd.Flags().StringVar(&from, "from", "", "destination name to pull from, or peer node name to filter source attribution (overloaded; names are unique across both kinds)")
	cmd.Flags().StringVar(&to, "to", "", "local target path (default: the volume's declared path)")
	cmd.Flags().BoolVar(&shallow, "shallow", false, "skip BLAKE3 verification on the way down")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview rclone actions without transferring")
	return cmd
}

func runRestore(cmd *cobra.Command, volumeName, fromName string, opts sync.RestoreOptions) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	vol, ok := cfg.Volumes[volumeName]
	if !ok {
		return fmt.Errorf("unknown volume %q (declare it in %s)", volumeName, cfg.Path)
	}

	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	dest, sourceNode, err := resolveRestoreTarget(cmd, s, cfg, vol, fromName)
	if err != nil {
		return err
	}

	if sourceNode.active {
		includeFile, cleanup, err := writeRestorePathFilter(cmd, s, vol, sourceNode.nodeID)
		if err != nil {
			return err
		}
		defer cleanup()
		if includeFile == "" {
			return fmt.Errorf("no rows attributed to --from %q for volume %q", fromName, vol.Name)
		}
		opts.IncludeFromFile = includeFile
	}

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

// restoreSourceFilter is the resolution of `--from <name>` against the
// `nodes` table. active=false means the name (if any) is a
// destination, not a node; active=true with nodeID.Valid filters to
// that peer; active=true with nodeID zero filters to NULL source
// (local writes).
type restoreSourceFilter struct {
	active bool
	nodeID sql.NullInt64
}

// resolveRestoreTarget decides what `--from <name>` means: a node name
// (returns a source filter, picks the destination from sync_to), a
// destination name (returns the destination, no filter), or empty
// (auto-picks the destination).
//
// Resolution checks both namespaces and errors on a tie so a
// `node_name` matching a destination name can't silently steal
// `--from <self-node>`: config.Load enforces uniqueness across
// [nodes.X] and [destinations.X], but does not check the top-level
// node_name. An honest collision is surfaced rather than picked one
// way or the other.
func resolveRestoreTarget(cmd *cobra.Command, s *store.Store, cfg *config.Config, vol *config.Volume, fromName string) (*config.Destination, restoreSourceFilter, error) {
	if fromName == "" {
		dest, err := pickSingleRestoreDestination(cfg, vol)
		return dest, restoreSourceFilter{}, err
	}
	filter, nodeFound, err := lookupNodeFilter(cmd, s, fromName)
	if err != nil {
		return nil, restoreSourceFilter{}, err
	}
	destMatch, destOK := cfg.Destinations[fromName]
	if nodeFound && destOK {
		return nil, restoreSourceFilter{}, fmt.Errorf("--from %q is ambiguous: it names both a node (self or peer) and a destination — rename one to disambiguate", fromName)
	}
	if nodeFound {
		dest, err := pickSingleRestoreDestination(cfg, vol)
		if err != nil {
			return nil, restoreSourceFilter{}, fmt.Errorf("--from %q is a node name; %w", fromName, err)
		}
		return dest, filter, nil
	}
	if destOK {
		if !slices.Contains(vol.SyncTo, fromName) {
			return nil, restoreSourceFilter{}, fmt.Errorf("destination %q is not in sync_to for volume %q", fromName, vol.Name)
		}
		return destMatch, restoreSourceFilter{}, nil
	}
	return nil, restoreSourceFilter{}, fmt.Errorf("--from %q matches neither a configured destination nor a known node", fromName)
}

// lookupNodeFilter asks the store whether name refers to a node — the
// self-row's name (NULL filter) or a peer row (filter by id). The
// found flag distinguishes "not a node, try the next namespace" from
// "node lookup itself failed" so the caller can keep the dispatch
// flat. A surfaced error reflects an underlying store failure, not a
// missing row.
func lookupNodeFilter(cmd *cobra.Command, s *store.Store, name string) (restoreSourceFilter, bool, error) {
	self, err := s.GetSelfNode(cmd.Context())
	if err != nil {
		return restoreSourceFilter{}, false, fmt.Errorf("lookup self node: %w", err)
	}
	if name == self.Name {
		return restoreSourceFilter{active: true}, true, nil
	}
	node, err := s.GetNodeByName(cmd.Context(), name)
	if err != nil {
		if store.IsNotFound(err) {
			return restoreSourceFilter{}, false, nil
		}
		return restoreSourceFilter{}, false, fmt.Errorf("lookup node %q: %w", name, err)
	}
	return restoreSourceFilter{active: true, nodeID: sql.NullInt64{Int64: node.ID, Valid: true}}, true, nil
}

// pickSingleRestoreDestination resolves the destination when --from
// did not name one. With exactly one entry in sync_to we use it; with
// more, the user must disambiguate by passing the destination name as
// --from. This is the same UX the destination-only flow had before
// node filtering was added.
func pickSingleRestoreDestination(cfg *config.Config, vol *config.Volume) (*config.Destination, error) {
	if len(vol.SyncTo) == 0 {
		return nil, fmt.Errorf("volume %q has no sync_to destinations", vol.Name)
	}
	if len(vol.SyncTo) > 1 {
		return nil, fmt.Errorf("volume %q has multiple destinations (%v) — pick one by name via --from", vol.Name, vol.SyncTo)
	}
	dest, ok := cfg.Destinations[vol.SyncTo[0]]
	if !ok {
		// sync_to may name a node, not a bucket; restore doesn't yet
		// pull from peer nodes, so surface that mismatch explicitly.
		return nil, fmt.Errorf("volume %q syncs only to %q which is not a bucket destination — restore from node destinations is not supported", vol.Name, vol.SyncTo[0])
	}
	return dest, nil
}

// writeRestorePathFilter materialises the path subset implied by
// `--from <node>` to a tempfile suitable for rclone's --files-from
// flag. It iterates ListPresentBySource against the volume row in the
// DB (not the volume from config) so a missing volume row surfaces
// before rclone gets invoked. Returns the file path, a cleanup func,
// and an error. The cleanup is non-nil even on error so deferring it
// is always safe.
func writeRestorePathFilter(cmd *cobra.Command, s *store.Store, vol *config.Volume, nodeID sql.NullInt64) (string, func(), error) {
	v, err := s.GetVolumeByName(cmd.Context(), vol.Name)
	if err != nil {
		if store.IsNotFound(err) {
			return "", func() {}, fmt.Errorf("volume %q has no index rows yet — index it on the receiver before restoring with --from", vol.Name)
		}
		return "", func() {}, fmt.Errorf("lookup volume %q: %w", vol.Name, err)
	}
	f, err := os.CreateTemp("", "squirrel-restore-files-*.txt")
	if err != nil {
		return "", func() {}, fmt.Errorf("create files-from tempfile: %w", err)
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	var count int
	for row, iterErr := range s.ListPresentBySource(cmd.Context(), v.ID, nodeID) {
		if iterErr != nil {
			_ = f.Close()
			cleanup()
			return "", func() {}, fmt.Errorf("list present by source: %w", iterErr)
		}
		if _, err := fmt.Fprintln(f, row.Path); err != nil {
			_ = f.Close()
			cleanup()
			return "", func() {}, fmt.Errorf("write files-from entry: %w", err)
		}
		count++
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close files-from tempfile: %w", err)
	}
	if count == 0 {
		cleanup()
		return "", func() {}, nil
	}
	return f.Name(), cleanup, nil
}

package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/sync"
)

// newPeerSyncPullDurabilityCmd returns the `squirrel peer-sync
// pull-durability <volume> <peer>` subcommand: a standalone run of the
// metadata-only durability pull that also fires automatically after a
// successful node sync. It fetches the peer's destination durability
// vectors for the volume and merges them into the local
// destination_run_ids — monotonic, with refused rewinds reported and an
// --allow-rewind override mirroring the watermark store's opt-in.
func newPeerSyncPullDurabilityCmd() *cobra.Command {
	var allowRewind bool
	cmd := &cobra.Command{
		Use:   "pull-durability <volume> <peer>",
		Short: "Fetch a peer's destination durability vectors for a volume into the local index",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPeerSyncPullDurability(cmd, args[0], args[1], allowRewind)
		},
	}
	cmd.Flags().BoolVar(&allowRewind, "allow-rewind", false,
		"accept peer components below the locally recorded value (recovery override)")
	return cmd
}

func runPeerSyncPullDurability(cmd *cobra.Command, volumeName, peerName string, allowRewind bool) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	vol, ok := cfg.Volumes[volumeName]
	if !ok {
		return fmt.Errorf("unknown volume %q", volumeName)
	}
	node, ok := cfg.Nodes[peerName]
	if !ok {
		return fmt.Errorf("unknown node %q", peerName)
	}
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	rep, err := sync.PullDurability(cmd.Context(), s, vol, node, allowRewind)
	if err != nil {
		return err
	}
	printDurabilityPull(cmd.OutOrStdout(), rep)
	if len(rep.Rewinds) > 0 {
		return fmt.Errorf("%d component(s) refused as rewinds; re-run with --allow-rewind to accept the peer's values", len(rep.Rewinds))
	}
	return nil
}

func printDurabilityPull(w io.Writer, rep sync.DurabilityPullReport) {
	fmt.Fprintf(w, "%s ← %s  fetched=%d applied=%d\n",
		rep.Volume, rep.Peer, rep.Fetched, rep.Applied)
	for _, rw := range rep.Rewinds {
		fmt.Fprintf(w, "  refused rewind: %s\n", rw)
	}
}

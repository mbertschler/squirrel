package main

import (
	"github.com/spf13/cobra"
)

// newPeerSyncCmd returns the `squirrel peer-sync` parent command. It is a
// namespace for the node-sync auxiliary subcommands; on its own it has no
// behaviour and prints help. Today it carries `history` (the append-only
// watermark transition log added with SAFETY-AUDIT H6) and
// `pull-durability` (the standalone durability metadata pull); future
// per-peer verbs belong here too rather than at the top level.
func newPeerSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "peer-sync",
		Short: "Inspect node-sync state and exchange peer metadata",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newPeerSyncHistoryCmd())
	cmd.AddCommand(newPeerSyncPullDurabilityCmd())
	return cmd
}

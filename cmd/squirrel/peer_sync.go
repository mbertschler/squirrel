package main

import (
	"github.com/spf13/cobra"
)

// newPeerSyncCmd returns the `squirrel peer-sync` parent command. It is a
// namespace for the node-sync forensic subcommands; on its own it has no
// behaviour and prints help. Today it carries `history` (the append-only
// watermark transition log added with SAFETY-AUDIT H6); future per-peer
// inspection verbs belong here too rather than at the top level.
func newPeerSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "peer-sync",
		Short: "Inspect node-sync state and history",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newPeerSyncHistoryCmd())
	return cmd
}

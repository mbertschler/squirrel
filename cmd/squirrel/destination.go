package main

import "github.com/spf13/cobra"

// newDestinationCmd returns the `squirrel destination` parent command
// grouping the destination-level recovery verbs. It has no behaviour of its
// own and prints help when invoked bare.
func newDestinationCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "destination",
		Short: "Manage a destination's recorded state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newDestinationResetCmd())
	return cmd
}

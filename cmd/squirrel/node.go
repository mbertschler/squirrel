package main

import "github.com/spf13/cobra"

// newNodeCmd returns the `squirrel node` parent command grouping the
// peer-relationship bootstrap helpers.
func newNodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Manage peer node relationships",
	}
	cmd.AddCommand(newNodePairCmd())
	return cmd
}

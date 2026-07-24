package main

import "github.com/spf13/cobra"

// newConfigCmd returns the `squirrel config` parent command. Its children
// are introspection over the config file itself (currently `check`), kept
// distinct from the DB-facing question commands.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect the squirrel configuration",
	}
	cmd.AddCommand(newConfigCheckCmd())
	return cmd
}

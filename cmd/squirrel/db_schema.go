package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newDBSchemaCmd returns `squirrel db schema`, which prints the DDL
// (tables, indexes, triggers) of whichever database the usual --db/config
// resolution opens, as a flattened script. Opening runs the normal
// migration chain first, so the output reflects the objects actually
// materialised in that file at the binary's SchemaVersion — useful for an
// operator or agent to inspect a real index directly, without a repo
// checkout of store/schema.sql.
func newDBSchemaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Print the index database's DDL (tables, indexes, triggers)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDBSchema(cmd)
		},
	}
	return cmd
}

func runDBSchema(cmd *cobra.Command) error {
	cfg, _ := tryLoadConfig(cmd) // cfg may be nil; openStore handles that.
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	ddl, err := s.DumpSchema(cmd.Context())
	if err != nil {
		return err
	}
	fmt.Fprint(cmd.OutOrStdout(), ddl)
	return nil
}

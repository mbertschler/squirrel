package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newDBSchemaCmd returns `squirrel db schema`, which prints the live
// database's DDL (tables, indexes, triggers) as a flattened script. It
// reads the shape of whichever DB the usual --db/config resolution opens,
// so an operator or agent can see what a real index actually looks like —
// including its current schema version — without replaying migrations.go
// or trusting the checked-in store/schema.sql to match an older file.
func newDBSchemaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Print the live index database's DDL (tables, indexes, triggers)",
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

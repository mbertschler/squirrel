package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newVolumesCmd returns the `squirrel volumes` cobra command which lists
// known volumes one per line as `id<TAB>name<TAB>path`.
func newVolumesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "volumes",
		Short: "List known indexing volumes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := tryLoadConfig(cmd)
			if err != nil {
				return err
			}
			s, err := openStore(cmd, cfg)
			if err != nil {
				return err
			}
			defer s.Close()

			vols, err := s.ListVolumes(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, v := range vols {
				fmt.Fprintf(out, "%d\t%s\t%s\n", v.ID, v.Name, v.Path)
			}
			return nil
		},
	}
}

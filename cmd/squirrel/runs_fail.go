package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/store"
)

// newRunsFailCmd returns the `squirrel runs fail <id>` subcommand. It
// is the manual recovery path for a row left in status='running' by a
// crashed sync/index/restore process. Without it the in-progress guard
// would refuse to start a new run on the same pair until the operator
// did SQL surgery.
//
// No heuristics, no bulk variants. The operator names the exact run id;
// the write goes through store.FinishRun so the row ends up
// indistinguishable from any clean failure aside from the synthesized
// error message.
func newRunsFailCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fail <id>",
		Short: "Mark a stuck running run as failed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid run id %q: %w", args[0], err)
			}
			return runRunsFail(cmd, runID)
		},
	}
}

func runRunsFail(cmd *cobra.Command, runID int64) error {
	cfg, err := tryLoadConfig(cmd)
	if err != nil {
		return err
	}
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	r, err := s.GetRun(cmd.Context(), runID)
	if err != nil {
		if store.IsNotFound(err) {
			return fmt.Errorf("no run with id %d", runID)
		}
		return fmt.Errorf("lookup run %d: %w", runID, err)
	}
	if r.Status != store.RunStatusRunning {
		return fmt.Errorf("run %d is already %s, refusing to overwrite", runID, r.Status)
	}

	errMsg := fmt.Sprintf("marked failed manually at %s", time.Now().UTC().Format(time.RFC3339))
	if err := s.FinishRun(cmd.Context(), runID, store.RunStatusFailed, errMsg, r.FileCount); err != nil {
		return fmt.Errorf("fail run %d: %w", runID, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "marked run %d as failed\n", runID)
	return nil
}

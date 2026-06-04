package main

import (
	"fmt"
	"os/user"
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
// No heuristics, no bulk variants. The operator names the exact run id.
// The row is moved to 'failed' via store.FinishRun, but — unlike a real
// process failure — a 'manual-fail' runs_audit row is also written so a
// forensic reader can tell an operator recovery from a genuine failure
// without parsing the synthesized error string. The running row's
// file_count is preserved rather than reset.
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
	// Pass r.FileCount — the value just read off the running row — so the
	// manual path leaves the partial count intact rather than zeroing it.
	if err := s.FinishRun(cmd.Context(), runID, store.RunStatusFailed, errMsg, r.FileCount); err != nil {
		return fmt.Errorf("fail run %d: %w", runID, err)
	}
	// Record the operator action as a distinct audit transition. This is
	// the source of truth that the failure was a manual recovery, not a
	// real process failure (FinishRun's own 'finish' row only carries the
	// resulting status, which a real failure shares).
	audit := store.RunAuditEntry{
		RunID:      runID,
		Transition: store.TransitionManualFail,
		Operator:   currentOperator(),
		Note:       errMsg,
	}
	if err := s.AppendRunAudit(cmd.Context(), audit); err != nil {
		return fmt.Errorf("record manual-fail audit for run %d: %w", runID, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "marked run %d as failed\n", runID)
	return nil
}

// currentOperator returns the OS username running the command, or ""
// when it can't be resolved (the audit row then stores NULL for
// operator). Best-effort: the transition tag already distinguishes a
// manual fail; the username is a convenience for the forensic reader.
func currentOperator() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}

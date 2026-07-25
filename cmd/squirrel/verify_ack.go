package main

import (
	"fmt"
	"os/user"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/store"
)

// newVerifyAckCmd returns `squirrel verify ack <destination>`: the explicit
// operator path to clear a standing verify alarm (#157, F30). It is the
// deliberate counterpart to the automatic clear a clean `squirrel verify`
// performs — for when the operator has inspected the destination and
// restored or re-uploaded the affected artifacts out of band, and wants
// the alarm gone without waiting for (or being able to run) a full
// re-verification pass. The clear is recorded against the run that raised
// the alarm and tagged with the operator, so the audit trail shows who
// cleared it and when.
func newVerifyAckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ack <destination>",
		Short: "Acknowledge and clear a standing verify alarm on a destination",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerifyAck(cmd, args[0])
		},
	}
}

func runVerifyAck(cmd *cobra.Command, destName string) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	alarm, err := s.GetDestinationAlarm(cmd.Context(), destName)
	if err != nil {
		if store.IsNotFound(err) {
			return fmt.Errorf("destination %q has no standing alarm to acknowledge", destName)
		}
		return fmt.Errorf("look up standing alarm for %q: %w", destName, err)
	}
	cleared, err := s.ClearDestinationAlarm(cmd.Context(), destName, alarm.RaisedRunID, ackOperator())
	if err != nil {
		return err
	}
	if !cleared {
		// Raced with an auto-clear (a clean verify) between the read and
		// the write — the alarm is gone either way, which is the outcome
		// the operator asked for.
		return fmt.Errorf("destination %q has no standing alarm to acknowledge", destName)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "verify %s: standing alarm acknowledged and cleared\n", destName)
	return nil
}

// ackOperator names who acked for the runs_audit operator column: the OS
// user when resolvable, else a generic label. Best-effort attribution for
// the audit trail, not an authorization check.
func ackOperator() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "operator"
}

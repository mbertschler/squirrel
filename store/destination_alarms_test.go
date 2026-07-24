package store

import (
	"context"
	"testing"
)

// TestDestinationAlarmLifecycle exercises the raise → idempotent re-raise →
// clear cycle and the runs_audit trail each transition leaves.
func TestDestinationAlarmLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	first, err := s.BeginRemoteVerifyRun(ctx)
	if err != nil {
		t.Fatalf("BeginRemoteVerifyRun: %v", err)
	}
	if err := s.RaiseDestinationAlarm(ctx, "offsite", AlarmKindVerifyMismatch, "mismatched=1 missing=0", first); err != nil {
		t.Fatalf("RaiseDestinationAlarm: %v", err)
	}
	got, err := s.GetDestinationAlarm(ctx, "offsite")
	if err != nil {
		t.Fatalf("GetDestinationAlarm: %v", err)
	}
	if got.Kind != AlarmKindVerifyMismatch || got.RaisedRunID != first || got.RaisedAtNs == 0 {
		t.Fatalf("alarm = %+v, want kind/verify-run/timestamp set", got)
	}
	firstAt := got.RaisedAtNs

	// A second detection keeps the original latch: same run, same
	// timestamp, same detail — so "in alarm since" never resets.
	second, err := s.BeginRemoteVerifyRun(ctx)
	if err != nil {
		t.Fatalf("BeginRemoteVerifyRun: %v", err)
	}
	if err := s.RaiseDestinationAlarm(ctx, "offsite", AlarmKindVerifyMismatch, "mismatched=2 missing=1", second); err != nil {
		t.Fatalf("re-raise: %v", err)
	}
	got, err = s.GetDestinationAlarm(ctx, "offsite")
	if err != nil {
		t.Fatalf("GetDestinationAlarm after re-raise: %v", err)
	}
	if got.RaisedRunID != first || got.RaisedAtNs != firstAt || got.Detail != "mismatched=1 missing=0" {
		t.Fatalf("re-raise mutated the latch: %+v", got)
	}

	all, err := s.ListDestinationAlarms(ctx)
	if err != nil {
		t.Fatalf("ListDestinationAlarms: %v", err)
	}
	if len(all) != 1 || all[0].Destination != "offsite" {
		t.Fatalf("active alarms = %+v, want exactly offsite", all)
	}

	// Exactly one alarm-raise audit row against the first run; none against
	// the second (the re-raise was a no-op).
	if n := countTransition(t, s, first, TransitionAlarmRaise); n != 1 {
		t.Fatalf("first run alarm-raise count = %d, want 1", n)
	}
	if n := countTransition(t, s, second, TransitionAlarmRaise); n != 0 {
		t.Fatalf("second run alarm-raise count = %d, want 0 (idempotent re-raise)", n)
	}

	// Clear via ack: recorded against the raising run, tagged operator.
	cleared, err := s.ClearDestinationAlarm(ctx, "offsite", first, "alice")
	if err != nil {
		t.Fatalf("ClearDestinationAlarm: %v", err)
	}
	if !cleared {
		t.Fatal("ClearDestinationAlarm reported no active alarm")
	}
	if _, err := s.GetDestinationAlarm(ctx, "offsite"); !IsNotFound(err) {
		t.Fatalf("alarm still present after clear: %v", err)
	}
	audits, err := s.ListRunAudit(ctx, first)
	if err != nil {
		t.Fatalf("ListRunAudit: %v", err)
	}
	var sawClear bool
	for _, a := range audits {
		if a.Transition == TransitionAlarmClear {
			sawClear = true
			if a.Operator.String != "alice" {
				t.Fatalf("alarm-clear operator = %q, want alice", a.Operator.String)
			}
		}
	}
	if !sawClear {
		t.Fatal("no alarm-clear audit row against the raising run")
	}

	// Clearing an already-clear destination reports false and writes nothing.
	cleared, err = s.ClearDestinationAlarm(ctx, "offsite", first, "alice")
	if err != nil {
		t.Fatalf("second clear: %v", err)
	}
	if cleared {
		t.Fatal("second clear reported an active alarm")
	}
}

func countTransition(t *testing.T, s *Store, runID int64, transition string) int {
	t.Helper()
	audits, err := s.ListRunAudit(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListRunAudit(%d): %v", runID, err)
	}
	n := 0
	for _, a := range audits {
		if a.Transition == transition {
			n++
		}
	}
	return n
}

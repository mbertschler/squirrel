package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Run-audit transition kinds. The runs_audit.transition column is free
// text (no CHECK) so call sites can add their own without a migration;
// these constants name the transitions this codebase writes today so
// callers don't restate the literals. Issue #77 adds
// 'set-correlated-run-id' against the same table.
const (
	// TransitionFinish records a FinishRun call. The note carries the
	// resulting terminal status so a forensic reader can see what the
	// row was moved to without joining back to runs.
	TransitionFinish = "finish"
	// TransitionManualFail records an operator-driven `runs fail`. It is
	// written in addition to the FinishRun-emitted 'finish' row so the
	// listing can distinguish a manual recovery from a real failure.
	TransitionManualFail = "manual-fail"
	// TransitionSetCorrelatedRunID records a SetCorrelatedRunID write: the
	// initiator stamping the receiver's run id onto an already-open row
	// once /v1/sync/begin returns. The note carries the old→new ids so a
	// forensic reader can see a correlation being (re)written without
	// trusting the live, overwrite-in-place runs.correlated_run_id column
	// (SAFETY-AUDIT H6).
	TransitionSetCorrelatedRunID = "set-correlated-run-id"
	// TransitionVerifyDestination records a remote-object verification
	// pass against its kind='audit' run (see BeginRemoteVerifyRun). The
	// note carries the destination name and the pass counters — the runs
	// CHECK keeps destination NULL on audit rows, so this entry is where
	// the audit trail names the verified destination.
	TransitionVerifyDestination = "verify-destination"
	// TransitionAbort records a 'running' row reaped to 'aborted' by
	// AbortRunningRuns at agent startup (#157, F14). The note carries the
	// reap reason so a forensic reader can tell a crash-reap apart from a
	// real failure.
	TransitionAbort = "abort"
	// TransitionAlarmRaise records a destination_alarms latch being raised
	// against the verify run that detected the mismatch (#157, F30). The
	// note carries the destination and a bounded detail summary.
	TransitionAlarmRaise = "alarm-raise"
	// TransitionAlarmClear records a destination_alarms latch being
	// cleared: against the clean verify run that auto-cleared it (operator
	// NULL) or against the run that raised it when an operator acks
	// (operator set). The note carries the destination.
	TransitionAlarmClear = "alarm-clear"
	// TransitionContestedRaise records a contested_paths latch being
	// raised against the sync run that hit the conflict (#158, F27): the
	// freeze that stops the divergent-edit ping-pong. The note carries the
	// volume-relative path. Written once per distinct freeze episode.
	TransitionContestedRaise = "contested-raise"
	// TransitionContestedClear records a contested_paths latch being
	// cleared by an explicit `squirrel conflicts resolve` (operator set),
	// against the run that raised it. The note carries the path. This is
	// the deliberate human act that unfreezes the path.
	TransitionContestedClear = "contested-clear"
	// TransitionPullDurability records an agent-scheduled durability pull
	// against its kind='audit' run (see BeginDurabilityPullRun). The note
	// carries the pulled volume, the peer, and the fetched/applied/dropped/
	// rewind counters — the runs CHECK keeps volume_id and destination NULL
	// on audit rows, so this entry is where the audit trail names them.
	TransitionPullDurability = "pull-durability"
	// TransitionConfigDriftRaise records a config_drift latch being raised
	// against the kind='audit' run that detected it (#191, F9): the config
	// file on disk no longer matches the bytes the agent loaded. The note
	// carries the config path. Written once per drift episode.
	TransitionConfigDriftRaise = "config-drift-raise"
	// TransitionConfigDriftClear records a config_drift latch being cleared,
	// against the run that raised it. The note carries the reason — the
	// agent restarted onto the config on disk, or the file's content came
	// back to what the running agent loaded. No operator: the resolving act
	// is a restart or an edit, not a typed command.
	TransitionConfigDriftClear = "config-drift-clear"
	// TransitionResetDestination records a `squirrel destination reset`:
	// the operator forgetting a destination's recorded upload and
	// durability state (ResetDestination). It shares the destination-scoped
	// kind='audit' run shape, so — like TransitionVerifyDestination — this
	// note is where the audit trail names the reset destination and carries
	// the cleared counts, the runs CHECK keeping destination NULL on the row.
	TransitionResetDestination = "reset-destination"
)

// RunAudit is one row of the insert-only runs_audit log: a single
// lifecycle transition recorded against a run. Operator and Note are
// nullable — machine-driven transitions leave Operator unset, and Note
// carries optional human-readable detail.
type RunAudit struct {
	ID         int64
	RunID      int64
	Transition string
	Operator   sql.NullString
	AtNs       int64
	Note       sql.NullString
}

// RunAuditEntry is the caller-facing input to AppendRunAudit. Operator
// and Note are plain strings; empty maps to SQL NULL so callers don't
// construct sql.NullString values by hand. RunID and Transition are
// required.
type RunAuditEntry struct {
	RunID      int64
	Transition string
	Operator   string
	Note       string
}

// appendRunAuditTx inserts one runs_audit row inside an existing
// transaction. Used by FinishRun so the run-row update and its audit
// line commit atomically — a crash can't leave a finished row without
// its 'finish' audit entry, or vice versa. at_ns is passed in so it
// matches the run row's ended_at_ns exactly rather than drifting by a
// second NowNs() read.
func appendRunAuditTx(ctx context.Context, tx *sql.Tx, e RunAuditEntry, atNs int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO runs_audit (run_id, transition, operator, at_ns, note)
		VALUES (?, ?, ?, ?, ?)
	`, e.RunID, e.Transition, nullString(e.Operator), atNs, nullString(e.Note))
	if err != nil {
		return fmt.Errorf("append runs_audit (%s) for run %d: %w", e.Transition, e.RunID, err)
	}
	return nil
}

// AppendRunAudit inserts one runs_audit row outside any caller-managed
// transaction, stamping at_ns with NowNs(). Used by call sites that
// record a transition independent of a FinishRun (today: the `runs
// fail` CLI's 'manual-fail' line; #77's correlation write).
func (s *Store) AppendRunAudit(ctx context.Context, e RunAuditEntry) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs_audit (run_id, transition, operator, at_ns, note)
		VALUES (?, ?, ?, ?, ?)
	`, e.RunID, e.Transition, nullString(e.Operator), NowNs(), nullString(e.Note))
	if err != nil {
		return fmt.Errorf("append runs_audit (%s) for run %d: %w", e.Transition, e.RunID, err)
	}
	return nil
}

// ListRunAudit returns every runs_audit row for runID, oldest first
// (ascending id, which is insertion order). Provided so tests and any
// future forensic CLI can read the transition log without reaching into
// the table directly; an empty slice means no audited transitions.
func (s *Store) ListRunAudit(ctx context.Context, runID int64) ([]RunAudit, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, transition, operator, at_ns, note
		FROM runs_audit WHERE run_id = ? ORDER BY id
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("list runs_audit for run %d: %w", runID, err)
	}
	defer rows.Close()
	var out []RunAudit
	for rows.Next() {
		var a RunAudit
		if err := rows.Scan(&a.ID, &a.RunID, &a.Transition, &a.Operator, &a.AtNs, &a.Note); err != nil {
			return nil, fmt.Errorf("scan runs_audit row: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// nullString maps "" to a NULL sql.NullString and any other value to a
// valid one. Keeps the audit insert helpers from constructing the
// struct inline at every call.
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

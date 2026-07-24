package store

import (
	"context"
	"fmt"
)

// AlarmKindVerifyMismatch is the destination_alarms.kind for a standing
// alarm raised when `squirrel verify` finds a recorded object or pack whose
// provider checksum no longer matches its upload fingerprint, or that has
// gone missing from the remote (#157, F30). It is the only alarm kind
// today; the column is free text so a future standing condition can share
// the table without a migration.
const AlarmKindVerifyMismatch = "verify-mismatch"

// DestinationAlarm is one row of the destination_alarms latch: an abnormal
// condition on a destination that persists ("latches") until a subsequent
// clean check auto-clears it or an operator acks it. It is derived standing
// state — the permanent forensic record of every raise and clear lives in
// the runs and runs_audit tables — so the live latch can be cleared in
// place without losing history. RaisedRunID references the run that first
// detected the condition; RaisedAtNs is when that first detection landed
// (stable across repeated detections, so the surface can show "in alarm
// since").
type DestinationAlarm struct {
	Destination string
	Kind        string
	Detail      string
	RaisedRunID int64
	RaisedAtNs  int64
}

// RaiseDestinationAlarm latches an alarm on destination when none is
// already active. The raise is idempotent: an existing alarm keeps its
// original raised-at timestamp and run so a repeated detection does not
// reset "in alarm since" or append a duplicate audit row. An 'alarm-raise'
// runs_audit entry is written against raisedRunID (in the same transaction
// as the latch insert) only when a fresh latch is created, keeping the
// append-only trail to one row per distinct alarm episode.
func (s *Store) RaiseDestinationAlarm(ctx context.Context, destination, kind, detail string, raisedRunID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin raise alarm %q: %w", destination, err)
	}
	defer func() { _ = tx.Rollback() }()

	atNs := NowNs()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO destination_alarms (destination, kind, detail, raised_run_id, raised_at_ns)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(destination) DO NOTHING
	`, destination, kind, detail, raisedRunID, atNs)
	if err != nil {
		return fmt.Errorf("raise alarm %q: %w", destination, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("raise alarm %q rows affected: %w", destination, err)
	}
	if inserted == 0 {
		// Already in alarm: keep the original latch untouched.
		return tx.Commit()
	}
	note := fmt.Sprintf("destination=%s %s", destination, detail)
	if err := appendRunAuditTx(ctx, tx,
		RunAuditEntry{RunID: raisedRunID, Transition: TransitionAlarmRaise, Note: note}, atNs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit raise alarm %q: %w", destination, err)
	}
	return nil
}

// ClearDestinationAlarm clears the active alarm on destination, if any,
// and appends an 'alarm-clear' runs_audit entry against auditRunID.
// operator is "" for an automatic clear by a clean verify pass (the
// clearing run is that pass) and the operator name for an explicit ack
// (the clearing run is the one that raised the alarm). Returns whether an
// alarm was active. Deleting the latch loses no history: the raise and
// this clear both survive in runs_audit, and every verify run that
// touched the destination survives in runs.
func (s *Store) ClearDestinationAlarm(ctx context.Context, destination string, auditRunID int64, operator string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin clear alarm %q: %w", destination, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM destination_alarms WHERE destination = ?`, destination)
	if err != nil {
		return false, fmt.Errorf("clear alarm %q: %w", destination, err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("clear alarm %q rows affected: %w", destination, err)
	}
	if deleted == 0 {
		return false, tx.Commit()
	}
	if err := appendRunAuditTx(ctx, tx, RunAuditEntry{
		RunID: auditRunID, Transition: TransitionAlarmClear, Operator: operator,
		Note: "destination=" + destination,
	}, NowNs()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit clear alarm %q: %w", destination, err)
	}
	return true, nil
}

// GetDestinationAlarm returns the active alarm on destination, or
// sql.ErrNoRows (IsNotFound) when the destination is not in alarm.
func (s *Store) GetDestinationAlarm(ctx context.Context, destination string) (DestinationAlarm, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT destination, kind, detail, raised_run_id, raised_at_ns
		 FROM destination_alarms WHERE destination = ?`, destination)
	return scanDestinationAlarm(row.Scan)
}

// ListDestinationAlarms returns every active alarm, destination-sorted so
// the listing is deterministic. An empty slice means nothing is in alarm.
func (s *Store) ListDestinationAlarms(ctx context.Context) ([]DestinationAlarm, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT destination, kind, detail, raised_run_id, raised_at_ns
		 FROM destination_alarms ORDER BY destination`)
	if err != nil {
		return nil, fmt.Errorf("list destination alarms: %w", err)
	}
	defer rows.Close()
	var out []DestinationAlarm
	for rows.Next() {
		a, err := scanDestinationAlarm(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanDestinationAlarm(scan func(...any) error) (DestinationAlarm, error) {
	var a DestinationAlarm
	err := scan(&a.Destination, &a.Kind, &a.Detail, &a.RaisedRunID, &a.RaisedAtNs)
	if err != nil {
		return DestinationAlarm{}, err
	}
	return a, nil
}

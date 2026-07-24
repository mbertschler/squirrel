package store

import (
	"context"
	"database/sql"
	"fmt"
)

// DestinationResetCounts reports how many recorded-state rows a destination
// reset cleared (or, on a dry run, would clear). The four categories are the
// full derived record squirrel keeps about a destination: the per-content
// and per-pack upload ledgers, the live durability vector, and the
// push-freshness maxima. They span every volume — a destination's recorded
// state is not volume-scoped — so the counts are totals across the index.
type DestinationResetCounts struct {
	RemoteObjects    int64
	RemotePacks      int64
	VectorComponents int64
	FreshnessRows    int64
}

// Total is the sum of all four categories, used as the reset run's
// file_count so the audit trail carries the magnitude at a glance.
func (c DestinationResetCounts) Total() int64 {
	return c.RemoteObjects + c.RemotePacks + c.VectorComponents + c.FreshnessRows
}

// Empty reports whether the destination has no recorded state at all — the
// "nothing to reset" case the CLI reports affirmatively rather than minting
// an empty run.
func (c DestinationResetCounts) Empty() bool { return c.Total() == 0 }

// rowQuerier is the subset of *sql.DB and *sql.Tx that
// countDestinationRecordedState needs, so the same count runs read-only on
// the DB (the dry-run preview) and inside the reset transaction (the note
// the run records).
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CountDestinationRecordedState returns how much derived state the index
// holds for destination, without changing anything. It is the read-only
// question behind `squirrel destination reset --dry-run`.
func (s *Store) CountDestinationRecordedState(ctx context.Context, destination string) (DestinationResetCounts, error) {
	if destination == "" {
		return DestinationResetCounts{}, fmt.Errorf("CountDestinationRecordedState: destination must be non-empty")
	}
	return countDestinationRecordedState(ctx, s.db, destination)
}

func countDestinationRecordedState(ctx context.Context, q rowQuerier, destination string) (DestinationResetCounts, error) {
	var c DestinationResetCounts
	counts := []struct {
		dst   *int64
		query string
	}{
		{&c.RemoteObjects, `SELECT COUNT(*) FROM remote_objects WHERE destination = ?`},
		{&c.RemotePacks, `SELECT COUNT(*) FROM remote_packs WHERE destination = ?`},
		{&c.VectorComponents, `SELECT COUNT(*) FROM destination_run_ids WHERE destination = ?`},
		{&c.FreshnessRows, `SELECT COUNT(*) FROM destination_push_freshness WHERE destination = ?`},
	}
	for _, item := range counts {
		if err := q.QueryRowContext(ctx, item.query, destination).Scan(item.dst); err != nil {
			return DestinationResetCounts{}, fmt.Errorf("count destination %q recorded state: %w", destination, err)
		}
	}
	return c, nil
}

// ResetDestination forgets everything the index records about a destination's
// remote state — its per-content and per-pack upload ledgers, its live
// durability vector, and its push-freshness maxima — so the next sync treats
// the destination as fresh and re-uploads. It is the supported recovery for a
// wrecked or repointed destination (friction log F20): before it, a fresh
// `root` still refused because the layout guard keyed on run history by
// destination name, leaving hand SQL or a config-wide rename as the only outs.
//
// The reset is audit-preserving. It never touches the runs table or the
// append-only destination_run_ids_history advance log, so the record of what
// was ever asserted durable survives; only the live derived state is cleared,
// exactly as RevokeDestinationRunIDsFromSource clears a peer's live
// assertions while leaving the history intact. The contents and files rows —
// the content squirrel must never lose track of — are untouched.
//
// The reset is itself recorded as a run: a kind='audit' row (the run kinds
// are fixed by a CHECK, so this reuses the destination-scoped audit shape
// BeginRemoteVerifyRun already uses — volume_id and destination NULL) whose
// runs_audit trail carries a 'reset-destination' note with the destination
// name and the cleared counts. Everything runs in one transaction: on any
// error nothing is cleared and no run is recorded, so a reset is all-or-nothing.
func (s *Store) ResetDestination(ctx context.Context, destination string) (int64, DestinationResetCounts, error) {
	if destination == "" {
		return 0, DestinationResetCounts{}, fmt.Errorf("ResetDestination: destination must be non-empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, DestinationResetCounts{}, fmt.Errorf("begin reset destination %q: %w", destination, err)
	}
	defer func() { _ = tx.Rollback() }()

	counts, err := countDestinationRecordedState(ctx, tx, destination)
	if err != nil {
		return 0, DestinationResetCounts{}, err
	}
	atNs := NowNs()
	runID, err := insertResetRunTx(ctx, tx, atNs)
	if err != nil {
		return 0, DestinationResetCounts{}, err
	}
	if err := deleteDestinationRecordedStateTx(ctx, tx, destination); err != nil {
		return 0, DestinationResetCounts{}, err
	}
	note := fmt.Sprintf("destination=%q objects=%d packs=%d vector=%d freshness=%d",
		destination, counts.RemoteObjects, counts.RemotePacks, counts.VectorComponents, counts.FreshnessRows)
	if err := appendRunAuditTx(ctx, tx,
		RunAuditEntry{RunID: runID, Transition: TransitionResetDestination, Note: note}, atNs); err != nil {
		return 0, DestinationResetCounts{}, err
	}
	if err := finishResetRunTx(ctx, tx, runID, atNs, counts.Total()); err != nil {
		return 0, DestinationResetCounts{}, err
	}
	if err := tx.Commit(); err != nil {
		return 0, DestinationResetCounts{}, fmt.Errorf("commit reset destination %q: %w", destination, err)
	}
	return runID, counts, nil
}

// insertResetRunTx opens the kind='audit' run that records the reset. The
// runs CHECK keeps audit rows' volume_id and destination NULL (the destination
// travels in the 'reset-destination' runs_audit note instead), matching
// BeginRemoteVerifyRun's destination-scoped audit shape.
func insertResetRunTx(ctx context.Context, tx *sql.Tx, atNs int64) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO runs (kind, volume_id, destination, started_at_ns, status, file_count)
		VALUES ('audit', NULL, NULL, ?, 'running', 0)
	`, atNs)
	if err != nil {
		return 0, fmt.Errorf("insert reset run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reset run last insert id: %w", err)
	}
	return id, nil
}

// deleteDestinationRecordedStateTx clears the four derived-state tables for
// destination. The append-only destination_run_ids_history and the runs
// table are deliberately absent — the reset preserves the audit trail.
func deleteDestinationRecordedStateTx(ctx context.Context, tx *sql.Tx, destination string) error {
	stmts := []string{
		`DELETE FROM remote_objects WHERE destination = ?`,
		`DELETE FROM remote_packs WHERE destination = ?`,
		`DELETE FROM destination_run_ids WHERE destination = ?`,
		`DELETE FROM destination_push_freshness WHERE destination = ?`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt, destination); err != nil {
			return fmt.Errorf("clear destination %q recorded state: %w", destination, err)
		}
	}
	return nil
}

// finishResetRunTx moves the reset run to success with the cleared total as
// its file_count and appends the 'finish' runs_audit line, mirroring
// FinishRun so the reset run reads like any other terminal run.
func finishResetRunTx(ctx context.Context, tx *sql.Tx, runID, atNs, total int64) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE runs SET ended_at_ns = ?, status = 'success', file_count = ?
		WHERE id = ?
	`, atNs, total, runID); err != nil {
		return fmt.Errorf("finish reset run %d: %w", runID, err)
	}
	return appendRunAuditTx(ctx, tx,
		RunAuditEntry{RunID: runID, Transition: TransitionFinish, Note: RunStatusSuccess}, atNs)
}

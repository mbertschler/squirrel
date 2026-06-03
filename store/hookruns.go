package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Hook triggers. A hook run is one exec of a user-configured per-volume
// command. 'change' fires after a successful index run that settled
// content; 'interval' fires on a cadence regardless of change. The
// discriminator is passed to the command as SQUIRREL_TRIGGER and recorded
// here so the two trigger histories stay distinguishable in the status
// surface.
const (
	HookTriggerChange   = "change"
	HookTriggerInterval = "interval"
)

// Hook statuses. A hook run begins 'running' and FinishHookRun moves it to
// a terminal state. squirrel records pass/fail only — it never interprets
// what the command did, so there is no 'partial': a timeout or a non-zero
// exit are both 'failed'. The generic exit code carries the nuance.
const (
	HookStatusRunning = "running"
	HookStatusSuccess = "success"
	HookStatusFailed  = "failed"
)

// HookRun is one row in the hook_runs table — the generic outcome of one
// hook invocation. ExitCode is NULL when the process never produced one
// (spawn failure or timeout); Error is NULL on success and carries a
// short diagnostic (stderr tail, "timeout", spawn error) otherwise.
// TriggeringRunID references the index run that fired an on-change hook
// and is NULL for interval hooks (no run triggered them). Changed mirrors
// the SQUIRREL_CHANGED value the hook was passed.
type HookRun struct {
	ID              int64
	VolumeID        int64
	Trigger         string
	TriggeringRunID sql.NullInt64
	Changed         bool
	StartedAtNs     int64
	EndedAtNs       sql.NullInt64
	Status          string
	ExitCode        sql.NullInt64
	Error           sql.NullString
}

// HookRunSpec is the immutable context BeginHookRun records when a hook
// invocation starts. TriggeringRunID is set for on-change hooks and left
// zero (NULL) for interval hooks.
type HookRunSpec struct {
	VolumeID        int64
	Trigger         string
	TriggeringRunID int64
	Changed         bool
}

// hookRunColumns is the fixed projection for every read of a hook_runs
// row, kept in lockstep with scanHookRun's scan order.
const hookRunColumns = `id, volume_id, trigger, triggering_run_id, changed, started_at_ns, ended_at_ns, status, exit_code, error`

func scanHookRun(scan func(...any) error) (HookRun, error) {
	var r HookRun
	err := scan(&r.ID, &r.VolumeID, &r.Trigger, &r.TriggeringRunID, &r.Changed,
		&r.StartedAtNs, &r.EndedAtNs, &r.Status, &r.ExitCode, &r.Error)
	return r, err
}

func scanHookRunRow(s rowScanner) (HookRun, error) {
	return scanHookRun(s.Scan)
}

// BeginHookRun records the start of a hook invocation and returns its id.
// The trigger must be one of the HookTrigger* constants. The row begins
// in status 'running'; FinishHookRun moves it to a terminal state. A
// non-zero spec.TriggeringRunID is stored as the FK; zero stores NULL.
func (s *Store) BeginHookRun(ctx context.Context, spec HookRunSpec) (int64, error) {
	if spec.Trigger != HookTriggerChange && spec.Trigger != HookTriggerInterval {
		return 0, fmt.Errorf("BeginHookRun: trigger must be %q or %q, got %q", HookTriggerChange, HookTriggerInterval, spec.Trigger)
	}
	var triggeringRun sql.NullInt64
	if spec.TriggeringRunID != 0 {
		triggeringRun = sql.NullInt64{Int64: spec.TriggeringRunID, Valid: true}
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO hook_runs (volume_id, trigger, triggering_run_id, changed, started_at_ns, status)
		VALUES (?, ?, ?, ?, ?, 'running')
	`, spec.VolumeID, spec.Trigger, triggeringRun, spec.Changed, NowNs())
	if err != nil {
		return 0, fmt.Errorf("insert hook run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("hook run last insert id: %w", err)
	}
	return id, nil
}

// FinishHookRun records the terminal state of a hook run. exitCode is
// stored as-is (pass an invalid sql.NullInt64 when the process produced
// no code, e.g. spawn failure or timeout); errMsg is stored as NULL when
// empty. Returns an error if id matches no row so a hook is never left
// stuck in 'running'.
func (s *Store) FinishHookRun(ctx context.Context, id int64, status string, exitCode sql.NullInt64, errMsg string) error {
	if status != HookStatusSuccess && status != HookStatusFailed {
		return fmt.Errorf("FinishHookRun: status must be %q or %q, got %q", HookStatusSuccess, HookStatusFailed, status)
	}
	var errVal sql.NullString
	if errMsg != "" {
		errVal = sql.NullString{String: errMsg, Valid: true}
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE hook_runs SET ended_at_ns = ?, status = ?, exit_code = ?, error = ?
		WHERE id = ?
	`, NowNs(), status, exitCode, errVal, id)
	if err != nil {
		return fmt.Errorf("finish hook run %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish hook run %d rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("finish hook run %d: no such hook run", id)
	}
	return nil
}

// HookRunListOpts filters and shapes ListHookRuns. The zero value returns
// every hook run, oldest first, with no cap.
type HookRunListOpts struct {
	// VolumeID, when non-nil, restricts results to hooks for that volume.
	VolumeID *int64
	// Limit caps the result count. Zero (or negative) means no cap.
	Limit int
	// Descending sorts by id descending (most recent first).
	Descending bool
}

// ListHookRuns returns hook runs matching opts. See HookRunListOpts for
// filter and ordering semantics.
func (s *Store) ListHookRuns(ctx context.Context, opts HookRunListOpts) ([]HookRun, error) {
	query := `SELECT ` + hookRunColumns + ` FROM hook_runs`
	var args []any
	if opts.VolumeID != nil {
		query += ` WHERE volume_id = ?`
		args = append(args, *opts.VolumeID)
	}
	if opts.Descending {
		query += ` ORDER BY id DESC`
	} else {
		query += ` ORDER BY id`
	}
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	return queryRows(ctx, s.db, query, scanHookRunRow, args...)
}

// LatestHookRun returns the most recent hook run for the given (volume,
// trigger), or sql.ErrNoRows when none exists. The interval-hook scheduler
// computes `now - last_run` from this row to decide whether a
// cadence-driven check is due; a still-running row counts (its
// started_at_ns anchors the cadence, and the don't-stack guard prevents a
// second concurrent invocation regardless).
//
// Ordering is by started_at_ns so the idx_hook_runs_volume_trigger index
// (volume_id, trigger, started_at_ns) satisfies both the filter and the
// sort — no separate sort pass as hook history grows. id breaks ties
// deterministically and is the index's implicit trailing rowid, so it
// stays index-only.
func (s *Store) LatestHookRun(ctx context.Context, volumeID int64, trigger string) (HookRun, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+hookRunColumns+`
		FROM hook_runs
		WHERE volume_id = ? AND trigger = ?
		ORDER BY started_at_ns DESC, id DESC LIMIT 1
	`, volumeID, trigger)
	return scanHookRun(row.Scan)
}

package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ConfigDriftMessage is the sentence every surface prints for a standing
// config-drift latch, and the `error` text recorded on the run that raised
// it. It lives here so the CLI, the TUI, and the audit trail all say the
// same thing about the same fact.
const ConfigDriftMessage = "config on disk has changed since this agent started; restart to apply"

// Reasons passed to ClearConfigDrift, recorded in the clearing runs_audit
// note. They name what resolved the drift, which is the difference between
// "the operator did the restart the latch asked for" and "the operator
// changed their mind and put the file back".
const (
	// ConfigDriftClearedByRestart is the clear an agent performs at startup:
	// it has just loaded the file on disk, so whatever the previous process
	// was latched about is now applied.
	ConfigDriftClearedByRestart = "agent restarted with the config on disk"
	// ConfigDriftClearedByRevert is the clear a re-check performs when the
	// file's content comes back to the bytes the running agent loaded — the
	// edit was undone, so there is nothing left to apply.
	ConfigDriftClearedByRevert = "config on disk matches the running config again"
)

// ConfigDrift is the standing config-drift latch (#191, friction log F9):
// the agent found the config file carrying different bytes than the ones it
// parsed at startup, so it is running configuration the operator has since
// changed. It stands until the agent is restarted (which applies the edit)
// or the file's content returns to what the agent loaded.
//
// There is at most one row — the latch describes the one loaded config of
// the one agent that owns this index, not any volume or destination. Like
// DestinationAlarm it is derived standing state: the permanent record of
// every raise and clear lives in runs and runs_audit, so clearing the live
// latch loses no history.
type ConfigDrift struct {
	// Path is the config file the running agent loaded.
	Path string
	// LoadedBlake3 is the digest of the bytes that agent parsed and
	// DiskBlake3 the differing digest the re-check read from disk. They are
	// the latch's evidence rather than display material: an operator who
	// wants to know *which* config an agent is holding can compare them
	// against a hash of the file, and a forensic reader can tell "the file
	// changed once" from "it changed, was reverted, and changed again".
	LoadedBlake3 []byte
	DiskBlake3   []byte
	// RaisedRunID is the kind='audit' run recording the detection, and
	// RaisedAtNs when it landed — stable across repeated detections, so a
	// surface can show "changed N ago" rather than "changed just now".
	RaisedRunID int64
	RaisedAtNs  int64
}

// RaiseConfigDrift latches config drift when none is already standing and
// reports whether this call created the latch. The raise is idempotent: an
// existing latch keeps its original run and timestamp, so a re-check every
// cadence tick neither resets "changed since" nor appends a second audit
// episode for one edit.
//
// A newly raised latch writes its own kind='audit' run — automatic work is
// never invisible (ux-principle 5) — in the same transaction as the latch
// insert, so a crash can leave neither a latch without its run nor a run
// without its latch. When the ON CONFLICT finds a latch already standing
// the whole transaction rolls back, which is also what keeps the losing
// call from stranding an orphan run row.
func (s *Store) RaiseConfigDrift(ctx context.Context, path string, loaded, disk []byte) (raised bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin raise config drift: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	atNs := NowNs()
	runID, err := insertConfigDriftRunTx(ctx, tx, atNs)
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO config_drift (id, path, loaded_blake3, disk_blake3, raised_run_id, raised_at_ns)
		VALUES (1, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING
	`, path, loaded, disk, runID, atNs)
	if err != nil {
		return false, fmt.Errorf("raise config drift: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("raise config drift rows affected: %w", err)
	}
	if inserted == 0 {
		// Already latched: leave the original untouched and drop this
		// transaction's run row with it.
		return false, nil
	}
	if err := appendRunAuditTx(ctx, tx, RunAuditEntry{
		RunID: runID, Transition: TransitionConfigDriftRaise, Note: "config=" + path,
	}, atNs); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit raise config drift: %w", err)
	}
	return true, nil
}

// insertConfigDriftRunTx writes the already-terminal kind='audit' run that
// records one drift detection, plus the 'finish' runs_audit line FinishRun
// would have written. The check is a point event — read the file, hash it,
// compare — so the row is never 'running': there is no window in which a
// killed agent could strand it for the startup reaper. 'partial' is the
// status a completed check with a finding takes, matching the verify pass
// that finds a mismatch, and changed_count is 0 because noticing drift
// changes nothing on this machine.
//
// The run carries no volume and no destination, the shape the other
// out-of-band audit checks use (see beginVolumelessAuditRun); the subject
// lands in the run's runs_audit note instead.
func insertConfigDriftRunTx(ctx context.Context, tx *sql.Tx, atNs int64) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO runs (kind, volume_id, destination, started_at_ns, ended_at_ns,
		                  status, error, file_count, changed_count)
		VALUES ('audit', NULL, NULL, ?, ?, ?, ?, 0, 0)
	`, atNs, atNs, RunStatusPartial, ConfigDriftMessage)
	if err != nil {
		return 0, fmt.Errorf("insert config-drift run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("config-drift run last insert id: %w", err)
	}
	if err := appendRunAuditTx(ctx, tx,
		RunAuditEntry{RunID: id, Transition: TransitionFinish, Note: RunStatusPartial}, atNs); err != nil {
		return 0, err
	}
	return id, nil
}

// ClearConfigDrift clears the standing latch, if any, and appends a
// 'config-drift-clear' runs_audit entry against the run that raised it —
// the shape ClearDestinationAlarm uses for an ack, so "this drift was dealt
// with, and how" stays a recoverable fact after the live row is gone.
// reason is one of the ConfigDriftClearedBy* constants and lands in the
// note. Returns whether a latch was standing.
//
// The entry names no operator: unlike `verify ack` or `conflicts resolve`
// there is no command being typed here — the resolving act is the restart
// (or the revert) itself, and the reason records which of the two it was.
func (s *Store) ClearConfigDrift(ctx context.Context, reason string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin clear config drift: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var raisedRunID int64
	switch err := tx.QueryRowContext(ctx,
		`SELECT raised_run_id FROM config_drift WHERE id = 1`).Scan(&raisedRunID); {
	case IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read config drift: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM config_drift WHERE id = 1`); err != nil {
		return false, fmt.Errorf("clear config drift: %w", err)
	}
	if err := appendRunAuditTx(ctx, tx, RunAuditEntry{
		RunID: raisedRunID, Transition: TransitionConfigDriftClear, Note: reason,
	}, NowNs()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit clear config drift: %w", err)
	}
	return true, nil
}

// GetConfigDrift returns the standing config-drift latch, or
// sql.ErrNoRows (IsNotFound) when the running config matches disk.
func (s *Store) GetConfigDrift(ctx context.Context) (ConfigDrift, error) {
	var d ConfigDrift
	err := s.db.QueryRowContext(ctx, `
		SELECT path, loaded_blake3, disk_blake3, raised_run_id, raised_at_ns
		FROM config_drift WHERE id = 1
	`).Scan(&d.Path, &d.LoadedBlake3, &d.DiskBlake3, &d.RaisedRunID, &d.RaisedAtNs)
	if err != nil {
		return ConfigDrift{}, err
	}
	return d, nil
}

package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ConfigDriftMessage is the sentence every surface prints for a standing
// config-drift latch that the agent has not been able to act on at all —
// the only shape the latch had before it could reload (#191), and still
// what an agent without a reloadable config (an embedder, a test) raises.
// It lives here so the CLI, the TUI, and the audit trail all say the same
// thing about the same fact.
const ConfigDriftMessage = "config on disk has changed since this agent started; restart to apply"

// Reasons passed to ClearConfigDrift, recorded in the clearing runs_audit
// note. They name what resolved the drift, which is the difference between
// "the operator did the restart the latch asked for", "the agent applied
// the edit itself", and "the operator changed their mind and put the file
// back".
const (
	// ConfigDriftClearedByRestart is the clear an agent performs at startup:
	// it has just loaded the file on disk, so whatever the previous process
	// was latched about is now applied.
	ConfigDriftClearedByRestart = "agent restarted with the config on disk"
	// ConfigDriftClearedByRevert is the clear a re-check performs when the
	// file's content comes back to the bytes the running agent loaded — the
	// edit was undone, so there is nothing left to apply.
	ConfigDriftClearedByRevert = "config on disk matches the running config again"
	// ConfigDriftClearedByReload is the clear a reload performs when it
	// adopted the whole edit in place (#204): every key that changed was one
	// this process can change while running, so no restart is owed.
	ConfigDriftClearedByReload = "agent reloaded the config on disk"
)

// ConfigDrift is the standing config-drift latch (F9): the agent found the
// config file carrying different bytes than the ones it is running, and
// could not (or could only partly) act on that itself. It stands until the
// agent is restarted, the rest of the edit becomes applicable, or the
// file's content returns to what the agent is running.
//
// There is at most one row — the latch describes the one loaded config of
// the one agent that owns this index, not any volume or destination. Like
// DestinationAlarm it is derived standing state: the permanent record of
// every raise and clear lives in runs and runs_audit, so clearing the live
// latch loses no history.
// The loaded_blake3 / disk_blake3 columns are deliberately not projected
// here. They are the episode's forensic evidence — a reader can tell "the
// file changed once" from "it changed, was reverted, and changed again",
// and can identify which config an agent is holding by comparing them
// against a hash of the file — but that reading is done against the table,
// by a human or `squirrel db`, not through this accessor. Carrying them as
// struct fields no caller reads would be exactly the unused public surface
// AGENTS.md rules out; the columns keep the record either way.
type ConfigDrift struct {
	// Path is the config file the running agent loaded.
	Path string
	// PendingKeys are the config keys whose change needs an agent restart,
	// after the agent applied everything it could in place. Empty means the
	// whole edit is still unapplied — either the agent cannot reload at all,
	// or ApplyError says why this one was refused.
	PendingKeys []string
	// ApplyError is why the edit on disk could not be adopted: the file no
	// longer loads, or the state derived from it could not be rebuilt. Empty
	// on the ordinary paths. When set, the agent is still running the last
	// config it loaded successfully.
	ApplyError string
	// RaisedRunID is the kind='audit' run recording the detection, and
	// RaisedAtNs when it landed — stable across repeated detections, so a
	// surface can show "changed N ago" rather than "changed just now".
	RaisedRunID int64
	RaisedAtNs  int64
}

// ConfigDriftMessageFor renders the one sentence a standing latch is worth,
// given what the agent managed to do about the edit. Every surface — the
// CLI, the TUI, the agent's log line, the run that records the detection —
// goes through here, so they cannot disagree about the same latch.
func ConfigDriftMessageFor(pendingKeys []string, applyError string) string {
	switch {
	case applyError != "":
		return fmt.Sprintf("config on disk has changed but could not be applied (%s); the agent is still running the last config it loaded", applyError)
	case len(pendingKeys) > 0:
		return fmt.Sprintf("config on disk has changed; the agent applied what it could and %s still need a restart",
			strings.Join(pendingKeys, ", "))
	default:
		return ConfigDriftMessage
	}
}

// ConfigDriftState is one detection's full finding: which file, what the
// agent is running, what is on disk, and what is left to do about it. It is
// the argument to RaiseConfigDrift, which is otherwise a five-scalar call
// where two of the scalars are indistinguishable digests.
type ConfigDriftState struct {
	// Path is the config file that was checked.
	Path string
	// Loaded is the digest of the bytes the agent last parsed and adopted,
	// and Disk the digest of the file at the moment of the check. Both
	// config.DigestLen bytes. After a reload that applied the policy half of
	// an edit the two agree, because the agent *is* running the file on
	// disk — what a restart still owes is then carried by PendingKeys, not
	// by the digests.
	Loaded []byte
	Disk   []byte
	// PendingKeys and ApplyError carry the same meaning as on ConfigDrift.
	PendingKeys []string
	ApplyError  string
}

// RaiseConfigDrift latches config drift and reports whether this call began
// a new episode. An episode is the continuous stretch during which the file
// on disk is not the configuration the agent is running; a re-check every
// cadence tick neither resets "changed since" nor appends a second audit
// entry for one edit.
//
// Within a standing episode the *finding* can still change: a file that did
// not parse gets fixed and now only needs a restart for its listener, or a
// second edit adds a key the first did not touch. Those refresh the row's
// detail in place, keeping the original run and timestamp — the drift has
// stood continuously since then, and it is that continuity, not the exact
// bytes, that the operator is being told about.
//
// A newly raised latch writes its own kind='audit' run — automatic work is
// never invisible (ux-principle 5) — in the same transaction as the latch
// insert, so a crash can leave neither a latch without its run nor a run
// without its latch.
func (s *Store) RaiseConfigDrift(ctx context.Context, st ConfigDriftState) (raised bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin raise config drift: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var standing int64
	switch err := tx.QueryRowContext(ctx,
		`SELECT raised_run_id FROM config_drift WHERE id = 1`).Scan(&standing); {
	case IsNotFound(err):
		if err := insertConfigDriftTx(ctx, tx, st); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit raise config drift: %w", err)
		}
		return true, nil
	case err != nil:
		return false, fmt.Errorf("read config drift: %w", err)
	}
	if err := refreshConfigDriftTx(ctx, tx, st); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit refresh config drift: %w", err)
	}
	return false, nil
}

// insertConfigDriftTx opens a new drift episode: the run recording the
// detection, the latch row, and the raise entry against that run.
func insertConfigDriftTx(ctx context.Context, tx *sql.Tx, st ConfigDriftState) error {
	atNs := NowNs()
	runID, err := insertConfigDriftRunTx(ctx, tx, atNs, ConfigDriftMessageFor(st.PendingKeys, st.ApplyError))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO config_drift (id, path, loaded_blake3, disk_blake3,
		                          pending_keys, apply_error, raised_run_id, raised_at_ns)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?)
	`, st.Path, st.Loaded, st.Disk, joinConfigKeys(st.PendingKeys), st.ApplyError, runID, atNs); err != nil {
		return fmt.Errorf("raise config drift: %w", err)
	}
	return appendRunAuditTx(ctx, tx, RunAuditEntry{
		RunID: runID, Transition: TransitionConfigDriftRaise, Note: "config=" + st.Path,
	}, atNs)
}

// refreshConfigDriftTx updates a standing episode's finding without
// disturbing the run and timestamp that anchor it.
func refreshConfigDriftTx(ctx context.Context, tx *sql.Tx, st ConfigDriftState) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE config_drift
		SET path = ?, loaded_blake3 = ?, disk_blake3 = ?, pending_keys = ?, apply_error = ?
		WHERE id = 1
	`, st.Path, st.Loaded, st.Disk, joinConfigKeys(st.PendingKeys), st.ApplyError); err != nil {
		return fmt.Errorf("refresh config drift: %w", err)
	}
	return nil
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
func insertConfigDriftRunTx(ctx context.Context, tx *sql.Tx, atNs int64, message string) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO runs (kind, volume_id, destination, started_at_ns, ended_at_ns,
		                  status, error, file_count, changed_count)
		VALUES ('audit', NULL, NULL, ?, ?, ?, ?, 0, 0)
	`, atNs, atNs, RunStatusPartial, message)
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
// there is no command being typed here — the resolving act is the restart,
// the reload, or the revert itself, and the reason records which.
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
	var (
		d       ConfigDrift
		pending string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT path, pending_keys, apply_error, raised_run_id, raised_at_ns
		FROM config_drift WHERE id = 1
	`).Scan(&d.Path, &pending, &d.ApplyError, &d.RaisedRunID, &d.RaisedAtNs)
	if err != nil {
		return ConfigDrift{}, err
	}
	d.PendingKeys = splitConfigKeys(pending)
	return d, nil
}

// RecordConfigReload writes the kind='audit' run for one applied reload
// (#204): the agent changed its own operating configuration without anyone
// typing anything, and automatic work is never invisible (ux-principle 5).
// applied and pending name the config keys the agent adopted in place and
// the ones that still want a restart; a reload that could adopt everything
// is 'success', one that left keys behind is 'partial' — the same "it
// worked, but not entirely" reading every other partial run has.
//
// Like the drift-detection run this is a point event, written already
// terminal so no reaper can ever find it running.
func (s *Store) RecordConfigReload(ctx context.Context, path string, applied, pending []string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin record config reload: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	status, errText := RunStatusSuccess, ""
	if len(pending) > 0 {
		status = RunStatusPartial
		errText = ConfigDriftMessageFor(pending, "")
	}
	atNs := NowNs()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO runs (kind, volume_id, destination, started_at_ns, ended_at_ns,
		                  status, error, file_count, changed_count)
		VALUES ('audit', NULL, NULL, ?, ?, ?, ?, 0, 0)
	`, atNs, atNs, status, errText)
	if err != nil {
		return 0, fmt.Errorf("insert config-reload run: %w", err)
	}
	runID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("config-reload run last insert id: %w", err)
	}
	note := fmt.Sprintf("config=%s applied=%s pending=%s",
		path, configKeyList(applied), configKeyList(pending))
	if err := appendRunAuditTx(ctx, tx,
		RunAuditEntry{RunID: runID, Transition: TransitionConfigReload, Note: note}, atNs); err != nil {
		return 0, err
	}
	if err := appendRunAuditTx(ctx, tx,
		RunAuditEntry{RunID: runID, Transition: TransitionFinish, Note: status}, atNs); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit record config reload: %w", err)
	}
	return runID, nil
}

// joinConfigKeys and splitConfigKeys move a key list across the single TEXT
// column that holds it. Config keys are dotted identifiers with no commas,
// so the separator can never appear inside one.
func joinConfigKeys(keys []string) string { return strings.Join(keys, ",") }

func splitConfigKeys(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// configKeyList renders a key list for a human-read audit note, naming the
// empty case rather than leaving a dangling `applied=`.
func configKeyList(keys []string) string {
	if len(keys) == 0 {
		return "none"
	}
	return strings.Join(keys, ",")
}

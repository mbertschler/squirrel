package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Run kinds. The runs.volume_id column is nullable so a future sync run can
// span volumes; index runs are always scoped to a single volume. Sync and
// restore runs additionally carry a non-empty runs.destination naming the
// rclone destination; index and audit runs leave destination NULL. Audit
// runs share the index run-kind's shape — they walk a volume root and
// reconcile the index with on-disk reality — but are tagged separately so
// out-of-band drift detections don't dilute the index-run history.
const (
	RunKindIndex   = "index"
	RunKindSync    = "sync"
	RunKindRestore = "restore"
	RunKindAudit   = "audit"
)

// Run statuses. A run begins in 'running' and is moved to a terminal state by
// FinishRun. 'partial' means the walk completed but some files errored.
const (
	RunStatusRunning = "running"
	RunStatusSuccess = "success"
	RunStatusFailed  = "failed"
	RunStatusPartial = "partial"
)

// ErrAlreadyFinished is returned by FinishRun when the target run is
// already in a terminal status (success/partial/failed). The first
// terminal write wins: its status, error, and ended_at_ns are the audit
// record, and a second FinishRun would silently rewrite them. Callers
// that may legitimately race a double-finish (e.g. agent/sync.go's
// handleClose firing after closeSession already finalised) detect this
// with errors.Is and fall back to a log-only path rather than treating
// it as a hard error.
var ErrAlreadyFinished = errors.New("run is already in a terminal status")

// isTerminalStatus reports whether status is one of the three terminal
// run states. A row in any of these must not be re-finalised by
// FinishRun.
func isTerminalStatus(status string) bool {
	switch status {
	case RunStatusSuccess, RunStatusPartial, RunStatusFailed:
		return true
	}
	return false
}

// Run is one entry in the runs table. VolumeID uses sql.NullInt64 because the
// column is nullable (cross-volume sync runs in the future); EndedAtNs and
// Error are likewise nullable while a run is still in-flight or finished
// without an error. Destination is NULL for index runs and required for
// sync/restore runs (enforced by a CHECK in the schema). FileCount is int64
// to match the SQLite INTEGER column. PeerNodeID and CorrelatedRunID are
// non-NULL on peer-sync runs only: the former references the peer's nodes
// row (initiator or receiver, depending on which side wrote the row), the
// latter carries the *other side's* local run id so the two halves of one
// logical sync can be joined offline.
type Run struct {
	ID              int64
	Kind            string
	VolumeID        sql.NullInt64
	Destination     sql.NullString
	StartedAtNs     int64
	EndedAtNs       sql.NullInt64
	Status          string
	Error           sql.NullString
	FileCount       int64
	PeerNodeID      sql.NullInt64
	CorrelatedRunID sql.NullInt64
	// Shallow is true when the run skipped BLAKE3 verification in
	// favour of the (size, mtime) shortcut: a skipped rehash for index
	// and audit runs, an rclone copy without --checksum --hash blake3
	// for initiator-side sync and restore runs. NULL (Valid=false) for
	// the receiver side of a node sync (which makes no such choice) and
	// for the pre-v10 history that never recorded it.
	Shallow sql.NullBool
}

// BeginRun records the start of a sync or restore run and returns its
// id. Callers must pair it with FinishRun (typically via defer with an
// error pointer or an explicit terminal call). volumeID must reference
// an existing volume; destination must be non-empty (sync/restore must
// name an rclone target). shallow records whether the transfer skipped
// BLAKE3 verification (rclone's size+mtime shortcut) so forensic readers
// can tell which restores were content-verified. Index and audit kinds
// belong on BeginIndexRun and are rejected here.
func (s *Store) BeginRun(ctx context.Context, kind string, volumeID int64, destination string, shallow bool) (int64, error) {
	if kind == RunKindIndex || kind == RunKindAudit {
		return 0, fmt.Errorf("BeginRun: kind %q must go through BeginIndexRun so runs.shallow is recorded", kind)
	}
	var destVal sql.NullString
	if destination != "" {
		destVal = sql.NullString{String: destination, Valid: true}
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (kind, volume_id, destination, started_at_ns, status, file_count, shallow)
		VALUES (?, ?, ?, ?, 'running', 0, ?)
	`, kind, volumeID, destVal, NowNs(), shallow)
	if err != nil {
		return 0, fmt.Errorf("insert run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("run last insert id: %w", err)
	}
	return id, nil
}

// BeginIndexRun is BeginRun's sibling for kind='index' or kind='audit'
// rows that additionally records whether the walk ran in shallow mode
// (skip rehash when size/mtime match). Index and audit are the only run
// kinds where the shortcut applies; sync/restore go through BeginRun and
// leave runs.shallow NULL. The kind argument is validated to be one of
// the two index-shaped kinds so this entry point can't silently widen
// shallow's meaning later.
func (s *Store) BeginIndexRun(ctx context.Context, kind string, volumeID int64, shallow bool) (int64, error) {
	if kind != RunKindIndex && kind != RunKindAudit {
		return 0, fmt.Errorf("BeginIndexRun: kind must be %q or %q, got %q", RunKindIndex, RunKindAudit, kind)
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (kind, volume_id, destination, started_at_ns, status, file_count, shallow)
		VALUES (?, ?, NULL, ?, 'running', 0, ?)
	`, kind, volumeID, NowNs(), shallow)
	if err != nil {
		return 0, fmt.Errorf("insert index run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("index run last insert id: %w", err)
	}
	return id, nil
}

// BeginPeerSyncRun is BeginRun's sibling for kind='sync' rows tied to a
// peer node. It records the (peer_node_id, correlated_run_id) pair
// alongside the regular destination name (the peer's name from the
// initiator's config, or its self-name on the receiver). The
// destination column stays populated so the schema CHECK
// (kind='sync' ⇒ destination non-empty) is satisfied and the existing
// `squirrel runs` listing renders sensibly without special-casing.
func (s *Store) BeginPeerSyncRun(ctx context.Context, volumeID, peerNodeID, correlatedRunID int64, destination string) (int64, error) {
	if destination == "" {
		return 0, fmt.Errorf("BeginPeerSyncRun: destination must be non-empty")
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (
			kind, volume_id, destination, started_at_ns, status, file_count,
			peer_node_id, correlated_run_id
		) VALUES ('sync', ?, ?, ?, 'running', 0, ?, ?)
	`, volumeID, destination, NowNs(), peerNodeID, correlatedRunID)
	if err != nil {
		return 0, fmt.Errorf("insert peer-sync run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("peer-sync run last insert id: %w", err)
	}
	return id, nil
}

// SetCorrelatedRunID stamps the supplied correlated id onto an
// already-open run row. Used by the initiator to record the receiver's
// run id once /v1/sync/begin returns: at BeginRun time the receiver
// hadn't yet allocated one. Returns a "no such run" error if runID is
// invalid.
//
// The update and a 'set-correlated-run-id' runs_audit row are written in
// one transaction so the overwrite-in-place correlated_run_id column
// gains an append-only trail of every value it ever held (SAFETY-AUDIT
// H6). The audit note carries the old→new ids.
func (s *Store) SetCorrelatedRunID(ctx context.Context, runID, correlatedRunID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set correlated run id %d: %w", runID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var prior sql.NullInt64
	switch err := tx.QueryRowContext(ctx,
		`SELECT correlated_run_id FROM runs WHERE id = ?`, runID).Scan(&prior); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("set correlated run id: no run with id %d", runID)
	case err != nil:
		return fmt.Errorf("set correlated run id read prior: %w", err)
	}

	atNs := NowNs()
	if _, err := tx.ExecContext(ctx,
		`UPDATE runs SET correlated_run_id = ? WHERE id = ?`,
		correlatedRunID, runID); err != nil {
		return fmt.Errorf("set correlated run id: %w", err)
	}
	note := fmt.Sprintf("%s->%d", nullInt64Label(prior), correlatedRunID)
	if err := appendRunAuditTx(ctx, tx,
		RunAuditEntry{RunID: runID, Transition: TransitionSetCorrelatedRunID, Note: note}, atNs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit set correlated run id %d: %w", runID, err)
	}
	return nil
}

// nullInt64Label renders a nullable int for an audit note: the decimal
// value when set, or the literal "none" when NULL (the pre-correlation
// state). Keeps the old→new note legible without a sql.NullInt64 dump.
func nullInt64Label(v sql.NullInt64) string {
	if !v.Valid {
		return "none"
	}
	return fmt.Sprintf("%d", v.Int64)
}

// FinishRun records the terminal state of a run. errMsg is stored as NULL
// when empty. fileCount should be the total number of files the run touched
// (added + modified + unchanged) so the runs table doubles as a coarse audit
// log without needing to scan files.
//
// The transition is guarded: a row already in a terminal status
// (success/partial/failed) is never re-finalised — the first terminal
// write wins and FinishRun returns ErrAlreadyFinished (matchable via
// errors.Is) without touching the row. This protects the audit trail
// from a double-finish bug or a buggy retry silently rewriting the
// original status, error, and ended-at timestamp. A runID matching no
// row returns a plain "no such run" error so a stuck 'running' row is
// never left behind silently.
//
// The status update and a 'finish' runs_audit row are written in one
// transaction so the append-only transition log can't diverge from the
// run row. The audit note carries the resulting status.
func (s *Store) FinishRun(ctx context.Context, runID int64, status string, errMsg string, fileCount int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin finish run %d: %w", runID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var current string
	switch err := tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ?`, runID).Scan(&current); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("finish run %d: no such run", runID)
	case err != nil:
		return fmt.Errorf("finish run %d read status: %w", runID, err)
	}
	if isTerminalStatus(current) {
		return fmt.Errorf("finish run %d (status %s): %w", runID, current, ErrAlreadyFinished)
	}

	atNs := NowNs()
	var errVal sql.NullString
	if errMsg != "" {
		errVal = sql.NullString{String: errMsg, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE runs SET ended_at_ns = ?, status = ?, error = ?, file_count = ?
		WHERE id = ?
	`, atNs, status, errVal, fileCount, runID); err != nil {
		return fmt.Errorf("finish run %d: %w", runID, err)
	}
	if err := appendRunAuditTx(ctx, tx,
		RunAuditEntry{RunID: runID, Transition: TransitionFinish, Note: status}, atNs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit finish run %d: %w", runID, err)
	}
	return nil
}

// ListRunsOpts filters and shapes the result of ListRuns. The zero value
// returns every run, oldest first, with no cap.
type ListRunsOpts struct {
	// VolumeID, when non-nil, restricts results to runs against that volume.
	// A nil VolumeID returns runs across every volume (including any future
	// cross-volume sync runs, which have a NULL volume_id).
	VolumeID *int64
	// Limit caps the result count. Zero (or negative) means no cap.
	Limit int
	// Descending sorts by id descending (most recent first). Defaults to
	// ascending so callers that walk history get start-order chronological
	// output without thinking about it.
	Descending bool
}

// runColumns is the fixed projection for every read of a runs row. Keeps
// the scan order in lockstep with the query order; adding a column means
// editing one place.
const runColumns = `id, kind, volume_id, destination, started_at_ns, ended_at_ns, status, error, file_count, peer_node_id, correlated_run_id, shallow`

func scanRun(scan func(...any) error) (Run, error) {
	var r Run
	err := scan(&r.ID, &r.Kind, &r.VolumeID, &r.Destination, &r.StartedAtNs, &r.EndedAtNs,
		&r.Status, &r.Error, &r.FileCount, &r.PeerNodeID, &r.CorrelatedRunID, &r.Shallow)
	return r, err
}

// scanRunRow adapts scanRun to the func(rowScanner) (T, error) shape
// queryRows expects, so the runs list-reads share one collection loop.
func scanRunRow(s rowScanner) (Run, error) {
	return scanRun(s.Scan)
}

// ListRuns returns runs matching opts. See ListRunsOpts for filter and
// ordering semantics.
func (s *Store) ListRuns(ctx context.Context, opts ListRunsOpts) ([]Run, error) {
	query := `SELECT ` + runColumns + ` FROM runs`
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
	return queryRows(ctx, s.db, query, scanRunRow, args...)
}

// ListRunsByPeer returns kind='sync' runs whose peer_node_id matches
// peerNodeID, most recent first. limit caps the result count; zero (or
// negative) returns every match. Provided so CLIs that surface per-peer
// run history don't reach into the runs table directly.
func (s *Store) ListRunsByPeer(ctx context.Context, peerNodeID int64, limit int) ([]Run, error) {
	query := `SELECT ` + runColumns + ` FROM runs
	          WHERE kind = 'sync' AND peer_node_id = ?
	          ORDER BY id DESC`
	args := []any{peerNodeID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	return queryRows(ctx, s.db, query, scanRunRow, args...)
}

// GetRun returns the run with the given id, or sql.ErrNoRows.
func (s *Store) GetRun(ctx context.Context, id int64) (Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	return scanRun(row.Scan)
}

// CountFilesFirstSeenByRunWithPathPrefix returns a map from run id to
// the number of files rows that run first-saw under pathPrefix. The
// path prefix is escaped against LIKE metacharacters by the helper —
// callers pass the directory name as plain text (no trailing slash,
// no wildcards). The map only carries entries for runs with non-zero
// matches; absent keys mean zero.
//
// `squirrel runs` uses this to render the CONFLICTS column without a
// per-run query (every conflict inserts one row under
// .squirrel-conflicts/run-<id>/, so the prefix count is the conflict
// count). The pathPrefix is passed in so the store stays decoupled
// from the sync-package directory naming convention.
func (s *Store) CountFilesFirstSeenByRunWithPathPrefix(ctx context.Context, runIDs []int64, pathPrefix string) (map[int64]int, error) {
	out := make(map[int64]int, len(runIDs))
	if len(runIDs) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(runIDs)-1) + "?"
	args := make([]any, 0, len(runIDs)+1)
	for _, id := range runIDs {
		args = append(args, id)
	}
	args = append(args, pathPrefix, escapeLikePrefix(pathPrefix)+"/%")
	rows, err := s.db.QueryContext(ctx,
		`SELECT f.first_seen_run_id, COUNT(*) FROM files f
		 JOIN folders fo ON fo.id = f.folder_id
		 WHERE f.first_seen_run_id IN (`+placeholders+`)
		   AND (fo.path = ? OR fo.path LIKE ? ESCAPE '\')
		 GROUP BY f.first_seen_run_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("count files by run: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("scan count row: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// escapeLikePrefix escapes %, _ and \ in s so it can be embedded into a
// SQL LIKE pattern without matching wildcards in the supplied prefix.
// The companion ESCAPE '\' clause on the query consumes the escapes.
func escapeLikePrefix(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' || c == '_' || c == '\\' {
			b = append(b, '\\')
		}
		b = append(b, c)
	}
	return string(b)
}

// ListAuditRunsSince returns every kind='audit' run on the given volume
// whose started_at_ns is strictly greater than sinceNs, oldest first.
// Used by the peer-sync handshake to surface "drift since last sync" to
// initiators: an empty watermark (sinceNs == 0) returns every audit run
// on the volume; a populated one narrows to the period since the
// receiver and peer last agreed.
func (s *Store) ListAuditRunsSince(ctx context.Context, volumeID int64, sinceNs int64) ([]Run, error) {
	return queryRows(ctx, s.db,
		`SELECT `+runColumns+` FROM runs
		 WHERE kind = 'audit' AND volume_id = ? AND started_at_ns > ?
		 ORDER BY id`,
		scanRunRow, volumeID, sinceNs)
}

// CountModifiedFilesByRun returns the number of files rows whose
// first_seen_run_id is the given runID and that have a prior superseded
// row at the same (folder_id, name). This is the "the bytes changed
// during this run" count, derivable without an audit-specific schema
// column because the supersede chain already records the prior content.
// Returns 0 when no such rows exist (a clean audit). New content at a
// path never seen before (no prior superseded row) is *not* counted —
// that is an addition, not a modification.
func (s *Store) CountModifiedFilesByRun(ctx context.Context, runID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files f
		 WHERE f.first_seen_run_id = ? AND f.status = 'present'
		   AND EXISTS (
		       SELECT 1 FROM files p
		       WHERE p.folder_id = f.folder_id AND p.name = f.name
		         AND p.status = 'superseded'
		   )`, runID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count modified by run %d: %w", runID, err)
	}
	return n, nil
}

// CountMissingFilesByRun returns the number of files rows whose status
// is 'missing' and whose last_seen_run_id equals runID — i.e. the rows
// MarkMissing flipped to missing during runID. Relies on the contract
// that MarkMissing stamps last_seen_run_id on the flip; together with
// CountModifiedFilesByRun it gives the "modified + missing" pair the
// drift-detection handshake (#17) surfaces.
func (s *Store) CountMissingFilesByRun(ctx context.Context, runID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files
		 WHERE status = 'missing' AND last_seen_run_id = ?`, runID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count missing by run %d: %w", runID, err)
	}
	return n, nil
}

// SyncRunSpec captures the columns BeginSyncRunIfClear writes onto a
// kind='sync' row. PeerNodeID and CorrelatedRunID are zero values
// (sql.NullInt64{Valid: false}) for bucket syncs and carry the peer
// linkage for node syncs. Destination is exact-match against the same
// string the guard query checks (bucket destination name, or peer node
// name from the initiator's config). Shallow records whether the
// transfer skipped BLAKE3 verification (rclone's size+mtime shortcut)
// so forensic readers can tell which syncs were content-verified.
type SyncRunSpec struct {
	VolumeID        int64
	Destination     string
	PeerNodeID      sql.NullInt64
	CorrelatedRunID sql.NullInt64
	Shallow         bool
}

// BeginSyncRunIfClear atomically inserts a 'running' kind='sync' row for
// (volume, destination) iff no other such row is currently in flight.
// The check and the insert run inside a single BEGIN IMMEDIATE
// transaction (the store's DSN sets `_txlock=immediate`), so two
// concurrent callers cannot both observe "no running run" and both
// insert — the second one's transaction sees the first one's row and
// returns it as the blocker.
//
// Returns (newID, nil, nil) when the row was inserted; (0, &blocker,
// nil) when refused — the caller is expected to render a diagnostic
// using the blocker's id and started_at_ns. Stale rows from crashed
// runs keep blocking here until cleared via `runs fail` (#37); the
// agent's scheduler (#39) uses the same call so CLI and scheduler
// share one gate.
func (s *Store) BeginSyncRunIfClear(ctx context.Context, spec SyncRunSpec) (int64, *Run, error) {
	if spec.Destination == "" {
		return 0, nil, fmt.Errorf("BeginSyncRunIfClear: destination must be non-empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("begin sync-run tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
		SELECT `+runColumns+`
		FROM runs
		WHERE kind = 'sync' AND status = 'running'
		  AND volume_id = ? AND destination = ?
		ORDER BY id LIMIT 1
	`, spec.VolumeID, spec.Destination)
	blocker, scanErr := scanRun(row.Scan)
	if scanErr == nil {
		return 0, &blocker, nil
	}
	if !errors.Is(scanErr, sql.ErrNoRows) {
		return 0, nil, fmt.Errorf("check running sync: %w", scanErr)
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO runs (
			kind, volume_id, destination, started_at_ns, status, file_count,
			peer_node_id, correlated_run_id, shallow
		) VALUES ('sync', ?, ?, ?, 'running', 0, ?, ?, ?)
	`, spec.VolumeID, spec.Destination, NowNs(), spec.PeerNodeID, spec.CorrelatedRunID, spec.Shallow)
	if err != nil {
		return 0, nil, fmt.Errorf("insert sync run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, nil, fmt.Errorf("sync run last insert id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("commit sync run: %w", err)
	}
	return id, nil, nil
}

// BeginIndexRunIfClear atomically inserts a 'running' kind='index' or
// kind='audit' row for volumeID iff no other index- or audit-kind run
// is currently in flight against the same volume. Symmetric to
// BeginSyncRunIfClear (BEGIN IMMEDIATE + check + insert in one tx) so
// two concurrent callers cannot both observe "no running run" and both
// insert. Cross-kind: an in-flight 'index' blocks a new 'audit' and
// vice versa because both walk the volume and call MarkMissing with
// their own run-id — letting them overlap is exactly the bug this
// guards against.
//
// Returns (newID, nil, nil) when the row was inserted; (0, &blocker,
// nil) when refused. Stale rows from crashed runs keep blocking here
// until cleared via `runs fail` (#37), same recovery story as sync.
func (s *Store) BeginIndexRunIfClear(ctx context.Context, kind string, volumeID int64, shallow bool) (int64, *Run, error) {
	if kind != RunKindIndex && kind != RunKindAudit {
		return 0, nil, fmt.Errorf("BeginIndexRunIfClear: kind must be %q or %q, got %q", RunKindIndex, RunKindAudit, kind)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("begin index-run tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
		SELECT `+runColumns+`
		FROM runs
		WHERE kind IN ('index', 'audit') AND status = 'running'
		  AND volume_id = ?
		ORDER BY id LIMIT 1
	`, volumeID)
	blocker, scanErr := scanRun(row.Scan)
	if scanErr == nil {
		return 0, &blocker, nil
	}
	if !errors.Is(scanErr, sql.ErrNoRows) {
		return 0, nil, fmt.Errorf("check running index/audit: %w", scanErr)
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO runs (kind, volume_id, destination, started_at_ns, status, file_count, shallow)
		VALUES (?, ?, NULL, ?, 'running', 0, ?)
	`, kind, volumeID, NowNs(), shallow)
	if err != nil {
		return 0, nil, fmt.Errorf("insert index run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, nil, fmt.Errorf("index run last insert id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("commit index run: %w", err)
	}
	return id, nil, nil
}

// LatestSuccessfulIndexRun returns the most recent index run for the given
// volume that finished in status 'success' or 'partial'. Used by the sync
// command as a prerequisite check: refusing to sync a volume that has never
// been indexed protects the user from pushing stale or untracked state.
// Returns sql.ErrNoRows when no such run exists.
func (s *Store) LatestSuccessfulIndexRun(ctx context.Context, volumeID int64) (Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+runColumns+`
		FROM runs
		WHERE kind = 'index' AND volume_id = ?
		  AND status IN ('success','partial')
		ORDER BY id DESC LIMIT 1
	`, volumeID)
	return scanRun(row.Scan)
}

// LatestFinishedRun returns the most recent terminal-status (success,
// partial, or failed) run of the given kind for (volumeID, destination).
// destination must be "" for index/audit kinds (the schema CHECK ensures
// those carry no destination) and the rclone destination or peer node
// name for sync/restore kinds. Returns sql.ErrNoRows when no matching
// run exists.
//
// The scheduler (#39) computes `now - last_finished` from this row to
// decide whether a cadence-driven run is due. Failed terminal states
// count: per the issue's failure-policy (no special retry, the next
// tick re-evaluates), a failed run consumes the cadence window like
// any other.
func (s *Store) LatestFinishedRun(ctx context.Context, kind string, volumeID int64, destination string) (Run, error) {
	var row *sql.Row
	if destination == "" {
		row = s.db.QueryRowContext(ctx,
			`SELECT `+runColumns+`
			 FROM runs
			 WHERE kind = ? AND volume_id = ? AND destination IS NULL
			   AND status IN ('success','partial','failed')
			 ORDER BY id DESC LIMIT 1`,
			kind, volumeID)
	} else {
		row = s.db.QueryRowContext(ctx,
			`SELECT `+runColumns+`
			 FROM runs
			 WHERE kind = ? AND volume_id = ? AND destination = ?
			   AND status IN ('success','partial','failed')
			 ORDER BY id DESC LIMIT 1`,
			kind, volumeID, destination)
	}
	return scanRun(row.Scan)
}

// LatestSuccessfulRunsByVolumeAndKind returns the most recent success or
// partial run for each (volume_id, kind) pair in one SQL pass, as a
// nested map keyed first by volume id and then by run kind. Used by the
// TUI's Dashboard and Volumes screens to compute "last index" / "last
// sync" without scanning a bounded recent-runs window — that approach
// silently returned "—" for volumes whose last successful run had fallen
// out of the window on long-lived installs.
//
// Runs with NULL volume_id (today's cross-volume sync placeholder) are
// excluded; cross-volume runs don't belong to any single volume row.
// Sync runs are de-duplicated across destinations: a volume's "last sync"
// is the latest successful sync to any of its destinations, which is
// what dashboards generally mean by the term.
func (s *Store) LatestSuccessfulRunsByVolumeAndKind(ctx context.Context) (map[int64]map[string]Run, error) {
	// Correlated subquery picks the max id per (volume_id, kind) among
	// success/partial rows. SQLite handles the NULL semantics correctly:
	// rows with NULL volume_id never match the correlation predicate, so
	// they are excluded from the outer result without an extra clause.
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+runColumns+`
		FROM runs r1
		WHERE r1.status IN ('success','partial')
		  AND r1.volume_id IS NOT NULL
		  AND r1.id = (
		    SELECT MAX(r2.id) FROM runs r2
		    WHERE r2.volume_id = r1.volume_id
		      AND r2.kind = r1.kind
		      AND r2.status IN ('success','partial')
		  )
	`)
	if err != nil {
		return nil, fmt.Errorf("latest successful runs: %w", err)
	}
	defer rows.Close()
	out := map[int64]map[string]Run{}
	for rows.Next() {
		r, err := scanRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		if !r.VolumeID.Valid {
			continue
		}
		byKind, ok := out[r.VolumeID.Int64]
		if !ok {
			byKind = map[string]Run{}
			out[r.VolumeID.Int64] = byKind
		}
		byKind[r.Kind] = r
	}
	return out, rows.Err()
}

// HasRunningRun reports whether any 'running' run of the given kind
// exists for (volumeID, destination). destination "" matches the
// SQL NULL column (the schema constraint for index/audit kinds); a
// non-empty destination is matched exactly.
//
// The scheduler calls this before kicking a new run so a stale
// 'running' row (from a crashed prior run, cleared via `runs fail`)
// or a concurrent CLI invocation produces a clean skip log rather
// than racing into a duplicate. Implemented via SELECT EXISTS so
// SQLite short-circuits at the first match rather than walking every
// matching row (COUNT(*) semantics).
func (s *Store) HasRunningRun(ctx context.Context, kind string, volumeID int64, destination string) (bool, error) {
	var found bool
	var err error
	if destination == "" {
		err = s.db.QueryRowContext(ctx,
			`SELECT EXISTS (
			   SELECT 1 FROM runs
			   WHERE kind = ? AND volume_id = ? AND destination IS NULL
			     AND status = 'running'
			 )`,
			kind, volumeID).Scan(&found)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT EXISTS (
			   SELECT 1 FROM runs
			   WHERE kind = ? AND volume_id = ? AND destination = ?
			     AND status = 'running'
			 )`,
			kind, volumeID, destination).Scan(&found)
	}
	if err != nil {
		return false, fmt.Errorf("check running run: %w", err)
	}
	return found, nil
}

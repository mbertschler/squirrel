package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"sort"
	"strings"
)

// FileRow is a single indexed file. Path is the file's volume-relative path
// — reconstructed from the underlying (folder_id, name) storage on every
// read. VolumeID references volumes(id). FirstSeenRunID is the run that
// first inserted this row and is never overwritten on subsequent updates;
// LastSeenRunID advances on every observation. SourceNodeID and SourceRunID
// record provenance for peer-syncs (NULL means "local write" — today's only
// path). They are populated on read for inspection; writes go through Upsert
// with an explicit *Provenance.
type FileRow struct {
	VolumeID       int64
	Path           string
	Blake3         []byte // raw 32-byte BLAKE3-256 digest
	SizeBytes      int64
	MtimeNs        int64
	Status         string
	FirstSeenRunID int64
	LastSeenRunID  int64
	IndexedAtNs    int64
	SourceNodeID   sql.NullInt64
	SourceRunID    sql.NullInt64
}

// Provenance carries the "who wrote this row" attribution that Upsert
// records on a peer-sourced write. NodeID references nodes(id) and RunID
// references runs(id) on the receiver's side. A nil *Provenance to Upsert
// records NULLs, the convention for local writes (today's only path).
type Provenance struct {
	NodeID int64
	RunID  int64
}

// FileWithVolume bundles a file row with its volume, returned by read APIs
// that need to render full filesystem paths to the user.
type FileWithVolume struct {
	File   FileRow
	Volume Volume
}

const (
	StatusPresent    = "present"
	StatusMissing    = "missing"
	StatusSuperseded = "superseded"
)

// fileSelectColumns is the projection used by every files read. The path
// column is reconstructed from the joined folders row so callers see the
// same FileRow shape as in v7 even though storage is keyed off
// (folder_id, name). Pair every new SELECT with this list and the
// fileFromJoin clause below so columns stay in lockstep with scanDests.
const fileSelectColumns = `fo.volume_id, ` + pathFromFolderAndName + `, f.blake3, f.size_bytes, f.mtime_ns, f.status, f.first_seen_run_id, f.last_seen_run_id, f.indexed_at_ns, f.source_node_id, f.source_run_id`

// fileFromJoin is the FROM clause every file read uses. files is the inner
// table; folders is joined for volume_id + path reconstruction.
const fileFromJoin = `files f JOIN folders fo ON fo.id = f.folder_id`

// joinedColumns extends fileSelectColumns with the volume row for SELECTs
// that pre-resolve the user-facing filesystem path. The volumes JOIN is
// added in the FROM clause at each call site.
const joinedColumns = fileSelectColumns + `, v.id, v.name, v.path`

// rowScanner abstracts over *sql.Row and *sql.Rows so scanFrom can serve
// both single-row and multi-row reads.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanFrom reads the next row into r in fileSelectColumns order. Pair every
// new SELECT with this method instead of hand-rolling another field-by-field
// list.
func (r *FileRow) scanFrom(s rowScanner) error {
	return s.Scan(r.scanDests()...)
}

// scanDests returns the per-column destination pointers in fileSelectColumns
// order. scanFrom delegates here; collectJoined appends the volume pointers
// on top so files-half scanning still has one source of truth.
func (r *FileRow) scanDests() []any {
	return []any{
		&r.VolumeID, &r.Path, &r.Blake3, &r.SizeBytes, &r.MtimeNs,
		&r.Status, &r.FirstSeenRunID, &r.LastSeenRunID, &r.IndexedAtNs,
		&r.SourceNodeID, &r.SourceRunID,
	}
}

// GetByPath returns the currently-live row for (volumeID, relPath) — i.e.
// the row with status='present' or status='missing'. Superseded rows
// (historical content at this path) are skipped; use ListHistoryByPath to
// see them. Returns sql.ErrNoRows when no live row exists for the path.
//
// The path-level invariant (at most one non-superseded row per
// (folder_id, name)) is enforced by Upsert and the uniq_files_live_per_path
// index, so this query is unambiguous despite touching a non-unique key.
func (s *Store) GetByPath(ctx context.Context, volumeID int64, relPath string) (FileRow, error) {
	folderPath, name := splitFilePath(relPath)
	row := s.db.QueryRowContext(ctx,
		`SELECT `+fileSelectColumns+` FROM `+fileFromJoin+`
		 WHERE fo.volume_id = ? AND fo.path = ? AND f.name = ? AND f.status != 'superseded'`,
		volumeID, folderPath, name)
	var r FileRow
	err := r.scanFrom(row)
	return r, err
}

// ListHistoryByPath returns every row ever recorded at (volumeID, relPath),
// ordered by first_seen_run_id ascending (i.e. the order in which each
// distinct content was first observed at this path). Useful for inspecting
// the content history of a path. Note: the live row is *not* guaranteed to
// be last — after a content revert (A → B → A), row A is reused and stays
// at its original first_seen position even though it is now live again.
// Filter on Status to find the live row, or use GetByPath.
func (s *Store) ListHistoryByPath(ctx context.Context, volumeID int64, relPath string) ([]FileRow, error) {
	folderPath, name := splitFilePath(relPath)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fileSelectColumns+` FROM `+fileFromJoin+`
		 WHERE fo.volume_id = ? AND fo.path = ? AND f.name = ?
		 ORDER BY f.first_seen_run_id`,
		volumeID, folderPath, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileRow
	for rows.Next() {
		var r FileRow
		if err := r.scanFrom(rows); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LoadVolumeIndex returns every non-superseded row in the volume keyed by
// volume-relative path. The indexer uses this to replace the per-file
// GetByPath round-trip on its hot path: with MaxOpenConns=1, every worker
// that calls GetByPath funnels through the same database/sql connection,
// so the lookups serialise even though they are read-only. Loading the
// whole index once amortises that cost across the entire run and lets
// workers proceed without contending on the DB.
//
// The returned map reflects the database state at the moment of the
// SELECT. Callers that interleave LoadVolumeIndex with concurrent writes
// (sync, audit) get a snapshot that can go stale — the indexer is fine
// with this because every write goes through ApplyIndexBatch in the same
// process and only the indexer's own observations matter for the run.
func (s *Store) LoadVolumeIndex(ctx context.Context, volumeID int64) (map[string]FileRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fileSelectColumns+` FROM `+fileFromJoin+`
		 WHERE fo.volume_id = ? AND f.status != 'superseded'`,
		volumeID)
	if err != nil {
		return nil, fmt.Errorf("load volume index: %w", err)
	}
	defer rows.Close()
	out := make(map[string]FileRow)
	for rows.Next() {
		var r FileRow
		if err := r.scanFrom(rows); err != nil {
			return nil, fmt.Errorf("scan volume index row: %w", err)
		}
		out[r.Path] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// GetByAbsolutePath finds the file whose volume.path + '/' + file.path equals
// abs. Resolution is done by longest-prefix match against the known volumes.
// Used by `squirrel query <path>` when the caller does not know the volume.
func (s *Store) GetByAbsolutePath(ctx context.Context, abs string) (FileWithVolume, error) {
	vols, err := s.ListVolumes(ctx)
	if err != nil {
		return FileWithVolume{}, err
	}
	// Longest path first so the most specific volume wins.
	sort.Slice(vols, func(i, j int) bool { return len(vols[i].Path) > len(vols[j].Path) })
	for _, v := range vols {
		rel, ok := relPathUnder(v.Path, abs)
		if !ok {
			continue
		}
		f, err := s.GetByPath(ctx, v.ID, rel)
		if err == nil {
			return FileWithVolume{File: f, Volume: v}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return FileWithVolume{}, err
		}
	}
	return FileWithVolume{}, sql.ErrNoRows
}

// relPathUnder returns the path of abs relative to base, with ok=false when
// abs is not equal to or under base. base is expected to be an absolute path
// without a trailing slash. The separator is hard-coded to '/' because the
// rest of the codebase stores file paths via filepath.ToSlash; Windows is
// not currently supported.
func relPathUnder(base, abs string) (string, bool) {
	if abs == base {
		return ".", true
	}
	prefix := base + "/"
	if !strings.HasPrefix(abs, prefix) {
		return "", false
	}
	return abs[len(prefix):], true
}

// GetPresentByBlake3InVolume returns the first present row whose
// blake3 digest matches in the given volume, or sql.ErrNoRows when no
// such row exists. The result is used by the sync planner to satisfy
// a CopyFromExisting disposition: when the receiver already holds the
// requested content at some other path in the same volume, the bytes
// can be materialised locally instead of crossing the network.
//
// Rows under the receiver-owned reserved subtrees
// (`.squirrel-history/`, `.squirrel-conflicts/`) are excluded: a
// conflict-preservation row carrying a prior blake3 is reachable for
// historical lookup but must not be elevated back into a live user
// path via dedup. The store layer enforces this so every caller
// inherits the policy without restating it.
//
// Ordering by reconstructed path makes the choice of source
// deterministic across runs even when several paths share the same
// blake3, which keeps audit trails predictable.
func (s *Store) GetPresentByBlake3InVolume(ctx context.Context, volumeID int64, digest []byte) (FileRow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+fileSelectColumns+` FROM `+fileFromJoin+`
		 WHERE f.blake3 = ? AND fo.volume_id = ? AND f.status = 'present'
		   AND fo.path != '.squirrel-history'   AND fo.path NOT LIKE '.squirrel-history/%'
		   AND fo.path != '.squirrel-conflicts' AND fo.path NOT LIKE '.squirrel-conflicts/%'
		 ORDER BY `+pathFromFolderAndName+` LIMIT 1`,
		digest, volumeID)
	var r FileRow
	err := r.scanFrom(row)
	return r, err
}

// GetByBlake3 returns all rows matching the given BLAKE3 digest (raw 32 bytes),
// joined with their volume.
func (s *Store) GetByBlake3(ctx context.Context, digest []byte) ([]FileWithVolume, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+joinedColumns+`
		FROM `+fileFromJoin+` JOIN volumes v ON v.id = fo.volume_id
		WHERE f.blake3 = ?
		ORDER BY v.name, `+pathFromFolderAndName+`
	`, digest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectJoined(rows)
}

// Upsert records an observation of content at a path. It is the only
// supported write path for the files table because it enforces the
// "never overwrite a hash" rule: blake3 on an existing row is immutable.
//
// There are three cases, all handled atomically in a single transaction:
//
//  1. A row with the exact (folder_id, name, blake3) already exists and is
//     the live row — update its mutable fields (touch / restore from
//     missing). first_seen_run_id is preserved.
//  2. A row with the exact (folder_id, name, blake3) exists but is
//     superseded (content has reverted to a previously-seen value) — flip
//     the currently-live row at this path to 'superseded' and revive the
//     matched row to the requested status (first_seen_run_id preserved).
//  3. No row exists at (folder_id, name, blake3) — flip the currently-live
//     row at (folder_id, name), if any, to 'superseded' and insert the new
//     row.
//
// In all cases, blake3 is never rewritten in place; content history at a
// path grows append-only. After the row write succeeds, the affected
// folder's shallow + deep hashes and every ancestor's deep hash are
// recomputed inside the same transaction so the folder Merkle stays
// consistent with the live file set (#44).
//
// prov carries the per-write provenance recorded as (source_node_id,
// source_run_id) on the affected row. A nil *Provenance records NULLs —
// "local write", today's only path. The provenance reflects the current
// observation: cases 1 and 2 rewrite the live row's source columns to the
// new prov (the previous attribution is preserved on the superseded
// row in case 2).
func (s *Store) Upsert(ctx context.Context, r FileRow, prov *Provenance) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin upsert: %w", err)
	}
	defer tx.Rollback()

	if _, err := upsertInTx(ctx, tx, r, prov); err != nil {
		return err
	}
	return tx.Commit()
}

// upsertInTx is the body of Upsert without its own transaction. Returns the
// folder_id of the affected row.
func upsertInTx(ctx context.Context, tx *sql.Tx, r FileRow, prov *Provenance) (int64, error) {
	folderID, err := upsertRowInTx(ctx, tx, r, prov)
	if err != nil {
		return 0, err
	}
	if err := recomputeFolderAndAncestors(ctx, tx, folderID, r.LastSeenRunID); err != nil {
		return 0, err
	}
	return folderID, nil
}

// upsertRowInTx applies the Upsert row-level state machine without the
// folder Merkle recompute that single-row Upsert appends afterwards.
// Batched callers (ApplyIndexBatch) use this so the recompute can be
// deduplicated across many ops on overlapping folders / ancestors.
// Returns the folder_id touched by this op so the batched caller can
// accumulate the set of affected folders.
func upsertRowInTx(ctx context.Context, tx *sql.Tx, r FileRow, prov *Provenance) (int64, error) {
	folderPath, name := splitFilePath(r.Path)
	folderID, err := getOrCreateFolderTx(ctx, tx, r.VolumeID, folderPath)
	if err != nil {
		return 0, err
	}

	var existingStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM files WHERE folder_id = ? AND name = ? AND blake3 = ?`,
		folderID, name, r.Blake3).Scan(&existingStatus)

	switch {
	case err == nil && existingStatus != StatusSuperseded:
		// Case 1: exact row exists and is live — touch it.
		if err := updateLiveRow(ctx, tx, folderID, name, r, prov); err != nil {
			return 0, err
		}
	case err == nil && existingStatus == StatusSuperseded:
		// Case 2: content revert — supersede whatever is live now, then
		// revive the matched (formerly superseded) row.
		if err := supersedeLiveRow(ctx, tx, folderID, name); err != nil {
			return 0, err
		}
		if err := updateLiveRow(ctx, tx, folderID, name, r, prov); err != nil {
			return 0, err
		}
	case errors.Is(err, sql.ErrNoRows):
		// Case 3: brand new content at this path (possibly first-ever).
		if err := supersedeLiveRow(ctx, tx, folderID, name); err != nil {
			return 0, err
		}
		if err := insertNewRow(ctx, tx, folderID, name, r, prov); err != nil {
			return 0, err
		}
	default:
		return 0, fmt.Errorf("lookup existing: %w", err)
	}
	return folderID, nil
}

// provColumns returns the (source_node_id, source_run_id) pair as
// sql.NullInt64 so callers can splat them into UPDATE/INSERT bind lists.
// A nil *Provenance yields two invalid NullInt64 — the binding renders as
// NULL columns, the "local write" convention.
func provColumns(p *Provenance) (sql.NullInt64, sql.NullInt64) {
	if p == nil {
		return sql.NullInt64{}, sql.NullInt64{}
	}
	return sql.NullInt64{Int64: p.NodeID, Valid: true},
		sql.NullInt64{Int64: p.RunID, Valid: true}
}

// supersedeLiveRow flips the single non-superseded row at (folderID, name)
// (if any) to status='superseded'. A no-op when there is no live row, e.g.
// the very first observation of a path. last_seen_run_id stays frozen at the
// value it had — that is the run during which the row was last seen alive.
func supersedeLiveRow(ctx context.Context, tx *sql.Tx, folderID int64, name string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE files SET status = 'superseded'
		WHERE folder_id = ? AND name = ? AND status != 'superseded'
	`, folderID, name)
	if err != nil {
		return fmt.Errorf("supersede live row: %w", err)
	}
	return nil
}

// RecordConflictPreStage atomically supersedes the live row at
// originalPath and inserts a new 'present' row at conflictRow.Path
// carrying the prior blake3 and the supplied provenance. The two
// updates run inside one transaction so an agent crash between them
// rolls both back rather than leaving the receiver in a state where
// the prior content is reachable only by path or only by hash.
//
// The on-disk rename that moves the bytes from originalPath to
// conflictRow.Path is NOT part of this transaction (the filesystem
// doesn't share the DB's journal). The contract the caller honours
// is "mv first, then record": a crash before this returns leaves
// the bytes at the conflict path with both index rows still in their
// pre-call state, so the next sync re-plans, sees the same conflict,
// and pre-stages again — content is preserved through re-runs.
//
// Folder hashes on both the original and conflict paths' folders (and
// every ancestor they share) are recomputed inside the same transaction
// so the Merkle stays consistent with the new live set.
func (s *Store) RecordConflictPreStage(ctx context.Context, volumeID int64, originalPath string, conflictRow FileRow, prov *Provenance) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin conflict pre-stage: %w", err)
	}
	defer tx.Rollback()

	origFolderPath, origName := splitFilePath(originalPath)
	origFolderID, err := getOrCreateFolderTx(ctx, tx, volumeID, origFolderPath)
	if err != nil {
		return err
	}
	if err := supersedeLiveRow(ctx, tx, origFolderID, origName); err != nil {
		return err
	}

	conflictFolderPath, conflictName := splitFilePath(conflictRow.Path)
	conflictFolderID, err := getOrCreateFolderTx(ctx, tx, conflictRow.VolumeID, conflictFolderPath)
	if err != nil {
		return err
	}
	if err := insertNewRow(ctx, tx, conflictFolderID, conflictName, conflictRow, prov); err != nil {
		return err
	}

	if err := recomputeFolderAndAncestors(ctx, tx, origFolderID, conflictRow.LastSeenRunID); err != nil {
		return err
	}
	if conflictFolderID != origFolderID {
		if err := recomputeFolderAndAncestors(ctx, tx, conflictFolderID, conflictRow.LastSeenRunID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// updateLiveRow refreshes the mutable fields on an existing row matching
// (folder_id, name, blake3). blake3 and first_seen_run_id are never touched.
// The (source_node_id, source_run_id) provenance pair is rewritten to the
// caller-supplied prov so the row tracks the most recent attribution.
func updateLiveRow(ctx context.Context, tx *sql.Tx, folderID int64, name string, r FileRow, prov *Provenance) error {
	srcNode, srcRun := provColumns(prov)
	_, err := tx.ExecContext(ctx, `
		UPDATE files SET
			size_bytes = ?, mtime_ns = ?, status = ?,
			last_seen_run_id = ?, indexed_at_ns = ?,
			source_node_id = ?, source_run_id = ?
		WHERE folder_id = ? AND name = ? AND blake3 = ?
	`, r.SizeBytes, r.MtimeNs, r.Status, r.LastSeenRunID, r.IndexedAtNs,
		srcNode, srcRun,
		folderID, name, r.Blake3)
	if err != nil {
		return fmt.Errorf("update live row: %w", err)
	}
	return nil
}

func insertNewRow(ctx context.Context, tx *sql.Tx, folderID int64, name string, r FileRow, prov *Provenance) error {
	srcNode, srcRun := provColumns(prov)
	_, err := tx.ExecContext(ctx,
		`INSERT INTO files (folder_id, name, blake3, size_bytes, mtime_ns,
			status, first_seen_run_id, last_seen_run_id, indexed_at_ns,
			source_node_id, source_run_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		folderID, name, r.Blake3, r.SizeBytes, r.MtimeNs,
		r.Status, r.FirstSeenRunID, r.LastSeenRunID, r.IndexedAtNs,
		srcNode, srcRun)
	if err != nil {
		return fmt.Errorf("insert new row: %w", err)
	}
	return nil
}

// TouchSeen sets last_seen_run_id on the live row at (volumeID, relPath) and
// flips its status to 'present'. Used by the indexer when it re-observed the
// exact same content as the stored row (kindUnchanged). first_seen_run_id is
// never modified. Does not touch superseded rows.
//
// Re-flipping a 'missing' row to 'present' changes the live set, so this
// also recomputes the folder hashes + ancestor chain inside the same
// transaction. The common "still present" case is also recomputed
// unconditionally — the cost is bounded by tree depth and keeps the
// behaviour predictable without a stale-hash window.
func (s *Store) TouchSeen(ctx context.Context, volumeID int64, relPath string, runID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin touch seen: %w", err)
	}
	defer tx.Rollback()
	if _, err := touchSeenInTx(ctx, tx, volumeID, relPath, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// touchSeenInTx is the body of TouchSeen without its own transaction.
// Returns the affected folder_id (0 when no live row was found, i.e. the
// path's folder doesn't exist in the DB) so batched callers can dedupe
// Merkle recomputes.
func touchSeenInTx(ctx context.Context, tx *sql.Tx, volumeID int64, relPath string, runID int64) (int64, error) {
	folderID, err := touchSeenRowInTx(ctx, tx, volumeID, relPath, runID)
	if err != nil || folderID == 0 {
		return folderID, err
	}
	if err := recomputeFolderAndAncestors(ctx, tx, folderID, runID); err != nil {
		return 0, err
	}
	return folderID, nil
}

// touchSeenRowInTx updates the row but skips the folder Merkle recompute,
// for the same reason upsertRowInTx splits it out: batched callers fold
// many ops' worth of recompute into one deduped pass.
func touchSeenRowInTx(ctx context.Context, tx *sql.Tx, volumeID int64, relPath string, runID int64) (int64, error) {
	folderPath, name := splitFilePath(relPath)
	var folderID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM folders WHERE volume_id = ? AND path = ?`,
		volumeID, folderPath).Scan(&folderID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("lookup folder: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE files SET last_seen_run_id = ?, status = 'present'
		 WHERE folder_id = ? AND name = ? AND status != 'superseded'`,
		runID, folderID, name); err != nil {
		return 0, fmt.Errorf("touch seen: %w", err)
	}
	return folderID, nil
}

// IndexBatchOpKind selects which write operation an IndexBatchEntry represents.
type IndexBatchOpKind int

const (
	// BatchOpTouchSeen records that content at Row.Path in volume Row.VolumeID
	// was re-observed unchanged. Only VolumeID, Path, and LastSeenRunID are
	// read off the row; the rest is ignored.
	BatchOpTouchSeen IndexBatchOpKind = iota
	// BatchOpUpsert records a new or modified file. All FileRow fields are
	// used, and Prov is forwarded as the provenance.
	BatchOpUpsert
)

// IndexBatchEntry is one row-level operation queued for a batched apply.
type IndexBatchEntry struct {
	Kind IndexBatchOpKind
	Row  FileRow
	Prov *Provenance
}

// ApplyIndexBatch runs every entry inside a single transaction, amortising
// BeginTx/Commit/fsync across many file observations and deduping the
// folder Merkle recompute across them. After the row work is done, the set
// of affected leaf folders is expanded to include every ancestor up to the
// volume root, the union is topologically ordered (deepest first), and
// each folder's shallow + deep hashes are recomputed exactly once.
//
// The per-op semantics match the single-row methods (Upsert / TouchSeen)
// one-for-one. The deduped recompute is observationally equivalent to N
// independent per-op recomputes because every per-op walk would have re-
// derived the same shallow/deep values from the same final-state files
// and child-folders rows.
//
// Errors abort the batch: the transaction rolls back, so partial writes are
// never visible. The returned error names the index of the failing entry to
// aid debugging.
func (s *Store) ApplyIndexBatch(ctx context.Context, entries []IndexBatchEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin index batch: %w", err)
	}
	defer tx.Rollback()

	leafFolders := make(map[int64]struct{})
	var runID int64 // pinned to the run id of the ops in this batch
	for i, e := range entries {
		var folderID int64
		switch e.Kind {
		case BatchOpTouchSeen:
			folderID, err = touchSeenRowInTx(ctx, tx, e.Row.VolumeID, e.Row.Path, e.Row.LastSeenRunID)
			if err != nil {
				return fmt.Errorf("batch op %d (touch_seen %s): %w", i, e.Row.Path, err)
			}
		case BatchOpUpsert:
			folderID, err = upsertRowInTx(ctx, tx, e.Row, e.Prov)
			if err != nil {
				return fmt.Errorf("batch op %d (upsert %s): %w", i, e.Row.Path, err)
			}
		default:
			return fmt.Errorf("batch op %d: unknown kind %d", i, e.Kind)
		}
		if folderID != 0 {
			leafFolders[folderID] = struct{}{}
		}
		// Every entry in one indexer batch shares a run id (set by the
		// indexer's batchEntry), so the last non-zero value wins and any
		// folder hash writes are stamped with it.
		if e.Row.LastSeenRunID != 0 {
			runID = e.Row.LastSeenRunID
		}
	}

	if err := recomputeFoldersClosure(ctx, tx, leafFolders, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkMissing flips every row in the given volume that was not touched by the
// given run (last_seen_run_id != currentRunID) and is currently 'present' to
// 'missing', stamping last_seen_run_id with the current run as part of the
// flip. The stamp captures the audit run that *observed* the absence so
// drift surfacing (#17) can count "files newly missing during run N" via
// CountMissingFilesByRun without any audit-specific schema column.
//
// The caller is responsible for only invoking this after the run has fully
// scanned the volume: any path the run failed to visit (per-file error,
// context cancellation, fatal walk failure) will look "missing" to this
// query even when it still exists on disk. The indexer enforces this by
// skipping MarkMissing whenever report.Errors > 0 or the walk returned an
// error.
//
// Folders that lost a live file have their hashes recomputed (and the
// ancestor chain re-folded) inside the same transaction so the Merkle is
// consistent with the new live set.
func (s *Store) MarkMissing(ctx context.Context, volumeID int64, currentRunID int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin mark missing: %w", err)
	}
	defer tx.Rollback()

	// Snapshot affected folders before the UPDATE so we can recompute
	// just the touched subtrees. A subquery against the post-UPDATE state
	// would re-find the same set, but capturing it explicitly keeps the
	// dependency in the SQL obvious.
	affRows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT f.folder_id FROM files f
		JOIN folders fo ON fo.id = f.folder_id
		WHERE fo.volume_id = ? AND f.status = 'present' AND f.last_seen_run_id != ?
	`, volumeID, currentRunID)
	if err != nil {
		return 0, fmt.Errorf("scan affected folders: %w", err)
	}
	var affected []int64
	for affRows.Next() {
		var id int64
		if err := affRows.Scan(&id); err != nil {
			affRows.Close()
			return 0, err
		}
		affected = append(affected, id)
	}
	if err := affRows.Err(); err != nil {
		affRows.Close()
		return 0, err
	}
	affRows.Close()

	res, err := tx.ExecContext(ctx, `
		UPDATE files SET status = 'missing', last_seen_run_id = ?
		WHERE status = 'present' AND folder_id IN (
			SELECT id FROM folders WHERE volume_id = ?
		) AND last_seen_run_id != ?
	`, currentRunID, volumeID, currentRunID)
	if err != nil {
		return 0, fmt.Errorf("mark missing: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	for _, id := range affected {
		if err := recomputeFolderAndAncestors(ctx, tx, id, currentRunID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// ListDuplicates returns rows whose blake3 digest appears at more than one
// (volume_id, path), joined with their volume.
func (s *Store) ListDuplicates(ctx context.Context) ([]FileWithVolume, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+joinedColumns+`
		FROM `+fileFromJoin+` JOIN volumes v ON v.id = fo.volume_id
		WHERE f.blake3 IN (
			SELECT blake3 FROM files WHERE status = 'present'
			GROUP BY blake3 HAVING COUNT(*) > 1
		)
		AND f.status = 'present'
		ORDER BY f.blake3, v.name, `+pathFromFolderAndName+`
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectJoined(rows)
}

// ListPresentPathsUnder returns the set of relative paths with status='present'
// within the given volume.
func (s *Store) ListPresentPathsUnder(ctx context.Context, volumeID int64) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+pathFromFolderAndName+` FROM `+fileFromJoin+`
		 WHERE f.status = 'present' AND fo.volume_id = ?`, volumeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = struct{}{}
	}
	return out, rows.Err()
}

// ListPresentFilesInFolder returns every present file row in one
// folder, ordered by name ascending so the caller's downstream
// processing is deterministic. Used by the Merkle walk's initiator
// to assemble the /plan slice from only the folders the walk found
// differing — orders of magnitude smaller than the full-volume
// listing the flat protocol sends.
func (s *Store) ListPresentFilesInFolder(ctx context.Context, folderID int64) ([]FileRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fileSelectColumns+` FROM `+fileFromJoin+`
		 WHERE f.folder_id = ? AND f.status = 'present'
		 ORDER BY f.name`,
		folderID)
	if err != nil {
		return nil, fmt.Errorf("list files in folder %d: %w", folderID, err)
	}
	defer rows.Close()
	var out []FileRow
	for rows.Next() {
		var r FileRow
		if err := r.scanFrom(rows); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListPresentBySource yields every present row in volumeID whose
// source_node_id matches nodeID. A valid nodeID matches that node id
// and exploits idx_files_source_node (the partial index on
// (source_node_id) WHERE status='present' AND source_node_id IS NOT
// NULL); a zero NullInt64 filters to rows with source_node_id IS NULL —
// the "local write" convention — and falls back to a status-scoped scan
// because the partial index excludes those rows by construction.
//
// Yielded in path order so a caller streaming to `rclone --files-from`
// produces a stable, diffable listing. iter.Seq2 is used so large
// volumes don't materialise the whole row set in memory before the
// caller starts consuming it.
func (s *Store) ListPresentBySource(ctx context.Context, volumeID int64, nodeID sql.NullInt64) iter.Seq2[FileRow, error] {
	return func(yield func(FileRow, error) bool) {
		var (
			rows *sql.Rows
			err  error
		)
		if nodeID.Valid {
			rows, err = s.db.QueryContext(ctx,
				`SELECT `+fileSelectColumns+` FROM `+fileFromJoin+`
				 WHERE fo.volume_id = ? AND f.status = 'present' AND f.source_node_id = ?
				 ORDER BY `+pathFromFolderAndName,
				volumeID, nodeID.Int64)
		} else {
			rows, err = s.db.QueryContext(ctx,
				`SELECT `+fileSelectColumns+` FROM `+fileFromJoin+`
				 WHERE fo.volume_id = ? AND f.status = 'present' AND f.source_node_id IS NULL
				 ORDER BY `+pathFromFolderAndName,
				volumeID)
		}
		if err != nil {
			yield(FileRow{}, fmt.Errorf("query present by source: %w", err))
			return
		}
		defer rows.Close()
		for rows.Next() {
			var r FileRow
			if err := r.scanFrom(rows); err != nil {
				yield(FileRow{}, fmt.Errorf("scan present by source: %w", err))
				return
			}
			if !yield(r, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(FileRow{}, err)
		}
	}
}

// ListMissing returns all rows with status='missing', joined with their volume.
func (s *Store) ListMissing(ctx context.Context) ([]FileWithVolume, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+joinedColumns+`
		FROM `+fileFromJoin+` JOIN volumes v ON v.id = fo.volume_id
		WHERE f.status = 'missing'
		ORDER BY v.name, `+pathFromFolderAndName+`
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectJoined(rows)
}

// collectJoined drains a SELECT that pulls joinedColumns into a slice of
// FileWithVolume. The file half routes through FileRow.scanDests so the
// file-column order stays defined in a single place; the volume pointers
// are appended after.
func collectJoined(rows *sql.Rows) ([]FileWithVolume, error) {
	var out []FileWithVolume
	for rows.Next() {
		var fv FileWithVolume
		dests := append(fv.File.scanDests(), &fv.Volume.ID, &fv.Volume.Name, &fv.Volume.Path)
		if err := rows.Scan(dests...); err != nil {
			return nil, err
		}
		out = append(out, fv)
	}
	return out, rows.Err()
}

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

// FileRow is a single indexed file: one path↔content observation joined
// with its contents row. Path is the file's volume-relative path —
// reconstructed from the underlying (folder_id, name) storage on every
// read. VolumeID references volumes(id). ContentID, Blake3, SizeBytes,
// OriginNodeID, and OriginRunID come from the joined contents row;
// origin is where the bytes first entered the system (NULL means
// "introduced locally"), with OriginRunID in the origin node's run
// space. FirstSeenRunID is the run that first inserted this row and is
// never overwritten on subsequent updates; LastSeenRunID advances on
// every observation. Writes go through Upsert, which resolves Blake3 +
// SizeBytes to a contents row (creating one, with the supplied
// *Provenance as its origin, on first contact) and ignores ContentID.
type FileRow struct {
	VolumeID           int64
	Path               string
	ContentID          int64
	Blake3             []byte // raw 32-byte BLAKE3-256 digest
	SizeBytes          int64
	MtimeNs            int64
	Status             string
	FirstSeenRunID     int64
	LastSeenRunID      int64
	IndexedAtNs        int64
	OriginNodeID       sql.NullInt64
	OriginRunID        sql.NullInt64
	StatusChangedRunID sql.NullInt64
}

// Provenance carries the "where did this content first enter the
// system" attribution that Upsert records as (origin_node_id,
// origin_run_id) when a write creates a new contents row. NodeID
// references nodes(id); RunID is in the origin node's run space — the
// run at which that node introduced the content — so it is not a local
// runs FK. The pair is propagated verbatim across peer hops, never
// relabelled to the immediate sender. A nil *Provenance records NULLs,
// the convention for locally introduced content. Content that already
// has a contents row keeps its recorded origin — origin is content-
// level first-introduction provenance, not per-observation attribution.
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

// File statuses. 'present' and 'missing' describe whether the live
// content was found on disk; 'offloaded' is the intentional sibling of
// 'missing' — the content is the path's current content but its local
// bytes were deliberately removed after being secured elsewhere, so
// indexing and audit treat the on-disk absence as expected. 'superseded'
// rows are the append-only content history of a path.
const (
	StatusPresent    = "present"
	StatusMissing    = "missing"
	StatusSuperseded = "superseded"
	StatusOffloaded  = "offloaded"
)

// fileSelectColumns is the projection used by every files read. The path
// column is reconstructed from the joined folders row, and the content
// columns (blake3, size, origin) from the joined contents row, so callers
// see one flat FileRow even though storage is split. Pair every new
// SELECT with this list and the fileFromJoin clause below so columns stay
// in lockstep with scanDests.
const fileSelectColumns = `fo.volume_id, ` + pathFromFolderAndName + `, f.content_id, c.blake3, c.size_bytes, f.mtime_ns, f.status, f.first_seen_run_id, f.last_seen_run_id, f.indexed_at_ns, c.origin_node_id, c.origin_run_id, f.status_changed_run_id`

// fileFromJoin is the FROM clause every file read uses. files is the inner
// table; folders is joined for volume_id + path reconstruction and
// contents for the content columns.
const fileFromJoin = `files f JOIN folders fo ON fo.id = f.folder_id JOIN contents c ON c.id = f.content_id`

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
// order. scanFrom delegates here; scanFileWithVolume appends the volume
// pointers on top so files-half scanning still has one source of truth.
func (r *FileRow) scanDests() []any {
	return []any{
		&r.VolumeID, &r.Path, &r.ContentID, &r.Blake3, &r.SizeBytes, &r.MtimeNs,
		&r.Status, &r.FirstSeenRunID, &r.LastSeenRunID, &r.IndexedAtNs,
		&r.OriginNodeID, &r.OriginRunID, &r.StatusChangedRunID,
	}
}

// scanFileRow scans one row into a fresh FileRow in fileSelectColumns
// order. It adapts FileRow.scanFrom to the func(rowScanner) (T, error)
// shape queryRows expects so every files list-read shares one loop.
func scanFileRow(s rowScanner) (FileRow, error) {
	var r FileRow
	err := r.scanFrom(s)
	return r, err
}

// queryRows runs query with args and collects every row into a slice,
// scanning each through scan. It centralises the
// QueryContext/Next/Close/Err loop that every list-returning read would
// otherwise repeat; scan receives the live *sql.Rows (as a rowScanner)
// and returns one populated T. Errors are returned unwrapped — the SQL
// error already names the failure, and callers that want more context
// wrap the result.
func queryRows[T any](ctx context.Context, db *sql.DB, query string, scan func(rowScanner) (T, error), args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetByPath returns the currently-live row for (volumeID, relPath) — i.e.
// the row with status 'present', 'missing', or 'offloaded'. Superseded
// rows (historical content at this path) are skipped; use
// ListHistoryByPath to see them. Returns sql.ErrNoRows when no live row
// exists for the path.
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
	return queryRows(ctx, s.db,
		`SELECT `+fileSelectColumns+` FROM `+fileFromJoin+`
		 WHERE fo.volume_id = ? AND fo.path = ? AND f.name = ?
		 ORDER BY f.first_seen_run_id`,
		scanFileRow, volumeID, folderPath, name)
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
// (`.squirrel-history/`, `.squirrel-conflicts/`,
// `.squirrel-restore-history/`) are excluded: a
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
		 WHERE c.blake3 = ? AND fo.volume_id = ? AND f.status = 'present'
		   AND fo.path != '.squirrel-history'   AND fo.path NOT LIKE '.squirrel-history/%'
		   AND fo.path != '.squirrel-conflicts' AND fo.path NOT LIKE '.squirrel-conflicts/%'
		   AND fo.path != '.squirrel-restore-history' AND fo.path NOT LIKE '.squirrel-restore-history/%'
		 ORDER BY `+pathFromFolderAndName+` LIMIT 1`,
		digest, volumeID)
	var r FileRow
	err := r.scanFrom(row)
	return r, err
}

// ContentIntroductionRunID returns the earliest first_seen_run_id among
// every files row (any status) observing contentID in volumeID — the
// local run at which the content was introduced to the volume. This is
// the origin-run coordinate the peer-sync sender materialises for
// locally-introduced content (contents.origin_* NULL). Returns
// sql.ErrNoRows when the content has never been observed in the volume.
func (s *Store) ContentIntroductionRunID(ctx context.Context, volumeID, contentID int64) (int64, error) {
	var run sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MIN(f.first_seen_run_id) FROM files f
		 JOIN folders fo ON fo.id = f.folder_id
		 WHERE fo.volume_id = ? AND f.content_id = ?`,
		volumeID, contentID).Scan(&run)
	if err != nil {
		return 0, fmt.Errorf("content introduction run: %w", err)
	}
	if !run.Valid {
		return 0, sql.ErrNoRows
	}
	return run.Int64, nil
}

// GetByBlake3 returns all rows matching the given BLAKE3 digest (raw 32 bytes),
// joined with their volume.
func (s *Store) GetByBlake3(ctx context.Context, digest []byte) ([]FileWithVolume, error) {
	return queryRows(ctx, s.db, `
		SELECT `+joinedColumns+`
		FROM `+fileFromJoin+` JOIN volumes v ON v.id = fo.volume_id
		WHERE c.blake3 = ?
		ORDER BY v.name, `+pathFromFolderAndName+`
	`, scanFileWithVolume, digest)
}

// Upsert records an observation of content at a path. It is the only
// supported write path for the files table because it enforces the
// "never overwrite a hash" rule: a row's content_id is immutable.
//
// The row's Blake3 + SizeBytes are first resolved to a contents row,
// creating one when the hash has never been seen (with prov as its
// origin). Then there are three cases, all handled atomically in a
// single transaction:
//
//  1. A row with the exact (folder_id, name, content_id) already exists
//     and is the live row — update its mutable fields (touch / restore
//     from missing or offloaded). first_seen_run_id is preserved.
//  2. A row with the exact (folder_id, name, content_id) exists but is
//     superseded (content has reverted to a previously-seen value) — flip
//     the currently-live row at this path to 'superseded' and revive the
//     matched row to the requested status (first_seen_run_id preserved).
//  3. No row exists at (folder_id, name, content_id) — flip the
//     currently-live row at (folder_id, name), if any, to 'superseded'
//     and insert the new row.
//
// In all cases, content_id is never rewritten in place; content history
// at a path grows append-only. After the row write succeeds, the affected
// folder's shallow + deep hashes and every ancestor's deep hash are
// recomputed inside the same transaction so the folder Merkle stays
// consistent with the live file set (#44).
//
// prov is recorded as the new contents row's (origin_node_id,
// origin_run_id) when this write introduces the content; a nil
// *Provenance records NULLs — "introduced locally". Content that already
// has a contents row keeps its recorded origin.
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
	contentID, err := getOrCreateContentTx(ctx, tx, r.Blake3, r.SizeBytes, prov)
	if err != nil {
		return 0, err
	}

	var existingStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM files WHERE folder_id = ? AND name = ? AND content_id = ?`,
		folderID, name, contentID).Scan(&existingStatus)

	switch {
	case err == nil && existingStatus != StatusSuperseded:
		// Case 1: exact row exists and is live — touch it.
		if err := updateLiveRow(ctx, tx, folderID, name, contentID, r); err != nil {
			return 0, err
		}
	case err == nil && existingStatus == StatusSuperseded:
		// Case 2: content revert — supersede whatever is live now, then
		// revive the matched (formerly superseded) row.
		if err := supersedeLiveRow(ctx, tx, folderID, name, r.LastSeenRunID); err != nil {
			return 0, err
		}
		if err := updateLiveRow(ctx, tx, folderID, name, contentID, r); err != nil {
			return 0, err
		}
	case errors.Is(err, sql.ErrNoRows):
		// Case 3: brand new content at this path (possibly first-ever).
		if err := supersedeLiveRow(ctx, tx, folderID, name, r.LastSeenRunID); err != nil {
			return 0, err
		}
		if err := insertNewRow(ctx, tx, folderID, name, contentID, r); err != nil {
			return 0, err
		}
	default:
		return 0, fmt.Errorf("lookup existing: %w", err)
	}
	return folderID, nil
}

// provColumns returns the (origin_node_id, origin_run_id) pair as
// sql.NullInt64 so callers can splat them into INSERT bind lists. A nil
// *Provenance yields two invalid NullInt64 — the binding renders as NULL
// columns, the "introduced locally" convention.
func provColumns(p *Provenance) (sql.NullInt64, sql.NullInt64) {
	if p == nil {
		return sql.NullInt64{}, sql.NullInt64{}
	}
	return sql.NullInt64{Int64: p.NodeID, Valid: true},
		sql.NullInt64{Int64: p.RunID, Valid: true}
}

// getOrCreateContentTx resolves a blake3 digest to its contents row id,
// inserting the row on first contact with this content. The insert
// records sizeBytes and the supplied provenance as the content's origin;
// a digest that already has a row keeps its stored size and origin (the
// contents table is append-only and rows are immutable). A stored size
// that disagrees with sizeBytes surfaces as an error.
func getOrCreateContentTx(ctx context.Context, tx *sql.Tx, digest []byte, sizeBytes int64, prov *Provenance) (int64, error) {
	id, err := lookupContentTx(ctx, tx, digest, sizeBytes)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	originNode, originRun := provColumns(prov)
	res, err := tx.ExecContext(ctx,
		`INSERT INTO contents (blake3, size_bytes, origin_node_id, origin_run_id)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(blake3) DO NOTHING`,
		digest, sizeBytes, originNode, originRun)
	if err != nil {
		return 0, fmt.Errorf("insert content: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("insert content rows: %w", err)
	}
	if n == 1 {
		id, err = res.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("content last insert id: %w", err)
		}
		return id, nil
	}
	// The digest landed via a concurrent writer between lookup and insert.
	id, err = lookupContentTx(ctx, tx, digest, sizeBytes)
	if err != nil {
		return 0, fmt.Errorf("re-lookup content after conflict: %w", err)
	}
	return id, nil
}

// lookupContentTx returns the contents row id for digest. A stored
// size_bytes that disagrees with sizeBytes means index corruption or a
// mis-hashing caller, so it surfaces loudly instead of returning the row.
func lookupContentTx(ctx context.Context, tx *sql.Tx, digest []byte, sizeBytes int64) (int64, error) {
	var id, storedSize int64
	err := tx.QueryRowContext(ctx,
		`SELECT id, size_bytes FROM contents WHERE blake3 = ?`, digest).Scan(&id, &storedSize)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if err != nil {
		return 0, fmt.Errorf("lookup content: %w", err)
	}
	if storedSize != sizeBytes {
		return 0, fmt.Errorf("content %x: stored size %d disagrees with observed size %d", digest, storedSize, sizeBytes)
	}
	return id, nil
}

// supersedeLiveRow flips the single non-superseded row at (folderID, name)
// (if any) to status='superseded', stamping runID as the row's
// status-change run. A no-op when there is no live row, e.g. the very
// first observation of a path. last_seen_run_id stays frozen at the
// value it had — that is the run during which the row was last seen alive.
func supersedeLiveRow(ctx context.Context, tx *sql.Tx, folderID int64, name string, runID int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE files SET status = 'superseded', status_changed_run_id = ?
		WHERE folder_id = ? AND name = ? AND status != 'superseded'
	`, runID, folderID, name)
	if err != nil {
		return fmt.Errorf("supersede live row: %w", err)
	}
	return nil
}

// RecordConflictPreStage atomically supersedes the live row at
// originalPath and inserts a new 'present' row at conflictRow.Path
// carrying the prior content. prov becomes the content's origin only
// when the conflict carries bytes never seen before (an out-of-band
// drift); known content keeps its recorded origin. The two updates run
// inside one transaction so an agent crash between them rolls both back
// rather than leaving the receiver in a state where the prior content
// is reachable only by path or only by hash.
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
	if err := supersedeLiveRow(ctx, tx, origFolderID, origName, conflictRow.LastSeenRunID); err != nil {
		return err
	}

	conflictFolderPath, conflictName := splitFilePath(conflictRow.Path)
	conflictFolderID, err := getOrCreateFolderTx(ctx, tx, conflictRow.VolumeID, conflictFolderPath)
	if err != nil {
		return err
	}
	conflictContentID, err := getOrCreateContentTx(ctx, tx, conflictRow.Blake3, conflictRow.SizeBytes, prov)
	if err != nil {
		return err
	}
	if err := insertNewRow(ctx, tx, conflictFolderID, conflictName, conflictContentID, conflictRow); err != nil {
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
// (folder_id, name, content_id). content_id and first_seen_run_id are
// never touched. status_changed_run_id advances exactly when the write
// changes the row's status (the CASE reads the pre-update status, per
// SQL UPDATE semantics), covering the revive transitions Case 2 routes
// through here.
func updateLiveRow(ctx context.Context, tx *sql.Tx, folderID int64, name string, contentID int64, r FileRow) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE files SET
			mtime_ns = ?,
			status_changed_run_id = CASE WHEN status = ? THEN status_changed_run_id ELSE ? END,
			status = ?,
			last_seen_run_id = ?, indexed_at_ns = ?
		WHERE folder_id = ? AND name = ? AND content_id = ?
	`, r.MtimeNs, r.Status, r.LastSeenRunID, r.Status, r.LastSeenRunID, r.IndexedAtNs,
		folderID, name, contentID)
	if err != nil {
		return fmt.Errorf("update live row: %w", err)
	}
	return nil
}

func insertNewRow(ctx context.Context, tx *sql.Tx, folderID int64, name string, contentID int64, r FileRow) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO files (folder_id, name, content_id, mtime_ns,
			status, first_seen_run_id, last_seen_run_id, indexed_at_ns,
			status_changed_run_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		folderID, name, contentID, r.MtimeNs,
		r.Status, r.FirstSeenRunID, r.LastSeenRunID, r.IndexedAtNs,
		r.FirstSeenRunID)
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
// Returns the folder_id resolved for the path (0 only when the path's
// folder row itself is absent) so batched callers can dedupe Merkle
// recomputes. A folder is returned even if the file-row UPDATE matched
// zero rows: the row may have been superseded or missing, but the
// folder still exists and may need its hashes refreshed against the
// current live set.
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
		`UPDATE files SET last_seen_run_id = ?,
			status_changed_run_id = CASE WHEN status = 'present' THEN status_changed_run_id ELSE ? END,
			status = 'present'
		 WHERE folder_id = ? AND name = ? AND status != 'superseded'`,
		runID, runID, folderID, name); err != nil {
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
// runID stamps last_changed_run_id on every folder hash write made by the
// closure recompute, and the caller is expected to populate each entry's
// FileRow.LastSeenRunID with the same value so the SQL row writes carry
// it too.
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
func (s *Store) ApplyIndexBatch(ctx context.Context, runID int64, entries []IndexBatchEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin index batch: %w", err)
	}
	defer tx.Rollback()

	leafFolders := make(map[int64]struct{})
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
	}

	if err := recomputeFoldersClosure(ctx, tx, leafFolders, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkMissing flips every row in the given volume that was not touched by the
// given run (last_seen_run_id != currentRunID) and is currently 'present' to
// 'missing', stamping last_seen_run_id with the current run as part of the
// flip. Only 'present' rows are eligible: an 'offloaded' row's on-disk
// absence is intentional, so it keeps its status and stays out of the
// drift counts. The stamp captures the audit run that *observed* the
// absence so drift surfacing (#17) can count "files newly missing during
// run N" via CountMissingFilesByRun without any audit-specific schema
// column.
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
		UPDATE files SET status = 'missing', last_seen_run_id = ?, status_changed_run_id = ?
		WHERE status = 'present' AND folder_id IN (
			SELECT id FROM folders WHERE volume_id = ?
		) AND last_seen_run_id != ?
	`, currentRunID, currentRunID, volumeID, currentRunID)
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

// MarkOffloaded flips the live 'present' row at (volumeID, relPath)
// carrying contentID to status='offloaded', stamping last_seen_run_id
// with the offload run that removed the bytes. Matching on the exact
// content id is part of the offload safety contract: the caller
// verified the on-disk bytes against this content immediately before
// unlinking, so a row whose content or status changed underfoot matches
// nothing here and surfaces as an error instead of mislabelling a
// different observation. The folder Merkle and ancestor chain are
// recomputed in the same transaction because the flip removes the file
// from the live set, mirroring MarkMissing.
func (s *Store) MarkOffloaded(ctx context.Context, volumeID int64, relPath string, contentID, runID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mark offloaded: %w", err)
	}
	defer tx.Rollback()

	folderPath, name := splitFilePath(relPath)
	var folderID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM folders WHERE volume_id = ? AND path = ?`,
		volumeID, folderPath).Scan(&folderID); err != nil {
		return fmt.Errorf("mark offloaded %s: lookup folder: %w", relPath, err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE files SET status = 'offloaded', last_seen_run_id = ?, status_changed_run_id = ?
		WHERE folder_id = ? AND name = ? AND content_id = ? AND status = 'present'
	`, runID, runID, folderID, name, contentID)
	if err != nil {
		return fmt.Errorf("mark offloaded %s: %w", relPath, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark offloaded rows %s: %w", relPath, err)
	}
	if n != 1 {
		return fmt.Errorf("mark offloaded %s: no live 'present' row with content id %d", relPath, contentID)
	}
	if err := recomputeFolderAndAncestors(ctx, tx, folderID, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListDuplicates returns rows whose content appears at more than one
// (volume_id, path), joined with their volume.
func (s *Store) ListDuplicates(ctx context.Context) ([]FileWithVolume, error) {
	return queryRows(ctx, s.db, `
		SELECT `+joinedColumns+`
		FROM `+fileFromJoin+` JOIN volumes v ON v.id = fo.volume_id
		WHERE f.content_id IN (
			SELECT content_id FROM files WHERE status = 'present'
			GROUP BY content_id HAVING COUNT(*) > 1
		)
		AND f.status = 'present'
		ORDER BY c.blake3, v.name, `+pathFromFolderAndName+`
	`, scanFileWithVolume)
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

// CountPresentFilesInVolume returns the number of live (status='present')
// file rows in the volume. The peer-sync summary uses it to report the
// already-correct count: under the Merkle walk only differing folders
// reach /plan, so already-correct is derived as present-total minus the
// paths the sync acted on rather than counted from the (partial)
// disposition list.
func (s *Store) CountPresentFilesInVolume(ctx context.Context, volumeID int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+fileFromJoin+`
		 WHERE f.status = 'present' AND fo.volume_id = ?`, volumeID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count present files in volume %d: %w", volumeID, err)
	}
	return n, nil
}

// ListPresentFilesInFolder returns every present file row in one
// folder, ordered by name ascending so the caller's downstream
// processing is deterministic. Used by the Merkle walk's initiator
// to assemble the /plan slice from only the folders the walk found
// differing — orders of magnitude smaller than the full-volume
// listing the flat protocol sends.
func (s *Store) ListPresentFilesInFolder(ctx context.Context, folderID int64) ([]FileRow, error) {
	return queryRows(ctx, s.db,
		`SELECT `+fileSelectColumns+` FROM `+fileFromJoin+`
		 WHERE f.folder_id = ? AND f.status = 'present'
		 ORDER BY f.name`,
		scanFileRow, folderID)
}

// ListPresentByOrigin yields every present row in volumeID whose
// content's origin_node_id matches nodeID. A valid nodeID matches that
// node id and exploits idx_contents_origin_node (the partial index on
// contents(origin_node_id) WHERE origin_node_id IS NOT NULL); a zero
// NullInt64 filters to content with origin_node_id IS NULL — the
// "introduced locally" convention — and falls back to a status-scoped
// scan because the partial index excludes those rows by construction.
//
// Yielded in path order so a caller streaming to `rclone --files-from`
// produces a stable, diffable listing. iter.Seq2 is used so large
// volumes don't materialise the whole row set in memory before the
// caller starts consuming it.
func (s *Store) ListPresentByOrigin(ctx context.Context, volumeID int64, nodeID sql.NullInt64) iter.Seq2[FileRow, error] {
	return func(yield func(FileRow, error) bool) {
		var (
			rows *sql.Rows
			err  error
		)
		if nodeID.Valid {
			rows, err = s.db.QueryContext(ctx,
				`SELECT `+fileSelectColumns+` FROM `+fileFromJoin+`
				 WHERE fo.volume_id = ? AND f.status = 'present' AND c.origin_node_id = ?
				 ORDER BY `+pathFromFolderAndName,
				volumeID, nodeID.Int64)
		} else {
			rows, err = s.db.QueryContext(ctx,
				`SELECT `+fileSelectColumns+` FROM `+fileFromJoin+`
				 WHERE fo.volume_id = ? AND f.status = 'present' AND c.origin_node_id IS NULL
				 ORDER BY `+pathFromFolderAndName,
				volumeID)
		}
		if err != nil {
			yield(FileRow{}, fmt.Errorf("query present by origin: %w", err))
			return
		}
		defer rows.Close()
		for rows.Next() {
			var r FileRow
			if err := r.scanFrom(rows); err != nil {
				yield(FileRow{}, fmt.Errorf("scan present by origin: %w", err))
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
	return queryRows(ctx, s.db, `
		SELECT `+joinedColumns+`
		FROM `+fileFromJoin+` JOIN volumes v ON v.id = fo.volume_id
		WHERE f.status = 'missing'
		ORDER BY v.name, `+pathFromFolderAndName+`
	`, scanFileWithVolume)
}

// scanFileWithVolume scans one joinedColumns row into a FileWithVolume.
// The file half routes through FileRow.scanDests so the file-column
// order stays defined in a single place; the volume pointers are
// appended after. Pairs with queryRows for every joined list-read.
func scanFileWithVolume(s rowScanner) (FileWithVolume, error) {
	var fv FileWithVolume
	dests := append(fv.File.scanDests(), &fv.Volume.ID, &fv.Volume.Name, &fv.Volume.Path)
	err := s.Scan(dests...)
	return fv, err
}

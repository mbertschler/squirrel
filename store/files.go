package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// FileRow is a single indexed file. Path is stored relative to the volume's
// path. VolumeID references volumes(id). FirstSeenRunID is the run that first
// inserted this row and is never overwritten on subsequent updates;
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

// fileColumns is the single source of truth for the files-table column
// order. SELECT lists, scanFrom, and insertArgs all derive from it; adding
// or reordering columns happens here and the helpers below stay in sync.
const fileColumns = `volume_id, path, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns, source_node_id, source_run_id`

// joinedColumns mirrors fileColumns with `f.` aliases plus the volume
// columns, for SELECTs that join files↔volumes. Kept manually aligned with
// fileColumns; the round-trip test pins the invariant.
const joinedColumns = `f.volume_id, f.path, f.blake3, f.size_bytes, f.mtime_ns, f.status, f.first_seen_run_id, f.last_seen_run_id, f.indexed_at_ns, f.source_node_id, f.source_run_id, v.id, v.name, v.path`

// filePlaceholders is the `?, ?, ?, ...` parameter list matching
// fileColumns by length. Computed once at package load so callers cannot
// drift from the canonical column count.
var filePlaceholders = buildPlaceholders(fileColumns)

func buildPlaceholders(cols string) string {
	n := strings.Count(cols, ",") + 1
	return strings.Repeat("?, ", n-1) + "?"
}

// rowScanner abstracts over *sql.Row and *sql.Rows so scanFrom can serve
// both single-row and multi-row reads.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanFrom reads the next row into r in fileColumns order. Pair every new
// SELECT with this method instead of hand-rolling another field-by-field
// list.
func (r *FileRow) scanFrom(s rowScanner) error {
	return s.Scan(r.scanDests()...)
}

// scanDests returns the per-column destination pointers in fileColumns
// order. scanFrom delegates here; collectJoined appends the volume
// pointers on top so files-half scanning still has one source of truth.
func (r *FileRow) scanDests() []any {
	return []any{
		&r.VolumeID, &r.Path, &r.Blake3, &r.SizeBytes, &r.MtimeNs,
		&r.Status, &r.FirstSeenRunID, &r.LastSeenRunID, &r.IndexedAtNs,
		&r.SourceNodeID, &r.SourceRunID,
	}
}

// insertArgs returns the row's column values in fileColumns order, ready
// to splat into an `INSERT INTO files (`+fileColumns+`) VALUES
// (`+filePlaceholders+`)` statement.
func (r FileRow) insertArgs() []any {
	return []any{
		r.VolumeID, r.Path, r.Blake3, r.SizeBytes, r.MtimeNs,
		r.Status, r.FirstSeenRunID, r.LastSeenRunID, r.IndexedAtNs,
		r.SourceNodeID, r.SourceRunID,
	}
}

// GetByPath returns the currently-live row for (volumeID, relPath) — i.e.
// the row with status='present' or status='missing'. Superseded rows
// (historical content at this path) are skipped; use ListHistoryByPath to
// see them. Returns sql.ErrNoRows when no live row exists for the path.
//
// The path-level invariant (at most one non-superseded row per
// (volume_id, path)) is enforced by Upsert and the v4 PK widening, so this
// query is unambiguous despite touching a non-unique key.
func (s *Store) GetByPath(ctx context.Context, volumeID int64, relPath string) (FileRow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+fileColumns+` FROM files
		 WHERE volume_id = ? AND path = ? AND status != 'superseded'`,
		volumeID, relPath)
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
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fileColumns+` FROM files
		 WHERE volume_id = ? AND path = ?
		 ORDER BY first_seen_run_id`,
		volumeID, relPath)
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

// GetByBlake3 returns all rows matching the given BLAKE3 digest (raw 32 bytes),
// joined with their volume.
func (s *Store) GetByBlake3(ctx context.Context, digest []byte) ([]FileWithVolume, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+joinedColumns+`
		FROM files f JOIN volumes v ON v.id = f.volume_id
		WHERE f.blake3 = ?
		ORDER BY v.name, f.path
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
//  1. A row with the exact (volume_id, path, blake3) already exists and is
//     the live row — update its mutable fields (touch / restore from
//     missing). first_seen_run_id is preserved.
//  2. A row with the exact (volume_id, path, blake3) exists but is
//     superseded (content has reverted to a previously-seen value) — flip
//     the currently-live row at this path to 'superseded' and revive the
//     matched row to the requested status (first_seen_run_id preserved).
//  3. No row exists at (volume_id, path, blake3) — flip the currently-live
//     row at (volume_id, path), if any, to 'superseded' and insert the new
//     row.
//
// In all cases, blake3 is never rewritten in place; content history at a
// path grows append-only.
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

	var existingStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM files WHERE volume_id = ? AND path = ? AND blake3 = ?`,
		r.VolumeID, r.Path, r.Blake3).Scan(&existingStatus)

	switch {
	case err == nil && existingStatus != StatusSuperseded:
		// Case 1: exact row exists and is live — touch it.
		if err := updateLiveRow(ctx, tx, r, prov); err != nil {
			return err
		}
	case err == nil && existingStatus == StatusSuperseded:
		// Case 2: content revert — supersede whatever is live now, then
		// revive the matched (formerly superseded) row.
		if err := supersedeLiveRow(ctx, tx, r.VolumeID, r.Path); err != nil {
			return err
		}
		if err := updateLiveRow(ctx, tx, r, prov); err != nil {
			return err
		}
	case errors.Is(err, sql.ErrNoRows):
		// Case 3: brand new content at this path (possibly first-ever).
		if err := supersedeLiveRow(ctx, tx, r.VolumeID, r.Path); err != nil {
			return err
		}
		if err := insertNewRow(ctx, tx, r, prov); err != nil {
			return err
		}
	default:
		return fmt.Errorf("lookup existing: %w", err)
	}

	return tx.Commit()
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

// supersedeLiveRow flips the single non-superseded row at (volumeID, relPath)
// (if any) to status='superseded'. A no-op when there is no live row, e.g.
// the very first observation of a path. last_seen_run_id stays frozen at the
// value it had — that is the run during which the row was last seen alive.
func supersedeLiveRow(ctx context.Context, tx *sql.Tx, volumeID int64, relPath string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE files SET status = 'superseded'
		WHERE volume_id = ? AND path = ? AND status != 'superseded'
	`, volumeID, relPath)
	if err != nil {
		return fmt.Errorf("supersede live row: %w", err)
	}
	return nil
}

// updateLiveRow refreshes the mutable fields on an existing row matching
// (volume_id, path, blake3). blake3 and first_seen_run_id are never touched.
// The (source_node_id, source_run_id) provenance pair is rewritten to the
// caller-supplied prov so the row tracks the most recent attribution.
func updateLiveRow(ctx context.Context, tx *sql.Tx, r FileRow, prov *Provenance) error {
	srcNode, srcRun := provColumns(prov)
	_, err := tx.ExecContext(ctx, `
		UPDATE files SET
			size_bytes = ?, mtime_ns = ?, status = ?,
			last_seen_run_id = ?, indexed_at_ns = ?,
			source_node_id = ?, source_run_id = ?
		WHERE volume_id = ? AND path = ? AND blake3 = ?
	`, r.SizeBytes, r.MtimeNs, r.Status, r.LastSeenRunID, r.IndexedAtNs,
		srcNode, srcRun,
		r.VolumeID, r.Path, r.Blake3)
	if err != nil {
		return fmt.Errorf("update live row: %w", err)
	}
	return nil
}

func insertNewRow(ctx context.Context, tx *sql.Tx, r FileRow, prov *Provenance) error {
	r.SourceNodeID, r.SourceRunID = provColumns(prov)
	_, err := tx.ExecContext(ctx,
		`INSERT INTO files (`+fileColumns+`) VALUES (`+filePlaceholders+`)`,
		r.insertArgs()...)
	if err != nil {
		return fmt.Errorf("insert new row: %w", err)
	}
	return nil
}

// TouchSeen sets last_seen_run_id on the live row at (volumeID, relPath) and
// flips its status to 'present'. Used by the indexer when it re-observed the
// exact same content as the stored row (kindUnchanged). first_seen_run_id is
// never modified. Does not touch superseded rows.
func (s *Store) TouchSeen(ctx context.Context, volumeID int64, relPath string, runID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE files SET last_seen_run_id = ?, status = 'present'
		 WHERE volume_id = ? AND path = ? AND status != 'superseded'`,
		runID, volumeID, relPath)
	return err
}

// MarkMissing flips every row in the given volume that was not touched by the
// given run (last_seen_run_id != currentRunID) and is currently 'present' to
// 'missing'. The caller is responsible for only invoking this after the run
// has fully scanned the volume: any path the run failed to visit (per-file
// error, context cancellation, fatal walk failure) will look "missing" to
// this query even when it still exists on disk. The indexer enforces this by
// skipping MarkMissing whenever report.Errors > 0 or the walk returned an
// error.
func (s *Store) MarkMissing(ctx context.Context, volumeID int64, currentRunID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE files SET status = 'missing'
		WHERE status = 'present' AND volume_id = ? AND last_seen_run_id != ?
	`, volumeID, currentRunID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListDuplicates returns rows whose blake3 digest appears at more than one
// (volume_id, path), joined with their volume.
func (s *Store) ListDuplicates(ctx context.Context) ([]FileWithVolume, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+joinedColumns+`
		FROM files f JOIN volumes v ON v.id = f.volume_id
		WHERE f.blake3 IN (
			SELECT blake3 FROM files WHERE status = 'present'
			GROUP BY blake3 HAVING COUNT(*) > 1
		)
		AND f.status = 'present'
		ORDER BY f.blake3, v.name, f.path
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
		`SELECT path FROM files WHERE status = 'present' AND volume_id = ?`, volumeID)
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

// ListMissing returns all rows with status='missing', joined with their volume.
func (s *Store) ListMissing(ctx context.Context) ([]FileWithVolume, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+joinedColumns+`
		FROM files f JOIN volumes v ON v.id = f.volume_id
		WHERE f.status = 'missing'
		ORDER BY v.name, f.path
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

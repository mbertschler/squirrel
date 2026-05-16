package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SchemaVersion is the schema version this binary writes and reads.
const SchemaVersion = 4

type Store struct {
	db *sql.DB
}

// Volume is a named indexing root. The path is an absolute filesystem path;
// it is intentionally not UNIQUE so two volumes can share the same path with
// different (future) filters or modes. The name is UNIQUE and case-sensitive.
type Volume struct {
	ID   int64
	Name string
	Path string
}

// FileRow is a single indexed file. Path is stored relative to the volume's
// path. VolumeID references volumes(id). FirstSeenRunID is the run that first
// inserted this row and is never overwritten on subsequent updates;
// LastSeenRunID advances on every observation.
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

// Run kinds. The runs.volume_id column is nullable so a future sync run can
// span volumes; index runs are always scoped to a single volume.
const (
	RunKindIndex = "index"
	RunKindSync  = "sync"
)

// Run statuses. A run begins in 'running' and is moved to a terminal state by
// FinishRun. 'partial' means the walk completed but some files errored.
const (
	RunStatusRunning = "running"
	RunStatusSuccess = "success"
	RunStatusFailed  = "failed"
	RunStatusPartial = "partial"
)

// Open opens (or creates) the SQLite database at the given filesystem path
// and ensures the schema is at the version this binary expects. The path must
// be a plain filesystem path (no '?' query string and no URI scheme prefix);
// DSN parameters are managed internally so callers cannot override pragmas
// like journal_mode or busy_timeout. Returns an error if the database's
// schema version is newer than SchemaVersion, or if it is at an older
// unsupported version (this binary does not migrate v1 databases).
func Open(path string) (*Store, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("db path %q must not contain '?' or '#'", path)
	}
	if strings.Contains(path, "://") || strings.HasPrefix(path, "file:") {
		return nil, fmt.Errorf("db path %q must be a plain filesystem path, not a URI", path)
	}
	// _txlock=immediate makes BeginTx start with `BEGIN IMMEDIATE`, acquiring
	// the write lock at transaction start. Without it, a transaction that
	// only reads first and then upgrades to a write (which is exactly what
	// Upsert's state machine does) can race against another writer and lose
	// the "supersede prior live row" step. With MaxOpenConns=1 today this is
	// theoretical, but the schema-level invariants would still let a buggy
	// future change land bad state — IMMEDIATE keeps the contract explicit.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}

	var current int
	row := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`)
	if err := row.Scan(&current); err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}

	if current > SchemaVersion {
		return fmt.Errorf("database schema version %d is newer than binary version %d; upgrade the binary", current, SchemaVersion)
	}
	if current == 1 {
		return fmt.Errorf("database schema version 1 is no longer supported (binary expects v%d); delete the database and re-index", SchemaVersion)
	}
	if current == 0 {
		if err := applyV4(ctx, s.db); err != nil {
			return fmt.Errorf("apply schema v4: %w", err)
		}
		return nil
	}
	// Chained upgrades — each step is a self-contained transaction.
	if current == 2 {
		if err := migrateV2ToV3(ctx, s.db); err != nil {
			return fmt.Errorf("migrate schema v2→v3: %w", err)
		}
		current = 3
	}
	if current == 3 {
		if err := migrateV3ToV4(ctx, s.db); err != nil {
			return fmt.Errorf("migrate schema v3→v4: %w", err)
		}
	}
	return nil
}

func applyV4(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, q := range schemaV4Stmts() {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("exec %q: %w", q, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (4)`); err != nil {
		return fmt.Errorf("record schema v4: %w", err)
	}
	return tx.Commit()
}

// schemaV4Stmts returns the DDL for a fresh v4 database. The files table's
// primary key is widened to (volume_id, path, blake3) so content history at a
// path is append-only: when a file is rewritten with different content, a new
// row is inserted and the prior row is marked 'superseded'.
func schemaV4Stmts() []string {
	return []string{
		`CREATE TABLE volumes (
			id   INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			path TEXT NOT NULL
		)`,
		`CREATE TABLE runs (
			id            INTEGER PRIMARY KEY,
			kind          TEXT NOT NULL CHECK (kind IN ('index','sync')),
			volume_id     INTEGER REFERENCES volumes(id),
			started_at_ns INTEGER NOT NULL,
			ended_at_ns   INTEGER,
			status        TEXT NOT NULL CHECK (status IN ('running','success','failed','partial')),
			error         TEXT,
			file_count    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX idx_runs_volume_started ON runs(volume_id, started_at_ns)`,
		`CREATE TABLE files (
			volume_id         INTEGER NOT NULL REFERENCES volumes(id),
			path              TEXT NOT NULL,
			blake3            BLOB NOT NULL CHECK (length(blake3) = 32),
			size_bytes        INTEGER NOT NULL,
			mtime_ns          INTEGER NOT NULL,
			status            TEXT NOT NULL CHECK (status IN ('present','missing','superseded')),
			first_seen_run_id INTEGER NOT NULL REFERENCES runs(id),
			last_seen_run_id  INTEGER NOT NULL REFERENCES runs(id),
			indexed_at_ns     INTEGER NOT NULL,
			PRIMARY KEY (volume_id, path, blake3)
		)`,
		// Covering index for blake3 lookups and cross-volume duplicate detection.
		`CREATE INDEX idx_files_blake3 ON files(blake3, volume_id, path)`,
		// Partial index: 'missing' is rare and 'superseded' is bounded by the
		// number of past content changes per path. Indexing 'present' (covers
		// ~all rows) would only inflate writes.
		`CREATE INDEX idx_files_missing ON files(volume_id, path) WHERE status = 'missing'`,
		// Partial UNIQUE index enforces the path-level invariant: at most one
		// non-superseded row per (volume_id, path). Doubles as the lookup
		// index that Upsert's state machine uses to find the live row.
		`CREATE UNIQUE INDEX uniq_files_live_per_path ON files(volume_id, path) WHERE status != 'superseded'`,
		// Schema-level enforcement of the "blake3 is immutable" rule. Any
		// UPDATE that mentions the blake3 column in its SET clause aborts.
		// The only sanctioned way to record new content at a path is to
		// supersede the prior row and INSERT a new one (see Upsert).
		`CREATE TRIGGER files_blake3_immutable BEFORE UPDATE OF blake3 ON files
		 BEGIN
		     SELECT RAISE(ABORT, 'blake3 is immutable; supersede the row and insert a new one');
		 END`,
	}
}

// migrateV2ToV3 upgrades an existing v2 database. It synthesizes one
// successful 'index' run per existing volume to stand in for the history we
// don't have, then rebuilds the files table with first_seen_run_id and
// last_seen_run_id columns referencing those runs. The drop of
// last_seen_at_ns is done via table rebuild (cleaner than ALTER ... DROP
// COLUMN, which requires SQLite 3.35+ and would leave dangling indices).
func migrateV2ToV3(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := createV3Runs(ctx, tx); err != nil {
		return err
	}
	if err := seedImportRunsForExistingVolumes(ctx, tx); err != nil {
		return err
	}
	if err := rebuildFilesAsV3(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (3)`); err != nil {
		return fmt.Errorf("record schema v3: %w", err)
	}
	return tx.Commit()
}

func createV3Runs(ctx context.Context, tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE runs (
			id            INTEGER PRIMARY KEY,
			kind          TEXT NOT NULL CHECK (kind IN ('index','sync')),
			volume_id     INTEGER REFERENCES volumes(id),
			started_at_ns INTEGER NOT NULL,
			ended_at_ns   INTEGER,
			status        TEXT NOT NULL CHECK (status IN ('running','success','failed','partial')),
			error         TEXT,
			file_count    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX idx_runs_volume_started ON runs(volume_id, started_at_ns)`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("create runs: %w", err)
		}
	}
	return nil
}

// seedImportRunsForExistingVolumes inserts one synthetic successful 'index'
// run per existing volume. Per-row history wasn't recorded in v2 so we
// collapse it into a single import point pinned to migration time.
func seedImportRunsForExistingVolumes(ctx context.Context, tx *sql.Tx) error {
	now := time.Now().UnixNano()
	_, err := tx.ExecContext(ctx, `
		INSERT INTO runs (kind, volume_id, started_at_ns, ended_at_ns, status, file_count)
		SELECT 'index', v.id, ?, ?, 'success',
		       (SELECT COUNT(*) FROM files f WHERE f.volume_id = v.id)
		FROM volumes v
	`, now, now)
	if err != nil {
		return fmt.Errorf("seed import runs: %w", err)
	}
	return nil
}

// rebuildFilesAsV3 creates the v3 files table, copies every v2 row into it
// joined against the per-volume synthetic run, and drops the old table.
func rebuildFilesAsV3(ctx context.Context, tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE files_v3 (
			volume_id         INTEGER NOT NULL REFERENCES volumes(id),
			path              TEXT NOT NULL,
			blake3            BLOB NOT NULL CHECK (length(blake3) = 32),
			size_bytes        INTEGER NOT NULL,
			mtime_ns          INTEGER NOT NULL,
			status            TEXT NOT NULL CHECK (status IN ('present','missing')),
			first_seen_run_id INTEGER NOT NULL REFERENCES runs(id),
			last_seen_run_id  INTEGER NOT NULL REFERENCES runs(id),
			indexed_at_ns     INTEGER NOT NULL,
			PRIMARY KEY (volume_id, path)
		)`,
		`INSERT INTO files_v3 (
			volume_id, path, blake3, size_bytes, mtime_ns, status,
			first_seen_run_id, last_seen_run_id, indexed_at_ns
		)
		SELECT f.volume_id, f.path, f.blake3, f.size_bytes, f.mtime_ns, f.status,
		       r.id, r.id, f.indexed_at_ns
		FROM files f
		JOIN runs r ON r.volume_id = f.volume_id`,
		`DROP TABLE files`,
		`ALTER TABLE files_v3 RENAME TO files`,
		`CREATE INDEX idx_files_blake3 ON files(blake3, volume_id, path)`,
		`CREATE INDEX idx_files_missing ON files(volume_id, path) WHERE status = 'missing'`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("rebuild files: %w", err)
		}
	}
	return nil
}

// migrateV3ToV4 widens the files PK from (volume_id, path) to
// (volume_id, path, blake3) and adds 'superseded' to the status check. Every
// existing v3 row has a unique blake3 at its (volume, path), so the widening
// is conflict-free and existing rows carry over verbatim.
func migrateV3ToV4(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`CREATE TABLE files_v4 (
			volume_id         INTEGER NOT NULL REFERENCES volumes(id),
			path              TEXT NOT NULL,
			blake3            BLOB NOT NULL CHECK (length(blake3) = 32),
			size_bytes        INTEGER NOT NULL,
			mtime_ns          INTEGER NOT NULL,
			status            TEXT NOT NULL CHECK (status IN ('present','missing','superseded')),
			first_seen_run_id INTEGER NOT NULL REFERENCES runs(id),
			last_seen_run_id  INTEGER NOT NULL REFERENCES runs(id),
			indexed_at_ns     INTEGER NOT NULL,
			PRIMARY KEY (volume_id, path, blake3)
		)`,
		`INSERT INTO files_v4 (
			volume_id, path, blake3, size_bytes, mtime_ns, status,
			first_seen_run_id, last_seen_run_id, indexed_at_ns
		)
		SELECT volume_id, path, blake3, size_bytes, mtime_ns, status,
		       first_seen_run_id, last_seen_run_id, indexed_at_ns
		FROM files`,
		`DROP TABLE files`,
		`ALTER TABLE files_v4 RENAME TO files`,
		`CREATE INDEX idx_files_blake3 ON files(blake3, volume_id, path)`,
		`CREATE INDEX idx_files_missing ON files(volume_id, path) WHERE status = 'missing'`,
		`CREATE UNIQUE INDEX uniq_files_live_per_path ON files(volume_id, path) WHERE status != 'superseded'`,
		`CREATE TRIGGER files_blake3_immutable BEFORE UPDATE OF blake3 ON files
		 BEGIN
		     SELECT RAISE(ABORT, 'blake3 is immutable; supersede the row and insert a new one');
		 END`,
		`INSERT INTO schema_version (version) VALUES (4)`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("widen files PK: %w", err)
		}
	}
	return tx.Commit()
}

// --- Volume APIs ---

// GetOrCreateVolume returns the volume whose path equals absPath, or creates
// a new one with a basename-derived name. On UNIQUE name collision, a numeric
// suffix (-2, -3, ...) is appended until a free name is found.
func (s *Store) GetOrCreateVolume(ctx context.Context, absPath string) (Volume, error) {
	var v Volume
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, path FROM volumes WHERE path = ? ORDER BY id LIMIT 1`, absPath).
		Scan(&v.ID, &v.Name, &v.Path)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Volume{}, fmt.Errorf("lookup volume by path: %w", err)
	}

	base := filepath.Base(absPath)
	const maxAttempts = 1000
	for i := range maxAttempts {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i+1)
		}
		var existingID int64
		err := s.db.QueryRowContext(ctx,
			`SELECT id FROM volumes WHERE name = ?`, name).Scan(&existingID)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Volume{}, fmt.Errorf("lookup volume by name: %w", err)
		}
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO volumes (name, path) VALUES (?, ?)`, name, absPath)
		if err != nil {
			return Volume{}, fmt.Errorf("insert volume: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return Volume{}, fmt.Errorf("volume last insert id: %w", err)
		}
		return Volume{ID: id, Name: name, Path: absPath}, nil
	}
	return Volume{}, fmt.Errorf("could not allocate unique volume name for %q after %d attempts", absPath, maxAttempts)
}

// GetVolumeByID returns the volume with the given id, or sql.ErrNoRows.
func (s *Store) GetVolumeByID(ctx context.Context, id int64) (Volume, error) {
	var v Volume
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, path FROM volumes WHERE id = ?`, id).
		Scan(&v.ID, &v.Name, &v.Path)
	return v, err
}

// GetVolumeByName returns the volume with the given name, or sql.ErrNoRows.
// Names are UNIQUE per the schema so this lookup is unambiguous.
func (s *Store) GetVolumeByName(ctx context.Context, name string) (Volume, error) {
	var v Volume
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, path FROM volumes WHERE name = ?`, name).
		Scan(&v.ID, &v.Name, &v.Path)
	return v, err
}

// GetVolumeByPath returns the volume whose path equals absPath, or
// sql.ErrNoRows. When multiple volumes share the same path (allowed by the
// schema), the lowest id wins.
func (s *Store) GetVolumeByPath(ctx context.Context, absPath string) (Volume, error) {
	var v Volume
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, path FROM volumes WHERE path = ? ORDER BY id LIMIT 1`, absPath).
		Scan(&v.ID, &v.Name, &v.Path)
	return v, err
}

// ListVolumes returns all volumes ordered by id.
func (s *Store) ListVolumes(ctx context.Context) ([]Volume, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, path FROM volumes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Volume
	for rows.Next() {
		var v Volume
		if err := rows.Scan(&v.ID, &v.Name, &v.Path); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// --- File APIs ---

const fileColumns = `volume_id, path, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns`

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
	return scanFileRow(row.Scan)
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
		r, err := scanFileRow(rows.Scan)
		if err != nil {
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
func (s *Store) Upsert(ctx context.Context, r FileRow) error {
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
		if err := updateLiveRow(ctx, tx, r); err != nil {
			return err
		}
	case err == nil && existingStatus == StatusSuperseded:
		// Case 2: content revert — supersede whatever is live now, then
		// revive the matched (formerly superseded) row.
		if err := supersedeLiveRow(ctx, tx, r.VolumeID, r.Path); err != nil {
			return err
		}
		if err := updateLiveRow(ctx, tx, r); err != nil {
			return err
		}
	case errors.Is(err, sql.ErrNoRows):
		// Case 3: brand new content at this path (possibly first-ever).
		if err := supersedeLiveRow(ctx, tx, r.VolumeID, r.Path); err != nil {
			return err
		}
		if err := insertNewRow(ctx, tx, r); err != nil {
			return err
		}
	default:
		return fmt.Errorf("lookup existing: %w", err)
	}

	return tx.Commit()
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
func updateLiveRow(ctx context.Context, tx *sql.Tx, r FileRow) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE files SET
			size_bytes = ?, mtime_ns = ?, status = ?,
			last_seen_run_id = ?, indexed_at_ns = ?
		WHERE volume_id = ? AND path = ? AND blake3 = ?
	`, r.SizeBytes, r.MtimeNs, r.Status, r.LastSeenRunID, r.IndexedAtNs,
		r.VolumeID, r.Path, r.Blake3)
	if err != nil {
		return fmt.Errorf("update live row: %w", err)
	}
	return nil
}

func insertNewRow(ctx context.Context, tx *sql.Tx, r FileRow) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO files (
			volume_id, path, blake3, size_bytes, mtime_ns, status,
			first_seen_run_id, last_seen_run_id, indexed_at_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, r.VolumeID, r.Path, r.Blake3, r.SizeBytes, r.MtimeNs, r.Status,
		r.FirstSeenRunID, r.LastSeenRunID, r.IndexedAtNs)
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

// CurrentSchemaVersion returns the version currently stored in the DB.
func (s *Store) CurrentSchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v)
	return v, err
}

const joinedColumns = `f.volume_id, f.path, f.blake3, f.size_bytes, f.mtime_ns, f.status, f.first_seen_run_id, f.last_seen_run_id, f.indexed_at_ns, v.id, v.name, v.path`

func scanFileRow(scan func(...any) error) (FileRow, error) {
	var r FileRow
	err := scan(&r.VolumeID, &r.Path, &r.Blake3, &r.SizeBytes, &r.MtimeNs, &r.Status, &r.FirstSeenRunID, &r.LastSeenRunID, &r.IndexedAtNs)
	return r, err
}

func collectJoined(rows *sql.Rows) ([]FileWithVolume, error) {
	var out []FileWithVolume
	for rows.Next() {
		var fv FileWithVolume
		if err := rows.Scan(
			&fv.File.VolumeID, &fv.File.Path, &fv.File.Blake3, &fv.File.SizeBytes,
			&fv.File.MtimeNs, &fv.File.Status, &fv.File.FirstSeenRunID, &fv.File.LastSeenRunID, &fv.File.IndexedAtNs,
			&fv.Volume.ID, &fv.Volume.Name, &fv.Volume.Path,
		); err != nil {
			return nil, err
		}
		out = append(out, fv)
	}
	return out, rows.Err()
}

// IsNotFound reports whether err is sql.ErrNoRows.
func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// NowNs returns the current wall-clock time in nanoseconds.
func NowNs() int64 { return time.Now().UnixNano() }

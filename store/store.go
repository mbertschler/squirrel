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
const SchemaVersion = 2

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
// path. VolumeID references volumes(id).
type FileRow struct {
	VolumeID     int64
	Path         string
	Blake3       []byte // raw 32-byte BLAKE3-256 digest
	SizeBytes    int64
	MtimeNs      int64
	Status       string
	LastSeenAtNs int64
	IndexedAtNs  int64
}

// FileWithVolume bundles a file row with its volume, returned by read APIs
// that need to render full filesystem paths to the user.
type FileWithVolume struct {
	File   FileRow
	Volume Volume
}

const (
	StatusPresent = "present"
	StatusMissing = "missing"
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
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
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
	if current > 0 && current < SchemaVersion {
		return fmt.Errorf("database schema version %d is no longer supported (binary expects v%d); delete the database and re-index", current, SchemaVersion)
	}
	if current == 0 {
		if err := applyV2(ctx, s.db); err != nil {
			return fmt.Errorf("apply schema v2: %w", err)
		}
	}
	return nil
}

func applyV2(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`CREATE TABLE volumes (
			id   INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			path TEXT NOT NULL
		)`,
		`CREATE TABLE files (
			volume_id     INTEGER NOT NULL REFERENCES volumes(id),
			path          TEXT NOT NULL,
			blake3        BLOB NOT NULL CHECK (length(blake3) = 32),
			size_bytes    INTEGER NOT NULL,
			mtime_ns      INTEGER NOT NULL,
			status        TEXT NOT NULL CHECK (status IN ('present','missing')),
			last_seen_at_ns INTEGER NOT NULL,
			indexed_at_ns INTEGER NOT NULL,
			PRIMARY KEY (volume_id, path)
		)`,
		// Covering index for blake3 lookups and cross-volume duplicate detection.
		`CREATE INDEX idx_files_blake3 ON files(blake3, volume_id, path)`,
		// Partial index: 'missing' is the rare/selective state. 'present' covers
		// ~all rows and an index there would only inflate writes.
		`CREATE INDEX idx_files_missing ON files(volume_id, path) WHERE status = 'missing'`,
		`INSERT INTO schema_version (version) VALUES (2)`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("exec %q: %w", q, err)
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
	for i := 0; i < maxAttempts; i++ {
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

const fileColumns = `volume_id, path, blake3, size_bytes, mtime_ns, status, last_seen_at_ns, indexed_at_ns`

// GetByPath returns the row for (volumeID, relPath) or sql.ErrNoRows.
func (s *Store) GetByPath(ctx context.Context, volumeID int64, relPath string) (FileRow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+fileColumns+` FROM files WHERE volume_id = ? AND path = ?`, volumeID, relPath)
	return scanFileRow(row.Scan)
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
// without a trailing slash.
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

// Upsert inserts or updates a file row.
func (s *Store) Upsert(ctx context.Context, r FileRow) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO files (volume_id, path, blake3, size_bytes, mtime_ns, status, last_seen_at_ns, indexed_at_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(volume_id, path) DO UPDATE SET
			blake3 = excluded.blake3,
			size_bytes = excluded.size_bytes,
			mtime_ns = excluded.mtime_ns,
			status = excluded.status,
			last_seen_at_ns = excluded.last_seen_at_ns,
			indexed_at_ns = excluded.indexed_at_ns
	`, r.VolumeID, r.Path, r.Blake3, r.SizeBytes, r.MtimeNs, r.Status, r.LastSeenAtNs, r.IndexedAtNs)
	return err
}

// TouchSeen updates last_seen_at_ns and status='present' for (volumeID, relPath).
func (s *Store) TouchSeen(ctx context.Context, volumeID int64, relPath string, lastSeenAtNs int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE files SET last_seen_at_ns = ?, status = 'present' WHERE volume_id = ? AND path = ?`,
		lastSeenAtNs, volumeID, relPath)
	return err
}

// MarkMissing flips every row in the given volume with last_seen_at_ns < cutoffNs
// and status='present' to status='missing'. Returns the number of rows changed.
func (s *Store) MarkMissing(ctx context.Context, volumeID int64, cutoffNs int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE files SET status = 'missing'
		WHERE status = 'present' AND volume_id = ? AND last_seen_at_ns < ?
	`, volumeID, cutoffNs)
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

const joinedColumns = `f.volume_id, f.path, f.blake3, f.size_bytes, f.mtime_ns, f.status, f.last_seen_at_ns, f.indexed_at_ns, v.id, v.name, v.path`

func scanFileRow(scan func(...any) error) (FileRow, error) {
	var r FileRow
	err := scan(&r.VolumeID, &r.Path, &r.Blake3, &r.SizeBytes, &r.MtimeNs, &r.Status, &r.LastSeenAtNs, &r.IndexedAtNs)
	return r, err
}

func collectJoined(rows *sql.Rows) ([]FileWithVolume, error) {
	var out []FileWithVolume
	for rows.Next() {
		var fv FileWithVolume
		if err := rows.Scan(
			&fv.File.VolumeID, &fv.File.Path, &fv.File.Blake3, &fv.File.SizeBytes,
			&fv.File.MtimeNs, &fv.File.Status, &fv.File.LastSeenAtNs, &fv.File.IndexedAtNs,
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

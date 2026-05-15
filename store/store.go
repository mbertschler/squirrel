package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// SchemaVersion is the schema version this binary writes and reads.
const SchemaVersion = 1

type Store struct {
	db *sql.DB
}

// FileRow is a single indexed file. Path is stored relative to Root.
// Root is currently the absolute filesystem path passed to `squirrel index`;
// a future config milestone will turn this into a logical root name.
type FileRow struct {
	Root       string
	Path       string
	Blake3     []byte // raw 32-byte BLAKE3-256 digest
	SizeBytes  int64
	MtimeNs    int64
	Status     string
	LastSeenAt int64
	IndexedAt  int64
}

const (
	StatusPresent = "present"
	StatusMissing = "missing"
)

// Open opens (or creates) the SQLite database at dsn and ensures the schema is
// at the version this binary expects. Returns an error if the database's
// schema version is newer than SchemaVersion.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
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

	if current < 1 {
		if err := applyV1(ctx, s.db); err != nil {
			return fmt.Errorf("apply schema v1: %w", err)
		}
	}

	return nil
}

func applyV1(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`CREATE TABLE files (
			root TEXT NOT NULL,
			path TEXT NOT NULL,
			blake3 BLOB NOT NULL CHECK (length(blake3) = 32),
			size_bytes INTEGER NOT NULL,
			mtime_ns INTEGER NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('present','missing')),
			last_seen_at INTEGER NOT NULL,
			indexed_at INTEGER NOT NULL,
			PRIMARY KEY (root, path)
		)`,
		`CREATE INDEX idx_files_blake3 ON files(blake3)`,
		`CREATE INDEX idx_files_status ON files(status)`,
		`INSERT INTO schema_version (version) VALUES (1)`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("exec %q: %w", q, err)
		}
	}
	return tx.Commit()
}

const selectColumns = `root, path, blake3, size_bytes, mtime_ns, status, last_seen_at, indexed_at`

// GetByPath returns the row for (root, relPath) or sql.ErrNoRows if none exists.
func (s *Store) GetByPath(ctx context.Context, root, relPath string) (FileRow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+selectColumns+` FROM files WHERE root = ? AND path = ?`, root, relPath)
	return scanRow(row.Scan)
}

// GetByAbsolutePath finds a row whose root || '/' || path equals abs. Used by
// the `squirrel query <path>` CLI when the caller does not know the root.
func (s *Store) GetByAbsolutePath(ctx context.Context, abs string) (FileRow, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+selectColumns+`
		FROM files
		WHERE (root = ? AND path = '.')
		   OR (root || '/' || path = ?)
		LIMIT 1
	`, abs, abs)
	return scanRow(row.Scan)
}

// GetByBlake3 returns all rows matching the given BLAKE3 digest (raw 32 bytes).
func (s *Store) GetByBlake3(ctx context.Context, digest []byte) ([]FileRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectColumns+` FROM files WHERE blake3 = ? ORDER BY root, path`, digest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectRows(rows)
}

// Upsert inserts or updates a file row.
func (s *Store) Upsert(ctx context.Context, r FileRow) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO files (root, path, blake3, size_bytes, mtime_ns, status, last_seen_at, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(root, path) DO UPDATE SET
			blake3 = excluded.blake3,
			size_bytes = excluded.size_bytes,
			mtime_ns = excluded.mtime_ns,
			status = excluded.status,
			last_seen_at = excluded.last_seen_at,
			indexed_at = excluded.indexed_at
	`, r.Root, r.Path, r.Blake3, r.SizeBytes, r.MtimeNs, r.Status, r.LastSeenAt, r.IndexedAt)
	return err
}

// TouchSeen updates last_seen_at and status='present' for (root, relPath).
func (s *Store) TouchSeen(ctx context.Context, root, relPath string, lastSeenAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE files SET last_seen_at = ?, status = 'present' WHERE root = ? AND path = ?`,
		lastSeenAt, root, relPath)
	return err
}

// MarkMissing marks every row in the given root with last_seen_at < cutoff and
// status='present' as missing. Returns the number of rows changed.
func (s *Store) MarkMissing(ctx context.Context, root string, cutoff int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE files SET status = 'missing'
		WHERE status = 'present' AND root = ? AND last_seen_at < ?
	`, root, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListDuplicates returns rows whose blake3 digest appears at more than one
// (root, path). Rows are returned sorted by blake3, root, path.
func (s *Store) ListDuplicates(ctx context.Context) ([]FileRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+selectColumns+`
		FROM files
		WHERE blake3 IN (
			SELECT blake3 FROM files WHERE status = 'present' GROUP BY blake3 HAVING COUNT(*) > 1
		)
		AND status = 'present'
		ORDER BY blake3, root, path
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectRows(rows)
}

// ListPresentPathsUnder returns the set of relative paths with status='present'
// within the given root.
func (s *Store) ListPresentPathsUnder(ctx context.Context, root string) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path FROM files WHERE status = 'present' AND root = ?`, root)
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

// ListMissing returns rows whose status is 'missing'.
func (s *Store) ListMissing(ctx context.Context) ([]FileRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectColumns+` FROM files WHERE status = 'missing' ORDER BY root, path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectRows(rows)
}

// CurrentSchemaVersion returns the version currently stored in the DB.
func (s *Store) CurrentSchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v)
	return v, err
}

// SetSchemaVersion forces a schema version. Test-only helper for round-trip checks.
func (s *Store) SetSchemaVersion(ctx context.Context, v int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, v)
	return err
}

func scanRow(scan func(...any) error) (FileRow, error) {
	var r FileRow
	err := scan(&r.Root, &r.Path, &r.Blake3, &r.SizeBytes, &r.MtimeNs, &r.Status, &r.LastSeenAt, &r.IndexedAt)
	return r, err
}

func collectRows(rows *sql.Rows) ([]FileRow, error) {
	var out []FileRow
	for rows.Next() {
		var r FileRow
		if err := rows.Scan(&r.Root, &r.Path, &r.Blake3, &r.SizeBytes, &r.MtimeNs, &r.Status, &r.LastSeenAt, &r.IndexedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// IsNotFound reports whether err is sql.ErrNoRows.
func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// Now returns the current wall-clock time in nanoseconds.
func Now() int64 { return time.Now().UnixNano() }

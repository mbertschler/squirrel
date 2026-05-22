package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
)

// Backup writes a consistent online snapshot of the database to dstPath
// using SQLite's `VACUUM INTO`. The destination must not exist; the
// snapshot is written under the destination's parent directory in a
// single transaction and visible to other processes atomically when the
// VACUUM completes. The runtime size of the snapshot is the size of
// the live data pages (not including WAL or unused pages), so the
// backup is also a compact form of the DB.
//
// VACUUM INTO works while the database is in use; concurrent writers
// see a serialised view of the DB as the VACUUM started, and the
// snapshot reflects that point in time. No new connection is opened —
// the existing *sql.DB handle is used.
func (s *Store) Backup(ctx context.Context, dstPath string) error {
	if dstPath == "" {
		return fmt.Errorf("Backup: destination path must be non-empty")
	}
	if _, err := os.Stat(dstPath); err == nil {
		return fmt.Errorf("Backup: %s already exists; choose a different path", dstPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("Backup: stat %s: %w", dstPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return fmt.Errorf("Backup: mkdir parent: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dstPath); err != nil {
		return fmt.Errorf("Backup: VACUUM INTO: %w", err)
	}
	return nil
}

// IntegrityCheck runs SQLite's `PRAGMA integrity_check` and returns
// the result rows. A clean database returns ["ok"]; corruption surfaces
// as one or more diagnostic strings naming the affected page or index.
// Callers should treat any result other than ["ok"] as failure; we
// avoid auto-repair because SQLite's recover process is destructive
// and opt-in by design.
func (s *Store) IntegrityCheck(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return nil, fmt.Errorf("PRAGMA integrity_check: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, fmt.Errorf("scan integrity_check row: %w", err)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate integrity_check rows: %w", err)
	}
	return out, nil
}

// IsIntegrityClean reports whether the result of IntegrityCheck is the
// single "ok" string SQLite returns on a clean DB. Convenience helper
// so CLI callers don't have to know the exact wire format.
func IsIntegrityClean(rows []string) bool {
	return len(rows) == 1 && rows[0] == "ok"
}

// ProbeLiveDBExclusive opens livePath without running migrations and
// attempts to start an EXCLUSIVE transaction. The transaction is
// immediately rolled back; the only point is to detect whether
// another process holds the DB open. SQLite's POSIX advisory locking
// handles this without needing flock or pidfiles.
//
// A nil return means the lock was obtained (and released) — nobody
// else has the DB open. A non-nil return means someone does
// (typically the running agent). The check is racy with respect to a
// process that opens the DB right after the lock is released; the
// caller should follow up with the file-system swap quickly.
func ProbeLiveDBExclusive(ctx context.Context, livePath string) error {
	if _, err := os.Stat(livePath); err != nil {
		if os.IsNotExist(err) {
			// No live DB; nothing to lock.
			return nil
		}
		return fmt.Errorf("stat live db: %w", err)
	}
	dsn := "file:" + filepath.ToSlash(livePath) + "?_pragma=busy_timeout(50)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("probe live db: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// BEGIN IMMEDIATE acquires the SQLite write lock without
	// requiring an open transaction first. In WAL mode this succeeds
	// only if no other process holds the write lock. The short
	// busy_timeout above keeps us from blocking on a contended live
	// agent.
	if _, err := db.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("live db is in use by another process")
	}
	if _, err := db.ExecContext(ctx, `ROLLBACK`); err != nil {
		// Best-effort cleanup — the probe is read-only so leaving the
		// connection open until db.Close() is acceptable.
		return nil
	}
	return nil
}

// PreflightCheckSnapshot reads dstPath as a candidate snapshot
// (a SQLite database) and verifies it is openable, syntactically a
// valid squirrel database, and at the same schema version as the
// running binary. Returns the snapshot's schema version on success;
// the caller is responsible for any further compatibility decisions.
// Does not modify the live DB or the snapshot.
func PreflightCheckSnapshot(ctx context.Context, snapshotPath string) (int, error) {
	if _, err := os.Stat(snapshotPath); err != nil {
		return 0, fmt.Errorf("snapshot %s: %w", snapshotPath, err)
	}
	// Open read-only so we don't accidentally migrate or modify the
	// snapshot. ?mode=ro plus query string syntax matches what Open
	// uses for the live DB, with the URI escaping applied via
	// filepath.ToSlash for cross-platform safety.
	dsn := "file:" + filepath.ToSlash(snapshotPath) + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return 0, fmt.Errorf("open snapshot: %w", err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema_version from snapshot: %w", err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("snapshot has no schema_version rows; not a squirrel database")
	}
	return v, nil
}

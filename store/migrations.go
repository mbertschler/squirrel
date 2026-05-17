package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SchemaVersion is the schema version this binary writes and reads.
const SchemaVersion = 7

// freshSchemaBaseline is the version applied to a brand-new database. The
// chain in `migrations` continues from here. v1 is no longer reachable from
// either branch — its migration function was deleted along with support —
// so a fresh DB jumps straight to v5 and then steps forward through the
// registry to SchemaVersion.
const freshSchemaBaseline = 5

// migration is one step in the registry. up advances the schema from
// (version - 1) to version, atomically with the matching INSERT INTO
// schema_version inside its own transaction. nodeName is threaded through
// for migrations that seed identity rows (today: v5→v6); migrations that
// don't need it ignore the argument.
type migration struct {
	version int
	up      func(ctx context.Context, db *sql.DB) error
}

// buildMigrations returns the ordered registry, with mctx-dependent
// migrations bound via closure so each entry shares the uniform
// `func(ctx, db) error` shape. The slice MUST stay strictly ascending by
// version; runMigrations relies on that order and a guard inside the
// loop rejects misordered slices before they run.
func buildMigrations(mctx migrationCtx) []migration {
	return []migration{
		{version: 3, up: migrateV2ToV3},
		{version: 4, up: migrateV3ToV4},
		{version: 5, up: migrateV4ToV5},
		{version: 6, up: func(ctx context.Context, db *sql.DB) error {
			return migrateV5ToV6(ctx, db, mctx.nodeName)
		}},
		{version: 7, up: migrateV6ToV7},
	}
}

// migrationCtx carries inputs migrations need from Open. nodeName is the
// validated identifier seeded into the self `nodes` row on v5→v6.
type migrationCtx struct {
	nodeName string
}

// migrate brings the database from whatever version it is at up to
// SchemaVersion. Refuses futures and the obsolete v1 schema. A fresh
// database (current == 0) jumps to the baseline then steps forward.
func (s *Store) migrate(ctx context.Context, nodeName string) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}

	current, err := s.CurrentSchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}

	if current > SchemaVersion {
		return fmt.Errorf("database schema version %d is newer than binary version %d; upgrade the binary", current, SchemaVersion)
	}
	if current == 1 {
		return fmt.Errorf("database schema version 1 is no longer supported (binary expects v%d); delete the database and re-index", SchemaVersion)
	}
	if current == 0 {
		if err := applyV5(ctx, s.db); err != nil {
			return fmt.Errorf("apply schema v%d: %w", freshSchemaBaseline, err)
		}
		current = freshSchemaBaseline
	}

	_, err = runMigrations(ctx, s.db, current, buildMigrations(migrationCtx{nodeName: nodeName}))
	return err
}

// runMigrations applies every migration whose version > current in order,
// returning the version the database ended at. Each migration writes its
// own schema_version row inside its transaction so version and schema stay
// atomic; this function only orchestrates iteration. Exposed within the
// package so tests can drive the registry with a custom slice.
func runMigrations(ctx context.Context, db *sql.DB, current int, ms []migration) (int, error) {
	prev := -1
	for _, m := range ms {
		if m.version <= prev {
			return current, fmt.Errorf("migrations registry out of order: v%d follows v%d", m.version, prev)
		}
		prev = m.version
		if m.version <= current {
			continue
		}
		if err := m.up(ctx, db); err != nil {
			return current, fmt.Errorf("migrate schema v%d→v%d: %w", current, m.version, err)
		}
		current = m.version
	}
	return current, nil
}

// CurrentSchemaVersion returns the version currently stored in the DB.
func (s *Store) CurrentSchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v)
	return v, err
}

// --- Fresh-DB baseline ---

func applyV5(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, q := range schemaV5Stmts() {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("exec %q: %w", q, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (5)`); err != nil {
		return fmt.Errorf("record schema v5: %w", err)
	}
	return tx.Commit()
}

// schemaV5Stmts returns the DDL for a fresh v5 database. The runs table
// gains a destination TEXT column and 'restore' as a valid kind; a CHECK
// constraint enforces that destination is set iff kind is 'sync' or
// 'restore'. The files table is unchanged from v4 — its (volume_id, path,
// blake3) PK still enforces the append-only content history.
func schemaV5Stmts() []string {
	return []string{
		`CREATE TABLE volumes (
			id   INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			path TEXT NOT NULL
		)`,
		`CREATE TABLE runs (
			id            INTEGER PRIMARY KEY,
			kind          TEXT NOT NULL CHECK (kind IN ('index','sync','restore')),
			volume_id     INTEGER REFERENCES volumes(id),
			destination   TEXT,
			started_at_ns INTEGER NOT NULL,
			ended_at_ns   INTEGER,
			status        TEXT NOT NULL CHECK (status IN ('running','success','failed','partial')),
			error         TEXT,
			file_count    INTEGER NOT NULL DEFAULT 0,
			CHECK (
				(kind = 'index' AND destination IS NULL) OR
				(kind IN ('sync','restore') AND destination IS NOT NULL AND destination != '')
			)
		)`,
		`CREATE INDEX idx_runs_volume_started ON runs(volume_id, started_at_ns)`,
		`CREATE INDEX idx_runs_destination ON runs(destination) WHERE destination IS NOT NULL`,
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

// --- v2 → v3 ---

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

// --- v3 → v4 ---

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

// --- v4 → v5 ---

// migrateV4ToV5 rebuilds the runs table to add the destination column,
// widen the kind CHECK to include 'restore', and add the kind↔destination
// coupling CHECK. Existing v4 rows are all kind='index' with a non-NULL
// volume_id, so they carry over verbatim with destination = NULL.
//
// Rebuild of a *parent* table referenced by FKs from another table follows
// the standard SQLite recipe: PRAGMA foreign_keys=OFF, rebuild, verify
// with foreign_key_check, then PRAGMA foreign_keys=ON. A single Conn is
// pinned so the PRAGMA applies to the same session that runs the
// migration transaction.
func migrateV4ToV5(ctx context.Context, db *sql.DB) error {
	conn, restore, err := disableForeignKeys(ctx, db)
	if err != nil {
		return err
	}
	defer restore()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := rebuildRunsTableV5(ctx, tx); err != nil {
		return err
	}
	if err := verifyForeignKeysClean(ctx, tx, "v4→v5"); err != nil {
		return err
	}
	return tx.Commit()
}

// disableForeignKeys pins a Conn, turns FK enforcement off on it, and
// returns a restore func the caller defers. The cleanup re-enables FKs
// and releases the Conn even if the migration fails partway through.
func disableForeignKeys(ctx context.Context, db *sql.DB) (*sql.Conn, func(), error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("acquire conn: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("disable foreign keys: %w", err)
	}
	return conn, func() {
		_, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
		conn.Close()
	}, nil
}

func rebuildRunsTableV5(ctx context.Context, tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE runs_v5 (
			id            INTEGER PRIMARY KEY,
			kind          TEXT NOT NULL CHECK (kind IN ('index','sync','restore')),
			volume_id     INTEGER REFERENCES volumes(id),
			destination   TEXT,
			started_at_ns INTEGER NOT NULL,
			ended_at_ns   INTEGER,
			status        TEXT NOT NULL CHECK (status IN ('running','success','failed','partial')),
			error         TEXT,
			file_count    INTEGER NOT NULL DEFAULT 0,
			CHECK (
				(kind = 'index' AND destination IS NULL) OR
				(kind IN ('sync','restore') AND destination IS NOT NULL AND destination != '')
			)
		)`,
		`INSERT INTO runs_v5 (
			id, kind, volume_id, destination,
			started_at_ns, ended_at_ns, status, error, file_count
		)
		SELECT id, kind, volume_id, NULL,
		       started_at_ns, ended_at_ns, status, error, file_count
		FROM runs`,
		`DROP TABLE runs`,
		`ALTER TABLE runs_v5 RENAME TO runs`,
		`CREATE INDEX idx_runs_volume_started ON runs(volume_id, started_at_ns)`,
		`CREATE INDEX idx_runs_destination ON runs(destination) WHERE destination IS NOT NULL`,
		`INSERT INTO schema_version (version) VALUES (5)`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("rebuild runs: %w", err)
		}
	}
	return nil
}

// verifyForeignKeysClean runs PRAGMA foreign_key_check inside tx — the
// explicit verification step the SQLite docs require when rebuilding with
// FKs off. Anything in violation surfaces here as a row, not at commit,
// so the transaction can be rolled back before damage spreads.
func verifyForeignKeysClean(ctx context.Context, tx *sql.Tx, label string) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("%s left dangling FK references; refusing to commit", label)
	}
	return nil
}

// --- v5 → v6 ---

// migrateV5ToV6 adds the node-sync foundations: the `nodes` table (with a
// self-row), the `peer_sync_state` watermark table, and the provenance
// columns on `files` and `runs`. ALTER TABLE ADD COLUMN is used for the
// column additions — the new columns are nullable with no default, so
// SQLite skips a full table rewrite and existing rows surface them as
// NULL ("local write" convention). The partial index on
// `files (source_node_id) WHERE status='present' AND source_node_id IS
// NOT NULL` lets the peer-sync planner answer "find rows sourced from
// peer X" cheaply: the NOT NULL clause excludes the local-write majority
// (which would otherwise dominate the index entries) so the index size
// tracks the peer-sourced subset, not every present row.
func migrateV5ToV6(ctx context.Context, db *sql.DB, nodeName string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`CREATE TABLE nodes (
			id                     INTEGER PRIMARY KEY,
			name                   TEXT NOT NULL UNIQUE,
			endpoint               TEXT,
			public_key_fingerprint TEXT
		)`,
		// last_shared_run_id is NOT a FK to local runs(id): it carries the
		// initiator's local run-id, which is the shared identifier between
		// the two nodes. The receiver records its own correlated id on
		// runs.correlated_run_id; FK-constraining the watermark to the
		// receiver's runs table would reject most updates.
		`CREATE TABLE peer_sync_state (
			volume_id          INTEGER NOT NULL REFERENCES volumes(id),
			peer_node_id       INTEGER NOT NULL REFERENCES nodes(id),
			last_shared_run_id INTEGER,
			last_synced_at     INTEGER NOT NULL,
			PRIMARY KEY (volume_id, peer_node_id)
		)`,
		`ALTER TABLE files ADD COLUMN source_node_id INTEGER REFERENCES nodes(id)`,
		`ALTER TABLE files ADD COLUMN source_run_id  INTEGER REFERENCES runs(id)`,
		`ALTER TABLE runs ADD COLUMN peer_node_id     INTEGER REFERENCES nodes(id)`,
		`ALTER TABLE runs ADD COLUMN correlated_run_id INTEGER`,
		`CREATE INDEX idx_files_source_node ON files(source_node_id)
		 WHERE status = 'present' AND source_node_id IS NOT NULL`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("exec %q: %w", q, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO nodes (name, endpoint, public_key_fingerprint) VALUES (?, NULL, NULL)`,
		nodeName); err != nil {
		return fmt.Errorf("insert self node row: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (6)`); err != nil {
		return fmt.Errorf("record schema v6: %w", err)
	}
	return tx.Commit()
}

// --- v6 → v7 ---

// migrateV6ToV7 rebuilds the runs table to widen the kind CHECK to
// include 'audit', the run-kind for periodic drift detection on
// daemon-hosted volumes (#17). 'audit' joins 'index' in the
// destination-NULL branch of the kind↔destination coupling CHECK —
// audits, like indexes, are scoped to one volume with no rclone
// target.
//
// Reuses the v4→v5 recipe (FK off, rebuild, foreign_key_check verify,
// FK on) because runs is referenced by files.first_seen_run_id and
// files.last_seen_run_id; an in-place ALTER would dangle those FKs
// during the rebuild.
func migrateV6ToV7(ctx context.Context, db *sql.DB) error {
	conn, restore, err := disableForeignKeys(ctx, db)
	if err != nil {
		return err
	}
	defer restore()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := rebuildRunsTableV7(ctx, tx); err != nil {
		return err
	}
	if err := verifyForeignKeysClean(ctx, tx, "v6→v7"); err != nil {
		return err
	}
	return tx.Commit()
}

func rebuildRunsTableV7(ctx context.Context, tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE runs_v7 (
			id            INTEGER PRIMARY KEY,
			kind          TEXT NOT NULL CHECK (kind IN ('index','sync','restore','audit')),
			volume_id     INTEGER REFERENCES volumes(id),
			destination   TEXT,
			started_at_ns INTEGER NOT NULL,
			ended_at_ns   INTEGER,
			status        TEXT NOT NULL CHECK (status IN ('running','success','failed','partial')),
			error         TEXT,
			file_count    INTEGER NOT NULL DEFAULT 0,
			peer_node_id      INTEGER REFERENCES nodes(id),
			correlated_run_id INTEGER,
			CHECK (
				(kind IN ('index','audit') AND destination IS NULL) OR
				(kind IN ('sync','restore') AND destination IS NOT NULL AND destination != '')
			)
		)`,
		`INSERT INTO runs_v7 (
			id, kind, volume_id, destination, started_at_ns, ended_at_ns,
			status, error, file_count, peer_node_id, correlated_run_id
		)
		SELECT id, kind, volume_id, destination, started_at_ns, ended_at_ns,
		       status, error, file_count, peer_node_id, correlated_run_id
		FROM runs`,
		`DROP TABLE runs`,
		`ALTER TABLE runs_v7 RENAME TO runs`,
		`CREATE INDEX idx_runs_volume_started ON runs(volume_id, started_at_ns)`,
		`CREATE INDEX idx_runs_destination ON runs(destination) WHERE destination IS NOT NULL`,
		`INSERT INTO schema_version (version) VALUES (7)`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("rebuild runs: %w", err)
		}
	}
	return nil
}

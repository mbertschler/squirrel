package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

// SchemaVersion is the schema version this binary writes and reads.
const SchemaVersion = 11

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
		{version: 8, up: migrateV7ToV8},
		{version: 9, up: migrateV8ToV9},
		{version: 10, up: migrateV9ToV10},
		{version: 11, up: migrateV10ToV11},
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
//
// Before any schema-advancing migration runs against an existing
// database, migrate takes an online snapshot via VACUUM INTO and lands
// it under opts.BackupDir (or "<dirname(s.path)>/backups" by default).
// The snapshot is the rollback surface if a migration commits bad
// state or walks over pre-existing corruption — the SQLite transaction
// would only roll back failed migrations, not buggy-but-successful
// ones. Fresh databases (current == 0) skip the snapshot because
// there's nothing to lose.
func (s *Store) migrate(ctx context.Context, nodeName string, opts OpenOptions) error {
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
	freshDB := current == 0
	if freshDB {
		if err := applyV5(ctx, s.db); err != nil {
			return fmt.Errorf("apply schema v%d: %w", freshSchemaBaseline, err)
		}
		current = freshSchemaBaseline
	}

	// Take a pre-migration snapshot only when an *existing* database is
	// about to step forward. A fresh DB has nothing worth snapshotting
	// (the applyV5 baseline plus the chain to SchemaVersion produces a
	// completely synthetic DB), and avoiding the snapshot keeps the
	// "open a temp DB for a test" path free of side files.
	if !freshDB && current < SchemaVersion && !opts.DisablePreMigrationBackup {
		if err := s.preMigrationBackup(ctx, current, opts.BackupDir); err != nil {
			return fmt.Errorf("pre-migration snapshot: %w", err)
		}
	}

	_, err = runMigrations(ctx, s.db, current, buildMigrations(migrationCtx{nodeName: nodeName}))
	return err
}

// preMigrationBackup writes a snapshot of the current DB to
// backupDir before the schema is advanced. The filename encodes the
// from/to versions and an ISO8601 timestamp so a backups/ directory
// can carry multiple snapshots (different upgrade points) without
// collisions.
func (s *Store) preMigrationBackup(ctx context.Context, fromVersion int, backupDir string) error {
	if s.path == "" {
		// migrate() was invoked without a path — e.g. in-memory test;
		// nothing to snapshot to.
		return nil
	}
	if backupDir == "" {
		backupDir = defaultBackupDir(s.path)
	}
	// Millisecond precision so two processes (e.g. CLI + agent) that
	// race through Open in the same second can't collide on the same
	// snapshot filename — Backup refuses to overwrite, and a collision
	// would make Open fail in a confusing way.
	name := fmt.Sprintf("pre-migration-v%d-to-v%d-%s.db",
		fromVersion, SchemaVersion,
		time.Now().UTC().Format("20060102T150405.000Z"))
	return s.Backup(ctx, filepath.Join(backupDir, name))
}

// defaultBackupDir returns "<dirname(dbPath)>/backups", the sibling
// directory the CLI uses for both pre-migration snapshots and manual
// `db backup` snapshots when --to is unset.
func defaultBackupDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "backups")
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
// agent-hosted volumes (#17). 'audit' joins 'index' in the
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

// --- v7 → v8 ---

// migrateV7ToV8 introduces the folders table for path-prefix dedup and
// per-folder Merkle hashes (#44). It (1) creates the folders table, (2)
// seeds one folder row per distinct directory path in the existing files
// table plus all ancestors up to the volume root, (3) rebuilds the files
// table to key off (folder_id, name) instead of (volume_id, path), and
// (4) backfills folder hashes bottom-up so the freshly migrated database
// is in the same shape a from-scratch v8 indexer would have produced.
//
// FK enforcement is disabled across the rebuild because files references
// folders and runs, and we drop the old files table mid-migration. The
// final foreign_key_check verifies no dangling refs slipped through
// before the transaction commits.
func migrateV7ToV8(ctx context.Context, db *sql.DB) error {
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

	if err := createFoldersTableV8(ctx, tx); err != nil {
		return err
	}
	if err := seedFoldersFromFilesV8(ctx, tx); err != nil {
		return err
	}
	if err := rebuildFilesV8(ctx, tx); err != nil {
		return err
	}
	if err := backfillFolderHashesV8(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (8)`); err != nil {
		return fmt.Errorf("record schema v8: %w", err)
	}
	if err := verifyForeignKeysClean(ctx, tx, "v7→v8"); err != nil {
		return err
	}
	return tx.Commit()
}

func createFoldersTableV8(ctx context.Context, tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE folders (
			id                  INTEGER PRIMARY KEY,
			volume_id           INTEGER NOT NULL REFERENCES volumes(id),
			parent_id           INTEGER REFERENCES folders(id),
			path                TEXT NOT NULL,
			shallow_blake3      BLOB CHECK (shallow_blake3 IS NULL OR length(shallow_blake3) = 32),
			deep_blake3         BLOB CHECK (deep_blake3    IS NULL OR length(deep_blake3)    = 32),
			last_changed_run_id INTEGER REFERENCES runs(id),
			UNIQUE (volume_id, path)
		)`,
		// parent_id is queried on every ancestor walk (hash bubble-up and
		// child-folder enumeration); without an index those become full
		// scans of the folders table.
		`CREATE INDEX idx_folders_parent ON folders(parent_id)`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("create folders: %w", err)
		}
	}
	return nil
}

// seedFoldersFromFilesV8 builds the full set of (volume_id, folder_path)
// tuples the new files table will reference, then inserts them with
// correct parent_id links. The set is the union of:
//
//   - every directory containing a file in v7 files
//   - every ancestor of those directories up to the volume root
//   - the volume root ("") for every volume that exists at all (so empty
//     volumes still have a hashable root)
//
// Insert order is by path length ascending so each row's parent already
// exists when the child INSERT runs.
// folderSeedKey is the local (volume_id, folder_path) tuple keyed in the
// seed phase. Kept package-scoped (lowercase) so the helpers below share
// a single named type rather than restating the literal at every call.
type folderSeedKey struct {
	volumeID int64
	path     string
}

func seedFoldersFromFilesV8(ctx context.Context, tx *sql.Tx) error {
	needed, err := collectNeededFolders(ctx, tx)
	if err != nil {
		return err
	}
	return insertSeededFolders(ctx, tx, needed)
}

// collectNeededFolders returns the full set of (volume_id, folder_path)
// tuples that the v8 files table will reference: every directory holding
// a v7 file, every ancestor up to the root, and the root itself for every
// volume (so empty volumes still have a hashable root).
func collectNeededFolders(ctx context.Context, tx *sql.Tx) (map[folderSeedKey]struct{}, error) {
	needed := map[folderSeedKey]struct{}{}
	if err := forEachVolume(ctx, tx, func(id int64) {
		needed[folderSeedKey{volumeID: id, path: ""}] = struct{}{}
	}); err != nil {
		return nil, err
	}
	if err := forEachDistinctFilePath(ctx, tx, func(volumeID int64, p string) {
		folderPath, _ := splitFilePath(p)
		for {
			needed[folderSeedKey{volumeID: volumeID, path: folderPath}] = struct{}{}
			if folderPath == "" {
				return
			}
			folderPath = parentFolderPath(folderPath)
		}
	}); err != nil {
		return nil, err
	}
	return needed, nil
}

// insertSeededFolders writes the rows in an order where every parent
// precedes its children, recording each row's id so the next sibling /
// child can wire parent_id correctly.
func insertSeededFolders(ctx context.Context, tx *sql.Tx, needed map[folderSeedKey]struct{}) error {
	keys := make([]folderSeedKey, 0, len(needed))
	for k := range needed {
		keys = append(keys, k)
	}
	// Path length is a strict order on ancestry (every ancestor has a
	// shorter path); secondary keys give a deterministic insert order.
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if la, lb := len(a.path), len(b.path); la != lb {
			return la < lb
		}
		if a.volumeID != b.volumeID {
			return a.volumeID < b.volumeID
		}
		return a.path < b.path
	})

	ids := make(map[folderSeedKey]int64, len(keys))
	for _, k := range keys {
		var parent sql.NullInt64
		if k.path != "" {
			pk := folderSeedKey{volumeID: k.volumeID, path: parentFolderPath(k.path)}
			parent = sql.NullInt64{Int64: ids[pk], Valid: true}
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO folders (volume_id, parent_id, path, shallow_blake3, deep_blake3)
			 VALUES (?, ?, ?, NULL, NULL)`,
			k.volumeID, parent, k.path)
		if err != nil {
			return fmt.Errorf("insert folder (volume=%d path=%q): %w", k.volumeID, k.path, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("last insert id for folder: %w", err)
		}
		ids[k] = id
	}
	return nil
}

func forEachVolume(ctx context.Context, tx *sql.Tx, fn func(id int64)) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM volumes`)
	if err != nil {
		return fmt.Errorf("list volumes for folder seed: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan volume id: %w", err)
		}
		fn(id)
	}
	return rows.Err()
}

func forEachDistinctFilePath(ctx context.Context, tx *sql.Tx, fn func(volumeID int64, path string)) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT volume_id, path FROM files`)
	if err != nil {
		return fmt.Errorf("list distinct file paths: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v int64
		var p string
		if err := rows.Scan(&v, &p); err != nil {
			return fmt.Errorf("scan file path: %w", err)
		}
		fn(v, p)
	}
	return rows.Err()
}

// rebuildFilesV8 creates the new files schema (folder_id + name) and
// copies every row from the v7 table into it, resolving the folder_id
// via a JOIN on the freshly seeded folders. Indexes are recreated
// matching v7 semantics but rekeyed to (folder_id, name).
func rebuildFilesV8(ctx context.Context, tx *sql.Tx) error {
	if err := createFilesV8Stage(ctx, tx); err != nil {
		return err
	}
	if err := copyFilesIntoV8(ctx, tx); err != nil {
		return err
	}
	return finishFilesV8Rebuild(ctx, tx)
}

func createFilesV8Stage(ctx context.Context, tx *sql.Tx) error {
	const ddl = `CREATE TABLE files_v8 (
		folder_id         INTEGER NOT NULL REFERENCES folders(id),
		name              TEXT NOT NULL,
		blake3            BLOB NOT NULL CHECK (length(blake3) = 32),
		size_bytes        INTEGER NOT NULL,
		mtime_ns          INTEGER NOT NULL,
		status            TEXT NOT NULL CHECK (status IN ('present','missing','superseded')),
		first_seen_run_id INTEGER NOT NULL REFERENCES runs(id),
		last_seen_run_id  INTEGER NOT NULL REFERENCES runs(id),
		indexed_at_ns     INTEGER NOT NULL,
		source_node_id    INTEGER REFERENCES nodes(id),
		source_run_id     INTEGER REFERENCES runs(id),
		PRIMARY KEY (folder_id, name, blake3)
	)`
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("create files_v8: %w", err)
	}
	return nil
}

// copyFilesIntoV8 reads every v7 row into memory, then writes each one
// into files_v8 with the resolved folder_id. The folder lookup runs in
// Go because SQLite has no portable last-slash helper; this also keeps
// the migration easy to reason about against test fixtures.
func copyFilesIntoV8(ctx context.Context, tx *sql.Tx) error {
	batch, err := readV7Files(ctx, tx)
	if err != nil {
		return err
	}
	for _, r := range batch {
		folderPath, name := splitFilePath(r.path)
		var folderID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM folders WHERE volume_id = ? AND path = ?`,
			r.volumeID, folderPath).Scan(&folderID); err != nil {
			return fmt.Errorf("lookup folder for (volume=%d path=%q): %w", r.volumeID, r.path, err)
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO files_v8 (folder_id, name, blake3, size_bytes, mtime_ns,
				status, first_seen_run_id, last_seen_run_id, indexed_at_ns,
				source_node_id, source_run_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			folderID, name, r.blake3, r.sizeBytes, r.mtimeNs, r.status,
			r.firstSeen, r.lastSeen, r.indexedAt, r.sourceNodeID, r.sourceRunID)
		if err != nil {
			return fmt.Errorf("copy file row (volume=%d path=%q): %w", r.volumeID, r.path, err)
		}
	}
	return nil
}

// v7FileRow is the in-memory shape of a single v7 files row. The
// transition is read-then-rewrite so capturing every column from the old
// schema is the simplest path.
type v7FileRow struct {
	volumeID     int64
	path         string
	blake3       []byte
	sizeBytes    int64
	mtimeNs      int64
	status       string
	firstSeen    int64
	lastSeen     int64
	indexedAt    int64
	sourceNodeID sql.NullInt64
	sourceRunID  sql.NullInt64
}

func readV7Files(ctx context.Context, tx *sql.Tx) ([]v7FileRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT volume_id, path, blake3, size_bytes, mtime_ns,
		status, first_seen_run_id, last_seen_run_id, indexed_at_ns, source_node_id, source_run_id
		FROM files`)
	if err != nil {
		return nil, fmt.Errorf("scan v7 files: %w", err)
	}
	defer rows.Close()
	var batch []v7FileRow
	for rows.Next() {
		var r v7FileRow
		if err := rows.Scan(&r.volumeID, &r.path, &r.blake3, &r.sizeBytes, &r.mtimeNs,
			&r.status, &r.firstSeen, &r.lastSeen, &r.indexedAt, &r.sourceNodeID, &r.sourceRunID); err != nil {
			return nil, fmt.Errorf("scan v7 row: %w", err)
		}
		batch = append(batch, r)
	}
	return batch, rows.Err()
}

// finishFilesV8Rebuild swaps the staged table into place and recreates
// every index + trigger the v7 files table carried, rekeyed to
// (folder_id, name) where applicable.
func finishFilesV8Rebuild(ctx context.Context, tx *sql.Tx) error {
	tail := []string{
		`DROP TABLE files`,
		`ALTER TABLE files_v8 RENAME TO files`,
		`CREATE INDEX idx_files_blake3 ON files(blake3, folder_id, name)`,
		`CREATE INDEX idx_files_missing ON files(folder_id, name) WHERE status = 'missing'`,
		`CREATE UNIQUE INDEX uniq_files_live_per_path ON files(folder_id, name) WHERE status != 'superseded'`,
		`CREATE INDEX idx_files_source_node ON files(source_node_id)
		 WHERE status = 'present' AND source_node_id IS NOT NULL`,
		`CREATE TRIGGER files_blake3_immutable BEFORE UPDATE OF blake3 ON files
		 BEGIN
		     SELECT RAISE(ABORT, 'blake3 is immutable; supersede the row and insert a new one');
		 END`,
	}
	for _, q := range tail {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("finish files rebuild: %w", err)
		}
	}
	return nil
}

// backfillFolderHashesV8 visits every folder in length-descending order
// (leaves first) and writes its shallow + deep digests. By the time the
// loop reaches an ancestor, every child's deep_blake3 is already
// populated, so the deep computation is a single pass with no recursion.
// last_changed_run_id stays NULL — there is no run that "caused" the
// migration's content, so claiming one would falsify the audit log.
//
// The compute and write happen via v8-local helpers (computeShallowV8 /
// computeDeepV8 / writeFolderHashesV8) so the migration doesn't read or
// write columns that only exist from v9 onwards.
func backfillFolderHashesV8(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM folders ORDER BY length(path) DESC, path DESC`)
	if err != nil {
		return fmt.Errorf("list folders for backfill: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan folder id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, id := range ids {
		shallow, err := computeShallowV8(ctx, tx, id)
		if err != nil {
			return err
		}
		deep, err := computeDeepV8(ctx, tx, id, shallow)
		if err != nil {
			return err
		}
		if err := writeFolderHashesV8(ctx, tx, id, shallow, deep); err != nil {
			return err
		}
	}
	return nil
}

// computeShallowV8 mirrors computeShallowAndDirectAggregatesTx but reads
// only the columns that exist in v8 (no file_count / cumulative_size on
// folders, no aggregate return values). Lives inside the migration file
// so v8 backfill doesn't depend on post-v8 helper signatures.
func computeShallowV8(ctx context.Context, tx *sql.Tx, folderID int64) ([]byte, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT name, blake3 FROM files
		 WHERE folder_id = ? AND status = 'present'
		 ORDER BY name`,
		folderID)
	if err != nil {
		return nil, fmt.Errorf("read folder %d files: %w", folderID, err)
	}
	defer rows.Close()
	var entries []ShallowEntry
	for rows.Next() {
		var e ShallowEntry
		if err := rows.Scan(&e.Name, &e.Blake3); err != nil {
			return nil, fmt.Errorf("scan folder %d files: %w", folderID, err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ComputeShallowHash(entries), nil
}

func computeDeepV8(ctx context.Context, tx *sql.Tx, folderID int64, shallow []byte) ([]byte, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT path, deep_blake3 FROM folders
		 WHERE parent_id = ?
		 ORDER BY path`,
		folderID)
	if err != nil {
		return nil, fmt.Errorf("read child folders of %d: %w", folderID, err)
	}
	defer rows.Close()
	var children []ChildFolder
	emptyDeep := ComputeShallowHash(nil)
	for rows.Next() {
		var path string
		var deep []byte
		if err := rows.Scan(&path, &deep); err != nil {
			return nil, fmt.Errorf("scan child folders of %d: %w", folderID, err)
		}
		if deep == nil {
			deep = emptyDeep
		}
		children = append(children, ChildFolder{Name: folderName(path), DeepBlake3: deep})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ComputeDeepHash(shallow, children), nil
}

func writeFolderHashesV8(ctx context.Context, tx *sql.Tx, folderID int64, shallow, deep []byte) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE folders SET shallow_blake3 = ?, deep_blake3 = ?, last_changed_run_id = NULL
		 WHERE id = ?`,
		shallow, deep, folderID)
	if err != nil {
		return fmt.Errorf("update folder %d hashes: %w", folderID, err)
	}
	return nil
}

// migrateV8ToV9 adds two cumulative aggregates to the folders table —
// file_count and cumulative_size — over the live (status='present') file
// set within the folder and all of its descendants. They power the
// ncdu-style desktop browser without re-summing the files table on
// every navigation; they are maintained in steady state by the same
// closure walk that re-folds the Merkle hashes (see
// recomputeOneFolder in folders.go).
//
// The migration is additive: two NOT NULL DEFAULT 0 columns, then a
// single leaves-first backfill pass that mirrors the v8 hash backfill.
// last_changed_run_id is left untouched — the aggregates were always
// "the same content" the v8 folder row already described, just
// uncounted, so no run id can honestly take credit.
func migrateV8ToV9(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`ALTER TABLE folders ADD COLUMN file_count      INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE folders ADD COLUMN cumulative_size INTEGER NOT NULL DEFAULT 0`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("v8→v9 add column: %w", err)
		}
	}
	if err := backfillFolderAggregatesV9(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (9)`); err != nil {
		return fmt.Errorf("record schema v9: %w", err)
	}
	return tx.Commit()
}

// backfillFolderAggregatesV9 walks every folder in leaves-first order
// and writes its (file_count, cumulative_size) as
// direct-present-files-in-folder + sum-of-children's-aggregates. The
// ordering is identical to backfillFolderHashesV8 so each parent's
// children are already populated when it runs.
func backfillFolderAggregatesV9(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM folders ORDER BY length(path) DESC, path DESC`)
	if err != nil {
		return fmt.Errorf("list folders for aggregate backfill: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan folder id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, id := range ids {
		var directCount, directSize int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM files
			 WHERE folder_id = ? AND status = 'present'`,
			id,
		).Scan(&directCount, &directSize); err != nil {
			return fmt.Errorf("aggregate folder %d files: %w", id, err)
		}
		var childCount, childSize int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(file_count), 0), COALESCE(SUM(cumulative_size), 0)
			 FROM folders WHERE parent_id = ?`,
			id,
		).Scan(&childCount, &childSize); err != nil {
			return fmt.Errorf("aggregate folder %d children: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE folders SET file_count = ?, cumulative_size = ? WHERE id = ?`,
			directCount+childCount, directSize+childSize, id,
		); err != nil {
			return fmt.Errorf("update folder %d aggregates: %w", id, err)
		}
	}
	return nil
}

// migrateV9ToV10 adds a nullable `shallow` flag to the runs table. The
// flag is meaningful for index and audit runs — it records whether the
// run took the (size, mtime) shortcut instead of rehashing every file —
// and is left NULL for sync/restore (where the concept doesn't apply)
// and for the history of pre-v10 rows (where the choice wasn't
// recorded). A CHECK constraint keeps the column to 0/1/NULL so the
// nullable bool semantics stay legible from raw SQL.
func migrateV9ToV10(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`ALTER TABLE runs ADD COLUMN shallow INTEGER CHECK (shallow IS NULL OR shallow IN (0, 1))`,
		`INSERT INTO schema_version (version) VALUES (10)`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("v9→v10: %w", err)
		}
	}
	return tx.Commit()
}

// migrateV10ToV11 adds the hook_runs table: the generic outcome record
// for the per-volume external-tool hooks (#84). A hook run is squirrel
// exec'ing a user-configured command on one of two triggers — 'change'
// (after a successful index run that settled content) or 'interval' (on
// a cadence, regardless of change) — and recording only the tool-agnostic
// result (exit code, timestamps, the triggering run). squirrel never
// parses what the command did, so the table carries no tool-specific
// columns.
//
// The table is additive and references existing parents (volumes, runs);
// no existing row is rewritten, so the content-immutability invariant on
// `files` is untouched. triggering_run_id is a nullable FK to runs(id):
// on-change hooks carry the index run that fired them; interval hooks
// leave it NULL because no run triggered them.
func migrateV10ToV11(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		// The trigger↔triggering_run_id coupling mirrors the runs table's
		// kind↔destination CHECK: a 'change' hook is always fired by an index
		// run (so it carries that run id), while an 'interval' hook fires on
		// the clock with no run behind it (so the id is NULL). Encoding it in
		// the schema keeps the RUN column in `squirrel hooks`/TUI honest and
		// turns a wiring bug into a loud failure instead of a NULL.
		`CREATE TABLE hook_runs (
			id                INTEGER PRIMARY KEY,
			volume_id         INTEGER NOT NULL REFERENCES volumes(id),
			trigger           TEXT NOT NULL CHECK (trigger IN ('change','interval')),
			triggering_run_id INTEGER REFERENCES runs(id),
			changed           INTEGER NOT NULL CHECK (changed IN (0, 1)),
			started_at_ns     INTEGER NOT NULL,
			ended_at_ns       INTEGER,
			status            TEXT NOT NULL CHECK (status IN ('running','success','failed')),
			exit_code         INTEGER,
			error             TEXT,
			CHECK (
				(trigger = 'change'   AND triggering_run_id IS NOT NULL) OR
				(trigger = 'interval' AND triggering_run_id IS NULL)
			)
		)`,
		// The cadence math (interval-hook due check) and the status surface
		// both read the latest hook run per (volume, trigger); index the
		// pair the queries order by.
		`CREATE INDEX idx_hook_runs_volume_trigger ON hook_runs(volume_id, trigger, started_at_ns)`,
		`INSERT INTO schema_version (version) VALUES (11)`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("v10→v11: %w", err)
		}
	}
	return tx.Commit()
}

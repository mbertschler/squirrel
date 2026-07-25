package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestSchemaIsAllStrict makes the AGENTS.md convention executable: every
// table in a database migrated to SchemaVersion must be STRICT. It covers
// the v26→v27 bulk conversion and, from here on, any new migration that
// forgets the keyword on a CREATE TABLE.
func TestSchemaIsAllStrict(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rows, err := s.db.QueryContext(ctx, `
		SELECT name, strict FROM pragma_table_list
		WHERE schema = 'main' AND type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		t.Fatalf("pragma_table_list: %v", err)
	}
	defer rows.Close()

	var loose []string
	tables := 0
	for rows.Next() {
		var name string
		var strict int
		if err := rows.Scan(&name, &strict); err != nil {
			t.Fatalf("scan table row: %v", err)
		}
		tables++
		if strict != 1 {
			loose = append(loose, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	if tables == 0 {
		t.Fatal("no tables found in a migrated database")
	}
	if len(loose) > 0 {
		t.Errorf("non-STRICT tables at v%d: %s — declare new tables STRICT (AGENTS.md, Schema & migrations)",
			SchemaVersion, strings.Join(loose, ", "))
	}
}

// TestStrictRebuildsCoverEveryV26Table checks the conversion list against
// the database it converts: the tables it rebuilds must be exactly the
// non-STRICT tables a v26 database carries. A table added before v27 and
// forgotten in the list would show up here as a missing entry, and a stale
// entry for a table that no longer exists as an extra one.
func TestStrictRebuildsCoverEveryV26Table(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	db := buildPopulatedV26DB(t, dsn)
	defer db.Close()

	rows, err := db.QueryContext(context.Background(), `
		SELECT name FROM pragma_table_list
		WHERE schema = 'main' AND type = 'table' AND strict = 0 AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		t.Fatalf("pragma_table_list: %v", err)
	}
	defer rows.Close()
	var loose []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		loose = append(loose, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}

	var specced []string
	for _, r := range strictRebuildsV27() {
		specced = append(specced, r.table)
	}
	sort.Strings(loose)
	sort.Strings(specced)
	if !equalStrings(specced, loose) {
		t.Errorf("conversion list = %v,\nnon-STRICT tables at v26 = %v", specced, loose)
	}
}

// TestMigrateV26ToV27PreservesEveryRow drives the real migration chain to
// v26, seeds every table, then opens with the current binary so v26→v27
// runs against a fully populated database. It pins what a whole-schema
// rebuild must not lose: every row of every table, the ids rows are keyed
// by, blob values byte-for-byte, and the full set of indexes and triggers.
func TestMigrateV26ToV27PreservesEveryRow(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db := buildPopulatedV26DB(t, dsn)
	countsBefore := tableRowCounts(t, db)
	objectsBefore := schemaObjectNames(t, db)
	if err := db.Close(); err != nil {
		t.Fatalf("close v26 db: %v", err)
	}

	s, err := OpenWithOptions(dsn, OpenOptions{DisablePreMigrationBackup: true})
	if err != nil {
		t.Fatalf("Open (migrates v26→v27): %v", err)
	}
	defer s.Close()
	if v, _ := s.CurrentSchemaVersion(ctx); v != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", v, SchemaVersion)
	}

	countsAfter := tableRowCounts(t, s.db)
	for table, before := range countsBefore {
		if before == 0 {
			t.Errorf("fixture left %s empty — the rebuild of an empty table proves nothing", table)
		}
		want := before
		if table == "schema_version" {
			// The v27 migration records its own version in the table it
			// just rebuilt, and Open carries on to SchemaVersion — one
			// row per step from the v26 fixture.
			want += int64(SchemaVersion - 26)
		}
		if after := countsAfter[table]; after != want {
			t.Errorf("%s rows = %d after migration, want %d", table, after, want)
		}
	}
	if got, want := len(countsAfter), len(countsBefore); got != want {
		t.Errorf("table count = %d after migration, want %d", got, want)
	}
	if after := schemaObjectNames(t, s.db); !equalStrings(after, objectsBefore) {
		t.Errorf("indexes/triggers after migration = %v, want %v", after, objectsBefore)
	}

	assertContentRowIntact(t, s)
	assertFilesRowsIntact(t, s)

	// Every FK still resolves: the rebuild ran with enforcement off, so
	// this is the check that the id-preserving copies kept the graph whole.
	if violations := countFKViolations(t, s.db); violations != 0 {
		t.Errorf("foreign_key_check reported %d violations after migration", violations)
	}
}

// countFKViolations returns how many rows PRAGMA foreign_key_check reports.
// Zero rows is a clean graph; the row contents don't matter here, only that
// iteration itself succeeded — a swallowed iteration error would read as
// "clean".
func countFKViolations(t *testing.T, db *sql.DB) int {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	violations := 0
	for rows.Next() {
		violations++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate foreign_key_check rows: %v", err)
	}
	return violations
}

// assertContentRowIntact checks the content entity survived the rebuild
// byte-exactly: the id↔hash binding, the 32-byte blob's storage class, and
// the size. Losing any of these would break the core principle — a hash
// once observed stays retrievable by the id everything else references.
func assertContentRowIntact(t *testing.T, s *Store) {
	t.Helper()
	var hash []byte
	var size int64
	var typ string
	err := s.db.QueryRowContext(context.Background(),
		`SELECT blake3, size_bytes, typeof(blake3) FROM contents WHERE id = 1`).Scan(&hash, &size, &typ)
	if err != nil {
		t.Fatalf("read contents id=1: %v", err)
	}
	if !bytes.Equal(hash, fixtureHash(0xa1)) {
		t.Errorf("contents.blake3 = %x, want %x", hash, fixtureHash(0xa1))
	}
	if size != 1000 {
		t.Errorf("contents.size_bytes = %d, want 1000", size)
	}
	if typ != "blob" {
		t.Errorf("typeof(contents.blake3) = %q, want blob", typ)
	}
}

// assertFilesRowsIntact checks the path↔content observations came across
// whole: the live row and the superseded predecessor at the same path in
// the root folder, plus the missing row in the child folder. The full
// (folder_id, name, content_id, status) tuple is compared — the binding of
// a path to a *particular* content is the observation, so a swapped
// content_id would be exactly the kind of loss this migration must not
// cause.
func assertFilesRowsIntact(t *testing.T, s *Store) {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT folder_id, name, content_id, status FROM files ORDER BY folder_id, name, content_id`)
	if err != nil {
		t.Fatalf("read files: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var folderID, contentID int64
		var name, status string
		if err := rows.Scan(&folderID, &name, &contentID, &status); err != nil {
			t.Fatalf("scan files row: %v", err)
		}
		got = append(got, fmt.Sprintf("folder=%d name=%s content=%d status=%s", folderID, name, contentID, status))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate files rows: %v", err)
	}
	// Ordered by (folder_id, name, content_id): a.txt's superseded
	// predecessor (content 1) precedes its live row (content 2).
	want := []string{
		"folder=1 name=a.txt content=1 status=superseded",
		"folder=1 name=a.txt content=2 status=present",
		"folder=2 name=gone.txt content=1 status=missing",
	}
	if !equalStrings(got, want) {
		t.Errorf("files rows = %v, want %v", got, want)
	}
}

// TestMigrateV26ToV27KeepsSchemaGuarantees verifies the rebuilt tables
// still enforce what they enforced at v26 — the append-only triggers on
// contents and the one-live-row-per-path partial unique index on files —
// and that STRICT now rejects a value whose storage class doesn't match its
// column, which is the whole point of the conversion.
func TestMigrateV26ToV27KeepsSchemaGuarantees(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db := buildPopulatedV26DB(t, dsn)
	if err := db.Close(); err != nil {
		t.Fatalf("close v26 db: %v", err)
	}
	s, err := OpenWithOptions(dsn, OpenOptions{DisablePreMigrationBackup: true})
	if err != nil {
		t.Fatalf("Open (migrates v26→v27): %v", err)
	}
	defer s.Close()

	refusals := []struct {
		what string
		sql  string
		args []any
	}{
		{"delete from contents", `DELETE FROM contents WHERE id = 1`, nil},
		{"update contents", `UPDATE contents SET size_bytes = 7 WHERE id = 1`, nil},
		{
			"second live row at one path",
			`INSERT INTO files (folder_id, name, content_id, mtime_ns, status,
				first_seen_run_id, last_seen_run_id, indexed_at_ns)
			 VALUES (1, 'a.txt', 3, 999, 'present', 1, 1, 999)`,
			nil,
		},
		{
			"TEXT bound into an INTEGER column",
			`INSERT INTO files (folder_id, name, content_id, mtime_ns, status,
				first_seen_run_id, last_seen_run_id, indexed_at_ns)
			 VALUES (1, 'strict.txt', 3, ?, 'present', 1, 1, 999)`,
			[]any{"not-a-timestamp"},
		},
		{"TEXT bound into contents.size_bytes", `INSERT INTO contents (blake3, size_bytes) VALUES (?, ?)`,
			[]any{fixtureHash(0xc3), "huge"}},
	}
	for _, r := range refusals {
		if _, err := s.db.ExecContext(ctx, r.sql, r.args...); err == nil {
			t.Errorf("%s was accepted after migration; want refusal", r.what)
		}
	}

	// A well-typed insert still works — the conversion tightened types, it
	// didn't wall the tables off.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO contents (blake3, size_bytes) VALUES (?, ?)`, fixtureHash(0xd4), 4096); err != nil {
		t.Errorf("well-typed insert rejected after migration: %v", err)
	}
}

// buildPopulatedV26DB creates a database at v26 through the real migration
// chain — not a hand-written fixture, so the shape is exactly what a v26
// binary left behind — and seeds every table with at least one row. It
// returns the open handle; the caller closes it before reopening through
// Open to trigger the v26→v27 migration.
func buildPopulatedV26DB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := openSQLite(buildDSN(dsn))
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}
	// Non-STRICT, the way every pre-v27 binary bootstrapped it, so the
	// migration's rebuild of schema_version itself is exercised.
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`); err != nil {
		t.Fatalf("create schema_version: %v", err)
	}
	if err := applyV5(ctx, db); err != nil {
		t.Fatalf("applyV5: %v", err)
	}
	var through []migration
	for _, m := range buildMigrations(migrationCtx{nodeName: "v26-self"}) {
		if m.version <= 26 {
			through = append(through, m)
		}
	}
	end, err := runMigrations(ctx, db, freshSchemaBaseline, through)
	if err != nil {
		t.Fatalf("migrate to v26: %v", err)
	}
	if end != 26 {
		t.Fatalf("chain stopped at v%d, want v26", end)
	}
	for _, stmt := range v26Seed() {
		if _, err := db.ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed %q: %v", stmt.sql, err)
		}
	}
	return db
}

type seedStmt struct {
	sql  string
	args []any
}

// v26Seed populates every table a v26 database carries, in FK-safe order:
// two nodes (the self row is already seeded by v5→v6), a volume with a
// folder tree, two contents with three files observations across them, run
// history and audit rows, a pack and its member, remote upload
// fingerprints, the peer and destination watermarks with their history, and
// the two standing-state latches. Each table ends up non-empty so the
// row-count comparison across the migration is meaningful everywhere.
func v26Seed() []seedStmt {
	hashA, hashB := fixtureHash(0xa1), fixtureHash(0xb2)
	return []seedStmt{
		{sql: `INSERT INTO volumes (id, name, path) VALUES (1, 'photos', '/vol/photos')`},
		{sql: `INSERT INTO nodes (id, name, endpoint, public_key_fingerprint) VALUES (2, 'nas', 'nas:9000', 'fp:abc')`},
		{sql: `INSERT INTO runs (id, kind, volume_id, destination, started_at_ns, ended_at_ns, status, file_count, shallow)
			VALUES (1, 'index', 1, NULL, 100, 200, 'success', 3, 0)`},
		{sql: `INSERT INTO runs (id, kind, volume_id, destination, started_at_ns, ended_at_ns, status, error, file_count, peer_node_id, correlated_run_id)
			VALUES (2, 'sync', 1, 'nas', 300, 400, 'partial', 'one object failed', 2, 2, 7)`},
		{sql: `INSERT INTO folders (id, volume_id, parent_id, path, shallow_blake3, deep_blake3, last_changed_run_id, file_count, cumulative_size)
			VALUES (1, 1, NULL, '', ?, ?, 1, 2, 3000)`, args: []any{hashA, hashB}},
		{sql: `INSERT INTO folders (id, volume_id, parent_id, path, last_changed_run_id, file_count, cumulative_size)
			VALUES (2, 1, 1, 'sub', 1, 1, 0)`},
		{sql: `INSERT INTO contents (id, blake3, size_bytes, origin_node_id, origin_run_id) VALUES (1, ?, 1000, NULL, 1)`, args: []any{hashA}},
		{sql: `INSERT INTO contents (id, blake3, size_bytes, origin_node_id, origin_run_id) VALUES (2, ?, 2000, 2, 7)`, args: []any{hashB}},
		{sql: `INSERT INTO files (folder_id, name, content_id, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns, status_changed_run_id)
			VALUES (1, 'a.txt', 1, 111, 'superseded', 1, 1, 120, 1)`},
		{sql: `INSERT INTO files (folder_id, name, content_id, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns, status_changed_run_id)
			VALUES (1, 'a.txt', 2, 222, 'present', 1, 1, 230, NULL)`},
		{sql: `INSERT INTO files (folder_id, name, content_id, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns, status_changed_run_id)
			VALUES (2, 'gone.txt', 1, 333, 'missing', 1, 1, 340, 1)`},
		{sql: `INSERT INTO runs_audit (id, run_id, transition, operator, at_ns, note) VALUES (1, 2, 'running→partial', 'alice', 410, 'retried')`},
		{sql: `INSERT INTO hook_runs (id, volume_id, trigger, triggering_run_id, changed, started_at_ns, ended_at_ns, status, exit_code, error)
			VALUES (1, 1, 'change', 1, 1, 250, 260, 'success', 0, NULL)`},
		{sql: `INSERT INTO hook_runs (id, volume_id, trigger, triggering_run_id, changed, started_at_ns, status)
			VALUES (2, 1, 'interval', NULL, 0, 270, 'running')`},
		{sql: `INSERT INTO packs (id, pack_key, size_bytes, member_count, created_run_id) VALUES (1, ?, 512, 1, 2)`, args: []any{fixtureHash(0xe5)}},
		{sql: `INSERT INTO pack_members (content_id, pack_id, byte_offset, byte_length) VALUES (1, 1, 0, 1000)`},
		{sql: `INSERT INTO remote_objects (content_id, destination, uploaded_run_id, checksum_algo, checksum, verified_at_ns)
			VALUES (2, 'nas', 2, 'sha256', 'deadbeef', 420)`},
		{sql: `INSERT INTO remote_packs (pack_id, destination, uploaded_run_id, checksum_algo, checksum, verified_at_ns)
			VALUES (1, 'nas', 2, NULL, NULL, NULL)`},
		{sql: `INSERT INTO peer_sync_state (volume_id, peer_node_id, last_shared_run_id, last_synced_at) VALUES (1, 2, 7, 400)`},
		{sql: `INSERT INTO peer_sync_state_history (id, volume_id, peer_node_id, last_shared_run_id, last_synced_at, at_ns)
			VALUES (1, 1, 2, 7, 400, 401)`},
		{sql: `INSERT INTO destination_run_ids (volume_id, destination, origin_node_id, origin_run_id, updated_at_ns, verify_method, source_node_id, verified_at_ns)
			VALUES (1, 'nas', 1, 2, 400, 'checksum', 2, 405)`},
		{sql: `INSERT INTO destination_run_ids_history (id, volume_id, destination, origin_node_id, origin_run_id, at_ns, verify_method, source_node_id)
			VALUES (1, 1, 'nas', 1, 2, 400, 'checksum', 2)`},
		{sql: `INSERT INTO destination_push_freshness (volume_id, destination, origin_node_id, origin_run_id, updated_at_ns)
			VALUES (1, 'nas', 1, 2, 400)`},
		{sql: `INSERT INTO destination_alarms (destination, kind, detail, raised_run_id, raised_at_ns)
			VALUES ('nas', 'verify-mismatch', 'checksum differs', 2, 430)`},
		{sql: `INSERT INTO contested_paths (volume_id, path, live_blake3, preserved_blake3, preserved_at_path, peer_node_id, raised_run_id, raised_at_ns)
			VALUES (1, 'a.txt', ?, ?, '.squirrel-conflicts/run-2/a.txt', 2, 2, 440)`, args: []any{hashB, hashA}},
	}
}

// fixtureHash returns a deterministic 32-byte hash-shaped blob, satisfying
// the length CHECK on every hash column.
func fixtureHash(fill byte) []byte {
	return bytes.Repeat([]byte{fill}, 32)
}

// tableRowCounts returns row counts per table for every table in the main
// schema, so a rebuild can be compared table-by-table rather than trusting
// a spot check.
func tableRowCounts(t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table names: %v", err)
	}

	counts := make(map[string]int64, len(tables))
	for _, table := range tables {
		var n int64
		// table names come from sqlite_master, not from input.
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+table+`"`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = n
	}
	return counts
}

// schemaObjectNames returns the sorted names of every index and trigger the
// schema declares, excluding the ones SQLite auto-creates for PRIMARY KEY /
// UNIQUE constraints (those carry a NULL sql and follow their table).
func schemaObjectNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
		SELECT name FROM sqlite_master
		WHERE type IN ('index', 'trigger') AND sql IS NOT NULL
		ORDER BY name`)
	if err != nil {
		t.Fatalf("list schema objects: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan object name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema objects: %v", err)
	}
	sort.Strings(names)
	return names
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

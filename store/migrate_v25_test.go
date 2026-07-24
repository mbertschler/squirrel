package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrateV24ToV25PreservesRunsAndWidensStatus builds a minimal v24
// database carrying runs rows in every pre-v25 terminal state plus an
// in-flight 'running' row, and a child row with a foreign key into runs,
// then migrates to v25. It pins the highest-risk part of the migration —
// the FK-off runs-table rebuild — proving it copies every row verbatim
// (id and status preserved), keeps a referencing FK resolving to the
// rebuilt table (the property the migration's own foreign_key_check
// guards on a full DB), and widens the status CHECK to admit 'refused'
// and 'aborted' without admitting a bogus status.
func TestMigrateV24ToV25PreservesRunsAndWidensStatus(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	v24DDL := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, endpoint TEXT, public_key_fingerprint TEXT)`,
		minimalRunsDDL,
		// A child table with a real FK into runs proves the id-preserving
		// rebuild keeps referencing rows resolving after runs is dropped
		// and recreated under FK-off.
		`CREATE TABLE run_child (id INTEGER PRIMARY KEY, run_id INTEGER NOT NULL REFERENCES runs(id))`,
		`INSERT INTO schema_version (version) VALUES (24)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'v', '/v')`,
		`INSERT INTO nodes (id, name) VALUES (1, 'self')`,
		`INSERT INTO runs (id, kind, volume_id, destination, started_at_ns, ended_at_ns, status, error, file_count)
			VALUES (1, 'index', 1, NULL, 100, 200, 'success', NULL, 5)`,
		`INSERT INTO runs (id, kind, volume_id, destination, started_at_ns, ended_at_ns, status, error, file_count)
			VALUES (2, 'sync', 1, 'bucket', 110, 210, 'failed', 'boom', 0)`,
		`INSERT INTO runs (id, kind, volume_id, destination, started_at_ns, ended_at_ns, status, error, file_count)
			VALUES (3, 'sync', 1, 'bucket', 120, 220, 'partial', NULL, 3)`,
		`INSERT INTO runs (id, kind, volume_id, destination, started_at_ns, status, file_count)
			VALUES (4, 'index', 1, NULL, 130, 'running', 0)`,
		`INSERT INTO run_child (id, run_id) VALUES (1, 2)`,
	}
	for _, q := range v24DDL {
		if _, err := rawDB.Exec(q); err != nil {
			t.Fatalf("v24 DDL %q: %v", q, err)
		}
	}
	rawDB.Close()

	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open (migrates v24→v25): %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	if v, _ := s.CurrentSchemaVersion(ctx); v != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", v, SchemaVersion)
	}

	// Every runs row carried over with its id and status intact.
	wantStatus := map[int64]string{
		1: RunStatusSuccess, 2: RunStatusFailed, 3: RunStatusPartial, 4: RunStatusRunning,
	}
	runs, err := s.ListRuns(ctx, ListRunsOpts{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != len(wantStatus) {
		t.Fatalf("runs after migration = %d, want %d", len(runs), len(wantStatus))
	}
	for _, r := range runs {
		if want := wantStatus[r.ID]; r.Status != want {
			t.Fatalf("run %d status = %q, want %q", r.ID, r.Status, want)
		}
	}

	// The child FK still resolves to the rebuilt runs table, and no
	// dangling reference slipped through the FK-off rebuild.
	var refs int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_child c JOIN runs r ON r.id = c.run_id`).Scan(&refs); err != nil {
		t.Fatalf("join child→runs: %v", err)
	}
	if refs != 1 {
		t.Fatalf("child rows resolving to runs = %d, want 1", refs)
	}
	fkRows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	violations := 0
	for fkRows.Next() {
		violations++
	}
	fkRows.Close()
	if violations != 0 {
		t.Fatalf("foreign_key_check reported %d violations after migration", violations)
	}

	// The widened CHECK admits the two new terminal states...
	for _, status := range []string{RunStatusRefused, RunStatusAborted} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO runs (kind, volume_id, destination, started_at_ns, status, file_count)
			 VALUES ('sync', 1, 'bucket', 300, ?, 0)`, status); err != nil {
			t.Fatalf("insert %q run rejected by CHECK after migration: %v", status, err)
		}
	}
	// ...but the CHECK still rejects an unknown status.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (kind, volume_id, destination, started_at_ns, status, file_count)
		 VALUES ('sync', 1, 'bucket', 320, 'bogus', 0)`); err == nil {
		t.Fatalf("CHECK accepted a bogus status, want rejection")
	}
}

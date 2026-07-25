package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrateToV28LeavesHistoryUnknown builds a pre-v28 database holding
// the two run shapes #182 is about — a bucket push whose file_count counts
// the whole volume, and a peer sync whose file_count is already the
// transferred count — and migrates it forward.
//
// The point of the nullable column is that history stays truthful: neither
// row gains a fabricated zero, both read back as "unknown", and Run.NoOp
// therefore keeps rendering them exactly as it did before the migration.
func TestMigrateToV28LeavesHistoryUnknown(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	ddl := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, endpoint TEXT, public_key_fingerprint TEXT)`,
		minimalRunsDDL,
		// Present since v11, so a v24 database has it; the fixture needs
		// it because FinishRun writes the run's transition here.
		`CREATE TABLE runs_audit (
			id         INTEGER PRIMARY KEY,
			run_id     INTEGER NOT NULL REFERENCES runs(id),
			transition TEXT    NOT NULL,
			operator   TEXT,
			at_ns      INTEGER NOT NULL,
			note       TEXT
		)`,
		`INSERT INTO schema_version (version) VALUES (24)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'v', '/v')`,
		`INSERT INTO nodes (id, name) VALUES (1, 'self')`,
		// An in-sync bucket push: nothing moved, but file_count counted
		// every already-correct file.
		`INSERT INTO runs (id, kind, volume_id, destination, started_at_ns, ended_at_ns, status, file_count)
			VALUES (1, 'sync', 1, 'bucket', 100, 200, 'success', 42)`,
		// A peer-sync no-op: file_count is the receiver-verified count, so
		// it was already zero.
		`INSERT INTO runs (id, kind, volume_id, destination, started_at_ns, ended_at_ns, status, file_count)
			VALUES (2, 'sync', 1, 'htpc', 110, 210, 'success', 0)`,
	}
	for _, q := range ddl {
		if _, err := rawDB.Exec(q); err != nil {
			t.Fatalf("pre-v28 DDL %q: %v", q, err)
		}
	}
	rawDB.Close()

	s, err := OpenWithOptions(dsn, OpenOptions{DisablePreMigrationBackup: true})
	if err != nil {
		t.Fatalf("Open (migrates to v%d): %v", SchemaVersion, err)
	}
	defer s.Close()
	ctx := context.Background()
	if v, _ := s.CurrentSchemaVersion(ctx); v != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", v, SchemaVersion)
	}

	runs, err := s.ListRuns(ctx, ListRunsOpts{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs after migration = %d, want 2", len(runs))
	}
	for _, r := range runs {
		if r.ChangedCount.Valid {
			t.Errorf("run %d changed_count = %+v, want NULL — history must not gain a fabricated count",
				r.ID, r.ChangedCount)
		}
	}
	// The fallback keeps each row's pre-migration rendering: the bucket
	// push stays visible (conservative), the peer-sync no-op still folds.
	if runs[0].NoOp() {
		t.Error("pre-v28 bucket push should stay visible, not fold on a guess")
	}
	if !runs[1].NoOp() {
		t.Error("pre-v28 peer-sync no-op should still fold")
	}

	// New runs on the migrated database record the real count.
	runID, err := s.BeginRun(ctx, RunKindSync, 1, "bucket", false)
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	if err := s.FinishRunChanged(ctx, runID, RunStatusSuccess, "", 42, 0); err != nil {
		t.Fatalf("FinishRunChanged: %v", err)
	}
	fresh, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !fresh.NoOp() {
		t.Errorf("in-sync bucket push should fold after v28: %+v", fresh.ChangedCount)
	}
}

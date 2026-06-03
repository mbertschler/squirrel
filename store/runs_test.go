package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// TestFinishRunRefusesTerminalRow is the H2 guard: once a run reaches a
// terminal status its status/error/ended_at are the audit record and a
// second FinishRun must not rewrite them. The second call returns
// ErrAlreadyFinished (matchable via errors.Is) and leaves the row
// exactly as the first finalisation left it.
func TestFinishRunRefusesTerminalRow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	runID := makeRun(t, s, vID)

	if err := s.FinishRun(ctx, runID, RunStatusSuccess, "", 7); err != nil {
		t.Fatalf("first FinishRun: %v", err)
	}
	before, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	// Second finalisation with a different status/error/count must be refused.
	err = s.FinishRun(ctx, runID, RunStatusFailed, "second writer", 99)
	if !errors.Is(err, ErrAlreadyFinished) {
		t.Fatalf("second FinishRun err = %v, want ErrAlreadyFinished", err)
	}

	after, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun after: %v", err)
	}
	if after.Status != RunStatusSuccess {
		t.Fatalf("status mutated to %q, want %q", after.Status, RunStatusSuccess)
	}
	if after.FileCount != before.FileCount {
		t.Fatalf("file_count mutated to %d, want %d", after.FileCount, before.FileCount)
	}
	if after.Error != before.Error {
		t.Fatalf("error mutated to %+v, want %+v", after.Error, before.Error)
	}
	if after.EndedAtNs != before.EndedAtNs {
		t.Fatalf("ended_at_ns mutated to %+v, want %+v", after.EndedAtNs, before.EndedAtNs)
	}

	// Exactly one 'finish' audit row — the refused call wrote nothing.
	audit, err := s.ListRunAudit(ctx, runID)
	if err != nil {
		t.Fatalf("ListRunAudit: %v", err)
	}
	if len(audit) != 1 {
		t.Fatalf("audit rows = %d, want 1 (refused call must not append)", len(audit))
	}
}

// TestFinishRunWritesAuditRow asserts a normal finalisation records one
// 'finish' runs_audit row whose note carries the resulting status and
// whose at_ns matches the run's ended_at_ns (written in the same tx).
func TestFinishRunWritesAuditRow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	runID := makeRun(t, s, vID)

	if err := s.FinishRun(ctx, runID, RunStatusPartial, "some files errored", 3); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	audit, err := s.ListRunAudit(ctx, runID)
	if err != nil {
		t.Fatalf("ListRunAudit: %v", err)
	}
	if len(audit) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(audit))
	}
	a := audit[0]
	if a.RunID != runID {
		t.Fatalf("audit run_id = %d, want %d", a.RunID, runID)
	}
	if a.Transition != TransitionFinish {
		t.Fatalf("transition = %q, want %q", a.Transition, TransitionFinish)
	}
	if a.Operator.Valid {
		t.Fatalf("operator = %+v, want NULL for machine-driven finish", a.Operator)
	}
	if !a.Note.Valid || a.Note.String != RunStatusPartial {
		t.Fatalf("note = %+v, want resulting status %q", a.Note, RunStatusPartial)
	}
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !run.EndedAtNs.Valid || run.EndedAtNs.Int64 != a.AtNs {
		t.Fatalf("audit at_ns = %d, want run ended_at_ns %+v", a.AtNs, run.EndedAtNs)
	}
}

// TestAppendRunAuditRoundTrip exercises the standalone (non-FinishRun)
// audit writer the `runs fail` CLI and #77's correlation write use, and
// confirms ListRunAudit returns rows oldest-first with operator/note
// preserved.
func TestAppendRunAuditRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	runID := makeRun(t, s, vID)

	if err := s.AppendRunAudit(ctx, RunAuditEntry{
		RunID: runID, Transition: TransitionManualFail, Operator: "alice", Note: "recovered stuck run",
	}); err != nil {
		t.Fatalf("AppendRunAudit: %v", err)
	}
	if err := s.AppendRunAudit(ctx, RunAuditEntry{
		RunID: runID, Transition: "set-correlated-run-id", Note: "peer=2",
	}); err != nil {
		t.Fatalf("AppendRunAudit second: %v", err)
	}

	audit, err := s.ListRunAudit(ctx, runID)
	if err != nil {
		t.Fatalf("ListRunAudit: %v", err)
	}
	if len(audit) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(audit))
	}
	if audit[0].Transition != TransitionManualFail || !audit[0].Operator.Valid || audit[0].Operator.String != "alice" {
		t.Fatalf("first row = %+v, want manual-fail by alice", audit[0])
	}
	if audit[1].Transition != "set-correlated-run-id" || audit[1].Operator.Valid {
		t.Fatalf("second row = %+v, want free-text transition with NULL operator", audit[1])
	}
}

// TestListRunAuditEmpty: a run that never recorded a transition returns
// an empty slice, not an error.
func TestListRunAuditEmpty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	runID := makeRun(t, s, vID)

	audit, err := s.ListRunAudit(ctx, runID)
	if err != nil {
		t.Fatalf("ListRunAudit: %v", err)
	}
	if len(audit) != 0 {
		t.Fatalf("audit rows = %d, want 0", len(audit))
	}
}

// TestRunsAuditForeignKey: an audit row must reference an existing run.
// Guards the FK so a typo in a future call site surfaces as an error
// rather than a dangling audit entry.
func TestRunsAuditForeignKey(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.AppendRunAudit(ctx, RunAuditEntry{RunID: 99999, Transition: TransitionFinish}); err == nil {
		t.Fatalf("AppendRunAudit against unknown run id succeeded, want FK violation")
	}
}

// TestMigrateV10ToV11CreatesRunsAudit seeds a v10-shape database by hand
// (schema_version + volumes + runs, the FK target the migration needs),
// opens it to trigger the v10→v11 step, and asserts the runs_audit table
// exists, is openable, and accepts an insert against the seeded run.
func TestMigrateV10ToV11CreatesRunsAudit(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	v10DDL := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE runs (
			id            INTEGER PRIMARY KEY,
			kind          TEXT NOT NULL CHECK (kind IN ('index','sync','restore','audit')),
			volume_id     INTEGER REFERENCES volumes(id),
			destination   TEXT,
			started_at_ns INTEGER NOT NULL,
			ended_at_ns   INTEGER,
			status        TEXT NOT NULL CHECK (status IN ('running','success','failed','partial')),
			error         TEXT,
			file_count    INTEGER NOT NULL DEFAULT 0,
			peer_node_id      INTEGER,
			correlated_run_id INTEGER,
			shallow INTEGER CHECK (shallow IS NULL OR shallow IN (0, 1))
		)`,
		`INSERT INTO schema_version (version) VALUES (10)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'v', '/v')`,
		`INSERT INTO runs (id, kind, volume_id, started_at_ns, status, file_count) VALUES (1, 'index', 1, 100, 'success', 5)`,
	}
	for _, q := range v10DDL {
		if _, err := rawDB.Exec(q); err != nil {
			t.Fatalf("v10 DDL %q: %v", q, err)
		}
	}
	rawDB.Close()

	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open (migrates v10→v11): %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if v, _ := s.CurrentSchemaVersion(ctx); v != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", v, SchemaVersion)
	}

	// The migrated DB carries the runs_audit table and its index, and an
	// insert against the seeded run id succeeds.
	if err := s.AppendRunAudit(ctx, RunAuditEntry{
		RunID: 1, Transition: TransitionManualFail, Operator: "op", Note: "post-migration insert",
	}); err != nil {
		t.Fatalf("AppendRunAudit after migration: %v", err)
	}
	audit, err := s.ListRunAudit(ctx, 1)
	if err != nil {
		t.Fatalf("ListRunAudit after migration: %v", err)
	}
	if len(audit) != 1 || audit[0].Transition != TransitionManualFail {
		t.Fatalf("post-migration audit = %+v, want one manual-fail row", audit)
	}
}

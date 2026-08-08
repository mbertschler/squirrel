package store

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
)

// digestFixture builds a distinct 32-byte digest for a test.
func digestFixture(b byte) []byte {
	d := make([]byte, 32)
	for i := range d {
		d[i] = b
	}
	return d
}

// TestConfigDriftLifecycle exercises raise → idempotent re-raise → clear
// and the runs/runs_audit trail each transition leaves behind.
func TestConfigDriftLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	loaded, disk := digestFixture(1), digestFixture(2)

	raised, err := s.RaiseConfigDrift(ctx, ConfigDriftState{
		Path: "/etc/squirrel.toml", Loaded: loaded, Disk: disk,
	})
	if err != nil {
		t.Fatalf("RaiseConfigDrift: %v", err)
	}
	if !raised {
		t.Fatal("first raise reported not-raised, want a fresh latch")
	}
	got, err := s.GetConfigDrift(ctx)
	if err != nil {
		t.Fatalf("GetConfigDrift: %v", err)
	}
	if got.Path != "/etc/squirrel.toml" {
		t.Fatalf("latch = %+v, want the path recorded", got)
	}
	gotLoaded, gotDisk := configDriftDigests(t, s)
	if !bytes.Equal(gotLoaded, loaded) || !bytes.Equal(gotDisk, disk) {
		t.Fatalf("latch digests = %x / %x, want %x / %x", gotLoaded, gotDisk, loaded, disk)
	}
	if got.RaisedRunID == 0 || got.RaisedAtNs == 0 {
		t.Fatalf("latch = %+v, want a raising run and timestamp", got)
	}
	firstRun, firstAt := got.RaisedRunID, got.RaisedAtNs

	// The raising run is a finished audit run carrying the message, and its
	// audit trail names the drift — automatic work is never invisible.
	run, err := s.GetRun(ctx, firstRun)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Kind != RunKindAudit || run.Status != RunStatusPartial || !run.EndedAtNs.Valid {
		t.Fatalf("raising run = %+v, want a finished audit run", run)
	}
	if run.Error.String != ConfigDriftMessage {
		t.Fatalf("raising run error = %q, want %q", run.Error.String, ConfigDriftMessage)
	}
	if !hasTransition(t, s, firstRun, TransitionConfigDriftRaise) {
		t.Fatalf("no %s runs_audit entry against run %d", TransitionConfigDriftRaise, firstRun)
	}

	// A second detection within the same episode refreshes what the latch
	// says without disturbing when it started, so "noticed N ago" never
	// resets — and writes no second run row.
	runsBefore := countRuns(t, s)
	reraised, err := s.RaiseConfigDrift(ctx, ConfigDriftState{
		Path: "/etc/squirrel.toml", Loaded: loaded, Disk: digestFixture(3),
		PendingKeys: []string{"agent.auth.token"},
	})
	if err != nil {
		t.Fatalf("re-raise: %v", err)
	}
	if reraised {
		t.Fatal("second raise reported a fresh latch, want not-raised (idempotent)")
	}
	got, err = s.GetConfigDrift(ctx)
	if err != nil {
		t.Fatalf("GetConfigDrift after re-raise: %v", err)
	}
	if got.RaisedRunID != firstRun || got.RaisedAtNs != firstAt {
		t.Fatalf("re-raise mutated the latch: %+v", got)
	}
	if _, gotDisk := configDriftDigests(t, s); !bytes.Equal(gotDisk, digestFixture(3)) {
		t.Fatalf("re-raise left the disk digest at %x, want the newer finding %x", gotDisk, digestFixture(3))
	}
	if want := []string{"agent.auth.token"}; !slices.Equal(got.PendingKeys, want) {
		t.Fatalf("pending keys = %v, want %v refreshed onto the standing latch", got.PendingKeys, want)
	}
	if after := countRuns(t, s); after != runsBefore {
		t.Fatalf("runs = %d after a losing raise, want %d (no orphan run row)", after, runsBefore)
	}

	cleared, err := s.ClearConfigDrift(ctx, ConfigDriftClearedByRestart)
	if err != nil {
		t.Fatalf("ClearConfigDrift: %v", err)
	}
	if !cleared {
		t.Fatal("clear reported nothing standing, want the latch cleared")
	}
	if _, err := s.GetConfigDrift(ctx); !IsNotFound(err) {
		t.Fatalf("GetConfigDrift after clear: %v, want not-found", err)
	}
	// The clear is recorded against the run that raised it, so the episode
	// stays recoverable after the live row is gone.
	if !hasTransition(t, s, firstRun, TransitionConfigDriftClear) {
		t.Fatalf("no %s runs_audit entry against run %d", TransitionConfigDriftClear, firstRun)
	}
	if run, err := s.GetRun(ctx, firstRun); err != nil || run.Status != RunStatusPartial {
		t.Fatalf("raising run after clear = %+v (err %v), want it untouched", run, err)
	}
}

// TestClearConfigDriftWithoutLatch is the ordinary agent-startup call on a
// machine that was never in drift: it must report nothing standing and
// leave no audit entry behind.
func TestClearConfigDriftWithoutLatch(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	cleared, err := s.ClearConfigDrift(ctx, ConfigDriftClearedByRestart)
	if err != nil {
		t.Fatalf("ClearConfigDrift: %v", err)
	}
	if cleared {
		t.Fatal("clear reported a standing latch on a fresh store")
	}
	if n := countRuns(t, s); n != 0 {
		t.Fatalf("runs = %d after a no-op clear, want 0", n)
	}
}

// TestRaiseConfigDriftAfterClear re-raises once the latch has been cleared,
// which is the "operator restarts, then edits again" path: a new episode
// gets its own run and its own timestamp.
func TestRaiseConfigDriftAfterClear(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.RaiseConfigDrift(ctx, ConfigDriftState{
		Path: "/c.toml", Loaded: digestFixture(1), Disk: digestFixture(2),
	}); err != nil {
		t.Fatalf("first raise: %v", err)
	}
	first, err := s.GetConfigDrift(ctx)
	if err != nil {
		t.Fatalf("GetConfigDrift: %v", err)
	}
	if _, err := s.ClearConfigDrift(ctx, ConfigDriftClearedByRevert); err != nil {
		t.Fatalf("clear: %v", err)
	}
	raised, err := s.RaiseConfigDrift(ctx, ConfigDriftState{
		Path: "/c.toml", Loaded: digestFixture(1), Disk: digestFixture(4),
	})
	if err != nil {
		t.Fatalf("second raise: %v", err)
	}
	if !raised {
		t.Fatal("raise after clear reported not-raised, want a fresh latch")
	}
	second, err := s.GetConfigDrift(ctx)
	if err != nil {
		t.Fatalf("GetConfigDrift after re-raise: %v", err)
	}
	if second.RaisedRunID == first.RaisedRunID {
		t.Fatalf("second episode reused run %d, want its own", first.RaisedRunID)
	}
	if _, gotDisk := configDriftDigests(t, s); !bytes.Equal(gotDisk, digestFixture(4)) {
		t.Fatalf("second episode disk digest = %x, want the newer one", gotDisk)
	}
}

// TestRaiseConfigDriftRejectsShortDigest proves the schema's fixed-length
// CHECK guards the latch: a truncated digest is a bug, not a shorter hash.
func TestRaiseConfigDriftRejectsShortDigest(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.RaiseConfigDrift(context.Background(), ConfigDriftState{
		Path: "/c.toml", Loaded: digestFixture(1)[:16], Disk: digestFixture(2),
	}); err == nil {
		t.Fatal("RaiseConfigDrift accepted a 16-byte digest, want a CHECK failure")
	}
	if n := countRuns(t, s); n != 0 {
		t.Fatalf("runs = %d after a rejected raise, want 0 (transaction rolled back)", n)
	}
}

// configDriftDigests reads the latch's evidence columns straight from the
// table. GetConfigDrift deliberately does not project them — nothing in the
// tree reads them, and an unused field on a public type is what AGENTS.md
// rules out — but they remain the episode's forensic record, so what gets
// written, and what a losing raise must not overwrite, is asserted here
// against the columns themselves.
func configDriftDigests(t *testing.T, s *Store) (loaded, disk []byte) {
	t.Helper()
	err := s.db.QueryRowContext(context.Background(),
		`SELECT loaded_blake3, disk_blake3 FROM config_drift WHERE id = 1`).Scan(&loaded, &disk)
	if err != nil {
		t.Fatalf("read config_drift digests: %v", err)
	}
	return loaded, disk
}

func hasTransition(t *testing.T, s *Store, runID int64, transition string) bool {
	t.Helper()
	entries, err := s.ListRunAudit(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListRunAudit: %v", err)
	}
	for _, e := range entries {
		if e.Transition == transition {
			return true
		}
	}
	return false
}

func countRuns(t *testing.T, s *Store) int {
	t.Helper()
	runs, err := s.ListRuns(context.Background(), ListRunsOpts{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	return len(runs)
}

// TestConfigDriftMessageFor covers the three sentences a latch can carry —
// nothing applied, a partial reload with a restart still owed, and an edit
// refused outright — since every surface renders through this one function.
func TestConfigDriftMessageFor(t *testing.T) {
	if got := ConfigDriftMessageFor(nil, ""); got != ConfigDriftMessage {
		t.Fatalf("plain message = %q, want %q", got, ConfigDriftMessage)
	}
	pending := ConfigDriftMessageFor([]string{"agent.auth.token", "agent.tls"}, "")
	if !strings.Contains(pending, "agent.auth.token") || !strings.Contains(pending, "agent.tls") {
		t.Fatalf("pending message = %q, want both keys named", pending)
	}
	if strings.Contains(pending, "could not be applied") {
		t.Fatalf("pending message = %q, want it to report a partial apply, not a refusal", pending)
	}
	refused := ConfigDriftMessageFor([]string{"agent.tls"}, "parse error at line 3")
	if !strings.Contains(refused, "parse error at line 3") {
		t.Fatalf("refusal message = %q, want the reason", refused)
	}
	if strings.Contains(refused, "agent.tls") {
		t.Fatalf("refusal message = %q, must not promise a partial apply that never happened", refused)
	}
}

// TestRecordConfigReload: the agent changing its own operating config lands
// in the trail as a terminal audit run naming what moved, 'success' when the
// whole edit was adopted and 'partial' when a restart is still owed.
func TestRecordConfigReload(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	full, err := s.RecordConfigReload(ctx, "/c.toml", []string{"volumes", "destinations"}, nil)
	if err != nil {
		t.Fatalf("RecordConfigReload: %v", err)
	}
	run, err := s.GetRun(ctx, full)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Kind != RunKindAudit || run.Status != RunStatusSuccess || !run.EndedAtNs.Valid {
		t.Fatalf("full reload run = %+v, want a finished successful audit run", run)
	}
	if run.Error.String != "" {
		t.Fatalf("full reload run error = %q, want none", run.Error.String)
	}
	if !hasTransition(t, s, full, TransitionConfigReload) {
		t.Fatalf("no %s entry against run %d", TransitionConfigReload, full)
	}

	partial, err := s.RecordConfigReload(ctx, "/c.toml", nil, []string{"agent.auth.token"})
	if err != nil {
		t.Fatalf("RecordConfigReload (partial): %v", err)
	}
	run, err = s.GetRun(ctx, partial)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != RunStatusPartial {
		t.Fatalf("partial reload run status = %q, want partial", run.Status)
	}
	if !strings.Contains(run.Error.String, "agent.auth.token") {
		t.Fatalf("partial reload run error = %q, want it to name the pending key", run.Error.String)
	}
	entries, err := s.ListRunAudit(ctx, partial)
	if err != nil {
		t.Fatalf("ListRunAudit: %v", err)
	}
	var note string
	for _, e := range entries {
		if e.Transition == TransitionConfigReload {
			note = e.Note.String
		}
	}
	if !strings.Contains(note, "applied=none") || !strings.Contains(note, "pending=agent.auth.token") {
		t.Fatalf("reload note = %q, want it to name both halves", note)
	}
}

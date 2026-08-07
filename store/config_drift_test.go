package store

import (
	"bytes"
	"context"
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

	raised, err := s.RaiseConfigDrift(ctx, "/etc/squirrel.toml", loaded, disk)
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
	if got.Path != "/etc/squirrel.toml" || !bytes.Equal(got.LoadedBlake3, loaded) || !bytes.Equal(got.DiskBlake3, disk) {
		t.Fatalf("latch = %+v, want the path and both digests recorded", got)
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

	// A second detection of the same episode leaves the latch alone, so
	// "noticed N ago" never resets — and writes no second run row.
	runsBefore := countRuns(t, s)
	reraised, err := s.RaiseConfigDrift(ctx, "/etc/squirrel.toml", loaded, digestFixture(3))
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
	if got.RaisedRunID != firstRun || got.RaisedAtNs != firstAt || !bytes.Equal(got.DiskBlake3, disk) {
		t.Fatalf("re-raise mutated the latch: %+v", got)
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

	if _, err := s.RaiseConfigDrift(ctx, "/c.toml", digestFixture(1), digestFixture(2)); err != nil {
		t.Fatalf("first raise: %v", err)
	}
	first, err := s.GetConfigDrift(ctx)
	if err != nil {
		t.Fatalf("GetConfigDrift: %v", err)
	}
	if _, err := s.ClearConfigDrift(ctx, ConfigDriftClearedByRevert); err != nil {
		t.Fatalf("clear: %v", err)
	}
	raised, err := s.RaiseConfigDrift(ctx, "/c.toml", digestFixture(1), digestFixture(4))
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
	if !bytes.Equal(second.DiskBlake3, digestFixture(4)) {
		t.Fatalf("second episode disk digest = %x, want the newer one", second.DiskBlake3)
	}
}

// TestRaiseConfigDriftRejectsShortDigest proves the schema's fixed-length
// CHECK guards the latch: a truncated digest is a bug, not a shorter hash.
func TestRaiseConfigDriftRejectsShortDigest(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.RaiseConfigDrift(context.Background(), "/c.toml", digestFixture(1)[:16], digestFixture(2)); err == nil {
		t.Fatal("RaiseConfigDrift accepted a 16-byte digest, want a CHECK failure")
	}
	if n := countRuns(t, s); n != 0 {
		t.Fatalf("runs = %d after a rejected raise, want 0 (transaction rolled back)", n)
	}
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

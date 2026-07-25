package sync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

// finishTestStore opens an empty index with one volume, returning the store
// and the volume id sync/restore runs can be opened against. It needs no
// rclone binary — these tests drive the terminal-state writer directly with
// a synthesised RunResult.
func finishTestStore(t *testing.T) (*store.Store, int64) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	vol, err := s.GetOrCreateVolume(context.Background(), "/pics")
	if err != nil {
		t.Fatalf("GetOrCreateVolume: %v", err)
	}
	return s, vol.ID
}

// TestFinishRunRecordsTransferredAsChanged covers the bucket-push half of
// #182: rclone's file_count is transferred + already-correct, so only the
// transferred count can tell an in-sync push from one that moved content.
// The two runs below differ in nothing else — same file_count, same status
// — yet only the in-sync one folds out of `runs --changes` and the TUI.
func TestFinishRunRecordsTransferredAsChanged(t *testing.T) {
	s, volID := finishTestStore(t)
	ctx := context.Background()

	inSync := &Report{RcloneResult: RunResult{Checked: 42}}
	moved := &Report{RcloneResult: RunResult{Transferred: 3, Checked: 39}}
	for _, tc := range []struct {
		name        string
		rep         *Report
		wantChanged int64
		wantNoOp    bool
	}{
		{"in-sync push", inSync, 0, true},
		{"push that transferred", moved, 3, false},
	} {
		runID, err := s.BeginRun(ctx, store.RunKindSync, volID, "scratch", false)
		if err != nil {
			t.Fatalf("%s: BeginRun: %v", tc.name, err)
		}
		finishRun(ctx, s, false, runID, tc.rep)
		if tc.rep.FinishErr != nil {
			t.Fatalf("%s: FinishErr: %v", tc.name, tc.rep.FinishErr)
		}
		run, err := s.GetRun(ctx, runID)
		if err != nil {
			t.Fatalf("%s: GetRun: %v", tc.name, err)
		}
		if run.FileCount != 42 {
			t.Errorf("%s: file_count = %d, want 42 (transferred + already-correct)", tc.name, run.FileCount)
		}
		if !run.ChangedCount.Valid || run.ChangedCount.Int64 != tc.wantChanged {
			t.Errorf("%s: changed_count = %+v, want %d", tc.name, run.ChangedCount, tc.wantChanged)
		}
		if got := run.NoOp(); got != tc.wantNoOp {
			t.Errorf("%s: NoOp() = %v, want %v", tc.name, got, tc.wantNoOp)
		}
	}
}

// TestFinishHandlerRunLeavesKopiaChangedUnknown pins the honest gap: a
// kopia snapshot reports its whole tree and no changed count, so its run
// records changed_count NULL rather than a fabricated zero — and keeps the
// conservative file_count rendering instead of folding away.
func TestFinishHandlerRunLeavesKopiaChangedUnknown(t *testing.T) {
	s, volID := finishTestStore(t)
	ctx := context.Background()
	runID, err := s.BeginRun(ctx, store.RunKindSync, volID, "vault", false)
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}

	rep := &Report{
		RunID:        runID,
		Status:       store.RunStatusSuccess,
		Verification: VerifyResult{Method: VerifyMethodKopia, Files: 42},
	}
	finishHandlerRun(ctx, s, rep, nil)
	if rep.FinishErr != nil {
		t.Fatalf("FinishErr: %v", rep.FinishErr)
	}
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.ChangedCount.Valid {
		t.Errorf("changed_count = %+v, want NULL for a kopia snapshot", run.ChangedCount)
	}
	if run.NoOp() {
		t.Error("a run with no changed count must stay visible, not fold on a guess")
	}
}

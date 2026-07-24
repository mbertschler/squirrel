package store

import (
	"context"
	"testing"
)

// TestAbortRunningRuns reaps every in-flight run to 'aborted', leaves
// terminal rows untouched, is idempotent, and records an 'abort'
// transition per reaped row.
func TestAbortRunningRuns(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vol, err := s.CreateVolume(ctx, "v", "/v")
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	idx, err := s.BeginIndexRun(ctx, RunKindIndex, vol.ID, true)
	if err != nil {
		t.Fatalf("BeginIndexRun: %v", err)
	}
	syncing, err := s.BeginRun(ctx, RunKindSync, vol.ID, "offsite", false)
	if err != nil {
		t.Fatalf("BeginRun sync: %v", err)
	}
	done, err := s.BeginRun(ctx, RunKindSync, vol.ID, "other", false)
	if err != nil {
		t.Fatalf("BeginRun done: %v", err)
	}
	if err := s.FinishRun(ctx, done, RunStatusSuccess, "", 5); err != nil {
		t.Fatalf("FinishRun done: %v", err)
	}

	ids, err := s.AbortRunningRuns(ctx, "killed")
	if err != nil {
		t.Fatalf("AbortRunningRuns: %v", err)
	}
	if len(ids) != 2 || ids[0] != idx || ids[1] != syncing {
		t.Fatalf("reaped ids = %v, want [%d %d]", ids, idx, syncing)
	}

	for _, id := range []int64{idx, syncing} {
		run, err := s.GetRun(ctx, id)
		if err != nil {
			t.Fatalf("GetRun(%d): %v", id, err)
		}
		if run.Status != RunStatusAborted {
			t.Fatalf("run %d status = %q, want aborted", id, run.Status)
		}
		if !run.EndedAtNs.Valid {
			t.Fatalf("aborted run %d has no ended_at_ns", id)
		}
		if run.Error.String != "killed" {
			t.Fatalf("aborted run %d error = %q, want the reap reason", id, run.Error.String)
		}
		if n := countTransition(t, s, id, TransitionAbort); n != 1 {
			t.Fatalf("run %d abort-transition count = %d, want 1", id, n)
		}
	}

	// The already-terminal success is untouched.
	if run, _ := s.GetRun(ctx, done); run.Status != RunStatusSuccess {
		t.Fatalf("finished run status = %q, want success", run.Status)
	}

	// Idempotent: a second reap finds nothing.
	again, err := s.AbortRunningRuns(ctx, "killed")
	if err != nil {
		t.Fatalf("second AbortRunningRuns: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second reap returned %v, want none", again)
	}
}

// TestLatestFinishedRunStatusScope pins the cadence-watermark policy: a
// 'refused' run consumes the cadence window (so the scheduler backs off a
// dead destination) while an 'aborted' run does not (so a crashed run is
// re-attempted).
func TestLatestFinishedRunStatusScope(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vol, err := s.CreateVolume(ctx, "v", "/v")
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	refused, err := s.BeginRun(ctx, RunKindSync, vol.ID, "dead", false)
	if err != nil {
		t.Fatalf("BeginRun refused: %v", err)
	}
	if err := s.FinishRun(ctx, refused, RunStatusRefused, "no marker", 0); err != nil {
		t.Fatalf("FinishRun refused: %v", err)
	}
	got, err := s.LatestFinishedRun(ctx, RunKindSync, vol.ID, "dead")
	if err != nil {
		t.Fatalf("refused run should be a finished run: %v", err)
	}
	if got.ID != refused || got.Status != RunStatusRefused {
		t.Fatalf("LatestFinishedRun = %+v, want the refused run", got)
	}

	if _, err := s.BeginRun(ctx, RunKindSync, vol.ID, "crashed", false); err != nil {
		t.Fatalf("BeginRun crashed: %v", err)
	}
	if _, err := s.AbortRunningRuns(ctx, "killed"); err != nil {
		t.Fatalf("AbortRunningRuns: %v", err)
	}
	if _, err := s.LatestFinishedRun(ctx, RunKindSync, vol.ID, "crashed"); !IsNotFound(err) {
		t.Fatalf("aborted run must not count as a finished run, got %v", err)
	}
}

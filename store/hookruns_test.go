package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestHookRunLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vol, err := s.CreateVolume(ctx, "photos", "/tmp/photos")
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	// A change hook references the index run that fired it.
	runID, err := s.BeginIndexRun(ctx, RunKindIndex, vol.ID, true)
	if err != nil {
		t.Fatalf("BeginIndexRun: %v", err)
	}

	id, err := s.BeginHookRun(ctx, HookRunSpec{
		VolumeID:        vol.ID,
		Trigger:         HookTriggerChange,
		TriggeringRunID: runID,
		Changed:         true,
	})
	if err != nil {
		t.Fatalf("BeginHookRun: %v", err)
	}

	got, err := s.hookRunByID(ctx, id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Status != HookStatusRunning {
		t.Fatalf("status = %q, want running", got.Status)
	}
	if !got.TriggeringRunID.Valid || got.TriggeringRunID.Int64 != runID {
		t.Fatalf("TriggeringRunID = %v, want %d", got.TriggeringRunID, runID)
	}
	if !got.Changed {
		t.Fatalf("Changed = false, want true")
	}
	if got.EndedAtNs.Valid {
		t.Fatalf("EndedAtNs set before finish")
	}

	exit := sql.NullInt64{Int64: 0, Valid: true}
	if err := s.FinishHookRun(ctx, id, HookStatusSuccess, exit, ""); err != nil {
		t.Fatalf("FinishHookRun: %v", err)
	}
	got, err = s.hookRunByID(ctx, id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Status != HookStatusSuccess {
		t.Fatalf("status = %q, want success", got.Status)
	}
	if !got.ExitCode.Valid || got.ExitCode.Int64 != 0 {
		t.Fatalf("ExitCode = %v, want 0", got.ExitCode)
	}
	if !got.EndedAtNs.Valid {
		t.Fatalf("EndedAtNs not set after finish")
	}
}

func TestBeginHookRunIntervalNoTriggeringRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vol, err := s.CreateVolume(ctx, "v", "/tmp/v")
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	id, err := s.BeginHookRun(ctx, HookRunSpec{VolumeID: vol.ID, Trigger: HookTriggerInterval})
	if err != nil {
		t.Fatalf("BeginHookRun: %v", err)
	}
	got, err := s.hookRunByID(ctx, id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.TriggeringRunID.Valid {
		t.Fatalf("TriggeringRunID = %v, want NULL for interval hook", got.TriggeringRunID)
	}
}

func TestBeginHookRunRejectsBadTrigger(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vol, _ := s.CreateVolume(ctx, "v", "/tmp/v")
	if _, err := s.BeginHookRun(ctx, HookRunSpec{VolumeID: vol.ID, Trigger: "bogus"}); err == nil {
		t.Fatalf("expected error for bad trigger, got nil")
	}
}

func TestFinishHookRunRejectsBadStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vol, _ := s.CreateVolume(ctx, "v", "/tmp/v")
	// Interval trigger needs no triggering run, keeping this focused on the
	// FinishHookRun status validation.
	id, err := s.BeginHookRun(ctx, HookRunSpec{VolumeID: vol.ID, Trigger: HookTriggerInterval})
	if err != nil {
		t.Fatalf("BeginHookRun: %v", err)
	}
	if err := s.FinishHookRun(ctx, id, HookStatusRunning, sql.NullInt64{}, ""); err == nil {
		t.Fatalf("expected error finishing with non-terminal status, got nil")
	}
}

func TestBeginHookRunRejectsTriggerRunMismatch(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vol, _ := s.CreateVolume(ctx, "v", "/tmp/v")
	// change without a triggering run is rejected.
	if _, err := s.BeginHookRun(ctx, HookRunSpec{VolumeID: vol.ID, Trigger: HookTriggerChange}); err == nil {
		t.Fatalf("expected error: change hook with no triggering run")
	}
	// interval with a triggering run is rejected.
	if _, err := s.BeginHookRun(ctx, HookRunSpec{VolumeID: vol.ID, Trigger: HookTriggerInterval, TriggeringRunID: 1}); err == nil {
		t.Fatalf("expected error: interval hook carrying a triggering run")
	}
}

func TestFinishHookRunUnknownID(t *testing.T) {
	s := openTestStore(t)
	if err := s.FinishHookRun(context.Background(), 9999, HookStatusFailed, sql.NullInt64{}, "x"); err == nil {
		t.Fatalf("expected error for unknown hook run id, got nil")
	}
}

// TestFinishHookRunRefusesTerminalRow: the first terminal write wins. A
// second finish is refused with ErrAlreadyFinished and leaves the
// recorded status, exit code, and end timestamp untouched (#114) — the
// same first-write-wins guard FinishRun has.
func TestFinishHookRunRefusesTerminalRow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vol, _ := s.CreateVolume(ctx, "v", "/tmp/v")
	id, err := s.BeginHookRun(ctx, HookRunSpec{VolumeID: vol.ID, Trigger: HookTriggerInterval})
	if err != nil {
		t.Fatalf("BeginHookRun: %v", err)
	}

	firstExit := sql.NullInt64{Int64: 0, Valid: true}
	if err := s.FinishHookRun(ctx, id, HookStatusSuccess, firstExit, ""); err != nil {
		t.Fatalf("first FinishHookRun: %v", err)
	}
	before, err := s.hookRunByID(ctx, id)
	if err != nil {
		t.Fatalf("read back after first finish: %v", err)
	}

	err = s.FinishHookRun(ctx, id, HookStatusFailed, sql.NullInt64{Int64: 7, Valid: true}, "second finish")
	if !errors.Is(err, ErrAlreadyFinished) {
		t.Fatalf("second FinishHookRun err = %v, want ErrAlreadyFinished", err)
	}

	after, err := s.hookRunByID(ctx, id)
	if err != nil {
		t.Fatalf("read back after refused finish: %v", err)
	}
	if after.Status != HookStatusSuccess {
		t.Fatalf("status = %q after refused second finish, want success", after.Status)
	}
	if after.ExitCode != before.ExitCode || after.EndedAtNs != before.EndedAtNs || after.Error != before.Error {
		t.Fatalf("terminal row mutated by refused finish: before=%+v after=%+v", before, after)
	}
}

func TestListHookRuns(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	va, _ := s.CreateVolume(ctx, "a", "/tmp/a")
	vb, _ := s.CreateVolume(ctx, "b", "/tmp/b")
	// The change hook needs a real triggering run (the column is an FK and
	// the trigger↔run coupling is enforced).
	runID, err := s.BeginIndexRun(ctx, RunKindIndex, va.ID, true)
	if err != nil {
		t.Fatalf("BeginIndexRun: %v", err)
	}
	id1, _ := s.BeginHookRun(ctx, HookRunSpec{VolumeID: va.ID, Trigger: HookTriggerChange, TriggeringRunID: runID})
	id2, _ := s.BeginHookRun(ctx, HookRunSpec{VolumeID: vb.ID, Trigger: HookTriggerInterval})
	id3, _ := s.BeginHookRun(ctx, HookRunSpec{VolumeID: va.ID, Trigger: HookTriggerInterval})

	all, err := s.ListHookRuns(ctx, HookRunListOpts{Descending: true})
	if err != nil {
		t.Fatalf("ListHookRuns: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
	if all[0].ID != id3 || all[2].ID != id1 {
		t.Fatalf("descending order wrong: got %d..%d", all[0].ID, all[2].ID)
	}

	onlyA, err := s.ListHookRuns(ctx, HookRunListOpts{VolumeID: &va.ID})
	if err != nil {
		t.Fatalf("ListHookRuns volume filter: %v", err)
	}
	if len(onlyA) != 2 {
		t.Fatalf("volume-filtered len = %d, want 2", len(onlyA))
	}
	for _, h := range onlyA {
		if h.VolumeID != va.ID {
			t.Fatalf("volume filter leaked volume %d", h.VolumeID)
		}
	}
	_ = id2
}

func TestLatestHookRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vol, err := s.CreateVolume(ctx, "v", "/tmp/v")
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	if _, err := s.LatestHookRun(ctx, vol.ID, HookTriggerInterval); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("LatestHookRun on empty = %v, want sql.ErrNoRows", err)
	}

	// The change hook needs a real triggering run (the trigger↔run coupling
	// is enforced); interval hooks carry none.
	runID, err := s.BeginIndexRun(ctx, RunKindIndex, vol.ID, true)
	if err != nil {
		t.Fatalf("BeginIndexRun: %v", err)
	}
	mustBegin := func(spec HookRunSpec) int64 {
		t.Helper()
		id, err := s.BeginHookRun(ctx, spec)
		if err != nil {
			t.Fatalf("BeginHookRun(%s): %v", spec.Trigger, err)
		}
		return id
	}
	// Two interval runs and a change run; LatestHookRun must return the
	// newest of the requested trigger only.
	mustBegin(HookRunSpec{VolumeID: vol.ID, Trigger: HookTriggerInterval})
	mustBegin(HookRunSpec{VolumeID: vol.ID, Trigger: HookTriggerChange, TriggeringRunID: runID})
	latestInterval := mustBegin(HookRunSpec{VolumeID: vol.ID, Trigger: HookTriggerInterval})

	got, err := s.LatestHookRun(ctx, vol.ID, HookTriggerInterval)
	if err != nil {
		t.Fatalf("LatestHookRun: %v", err)
	}
	if got.ID != latestInterval {
		t.Fatalf("latest interval id = %d, want %d", got.ID, latestInterval)
	}
	if got.Trigger != HookTriggerInterval {
		t.Fatalf("trigger = %q, want interval", got.Trigger)
	}
}

// hookRunByID reads a single hook run by id. It lives in the test
// file (not the package surface) because production code never fetches a
// hook run by primary key — it lists or, in the follow-up interval work,
// reads the latest per (volume, trigger).
func (s *Store) hookRunByID(ctx context.Context, id int64) (HookRun, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+hookRunColumns+` FROM hook_runs WHERE id = ?`, id)
	r, err := scanHookRun(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return HookRun{}, err
	}
	return r, err
}

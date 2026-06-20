package store

import (
	"context"
	"testing"
)

// upsertPresent writes one 'present' row and returns it re-read, so
// tests get the resolved ContentID.
func upsertPresent(t *testing.T, s *Store, volumeID, runID int64, relPath string, d byte) FileRow {
	t.Helper()
	ctx := context.Background()
	r := FileRow{
		VolumeID: volumeID, Path: relPath, Blake3: digest(d), SizeBytes: 10,
		MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: 100,
	}
	if err := s.Upsert(ctx, r, nil); err != nil {
		t.Fatalf("Upsert %s: %v", relPath, err)
	}
	got, err := s.GetByPath(ctx, volumeID, relPath)
	if err != nil {
		t.Fatalf("GetByPath %s: %v", relPath, err)
	}
	return got
}

// TestMarkOffloaded: the present → offloaded flip stamps
// last_seen_run_id with the offload run, preserves first_seen_run_id
// and the content binding, and updates the folder's live aggregates in
// the same transaction (the offloaded file leaves the live set).
func TestMarkOffloaded(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	indexRun := makeRun(t, s, vID)
	row := upsertPresent(t, s, vID, indexRun, "sub/a.txt", 0x01)
	upsertPresent(t, s, vID, indexRun, "sub/b.txt", 0x02)

	offloadRun := makeRun(t, s, vID)
	if err := s.MarkOffloaded(ctx, vID, "sub/a.txt", row.ContentID, offloadRun); err != nil {
		t.Fatalf("MarkOffloaded: %v", err)
	}

	got, err := s.GetByPath(ctx, vID, "sub/a.txt")
	if err != nil {
		t.Fatalf("GetByPath after flip: %v", err)
	}
	if got.Status != StatusOffloaded {
		t.Fatalf("status = %q, want offloaded", got.Status)
	}
	if got.LastSeenRunID != offloadRun {
		t.Fatalf("last_seen_run_id = %d, want offload run %d", got.LastSeenRunID, offloadRun)
	}
	if got.FirstSeenRunID != row.FirstSeenRunID || got.ContentID != row.ContentID {
		t.Fatalf("first_seen/content changed: got %+v, want %+v", got, row)
	}

	folder, err := s.GetFolderByPath(ctx, vID, "sub")
	if err != nil {
		t.Fatalf("GetFolderByPath: %v", err)
	}
	if folder.FileCount != 1 {
		t.Fatalf("folder file_count = %d, want 1 (only b.txt stays live)", folder.FileCount)
	}
}

// TestMarkOffloadedRefusals: the flip only ever applies to the exact
// live 'present' (folder, name, content) row the caller verified. A
// row in any other status, a different content id, or an unknown path
// must error instead of mislabelling.
func TestMarkOffloadedRefusals(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run := makeRun(t, s, vID)
	row := upsertPresent(t, s, vID, run, "a.txt", 0x01)

	if err := s.MarkOffloaded(ctx, vID, "a.txt", row.ContentID+99, run); err == nil {
		t.Fatalf("MarkOffloaded with wrong content id succeeded, want error")
	}
	if err := s.MarkOffloaded(ctx, vID, "nope/missing.txt", row.ContentID, run); err == nil {
		t.Fatalf("MarkOffloaded for unknown path succeeded, want error")
	}

	if err := s.MarkOffloaded(ctx, vID, "a.txt", row.ContentID, run); err != nil {
		t.Fatalf("first MarkOffloaded: %v", err)
	}
	if err := s.MarkOffloaded(ctx, vID, "a.txt", row.ContentID, run); err == nil {
		t.Fatalf("second MarkOffloaded on an offloaded row succeeded, want error")
	}
}

// TestBeginOffloadRunIfClear: offload defers to every other run kind on
// the volume — any 'running' row blocks, finished rows clear the gate,
// and the inserted row is kind='offload' with destination NULL.
func TestBeginOffloadRunIfClear(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	indexRun, err := s.BeginIndexRun(ctx, RunKindIndex, vID, false)
	if err != nil {
		t.Fatalf("BeginIndexRun: %v", err)
	}
	id, blocker, err := s.BeginOffloadRunIfClear(ctx, vID)
	if err != nil {
		t.Fatalf("BeginOffloadRunIfClear: %v", err)
	}
	if id != 0 || blocker == nil || blocker.ID != indexRun {
		t.Fatalf("running index run did not block: id=%d blocker=%+v", id, blocker)
	}
	if err := s.FinishRun(ctx, indexRun, RunStatusSuccess, "", 0); err != nil {
		t.Fatalf("FinishRun index: %v", err)
	}

	id, blocker, err = s.BeginOffloadRunIfClear(ctx, vID)
	if err != nil {
		t.Fatalf("BeginOffloadRunIfClear after finish: %v", err)
	}
	if blocker != nil || id == 0 {
		t.Fatalf("clear volume refused: id=%d blocker=%+v", id, blocker)
	}
	run, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Kind != RunKindOffload || run.Destination.Valid || run.Status != RunStatusRunning {
		t.Fatalf("offload run = %+v, want kind=offload destination=NULL status=running", run)
	}

	id2, blocker, err := s.BeginOffloadRunIfClear(ctx, vID)
	if err != nil {
		t.Fatalf("BeginOffloadRunIfClear while offload running: %v", err)
	}
	if id2 != 0 || blocker == nil || blocker.Kind != RunKindOffload {
		t.Fatalf("running offload run did not block: id=%d blocker=%+v", id2, blocker)
	}
}

// TestBeginOffloadRunIfClearScopedToVolume: a running run on another
// volume never blocks this volume's offload.
func TestBeginOffloadRunIfClearScopedToVolume(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	aID := makeVolume(t, s, "/a")
	bID := makeVolume(t, s, "/b")
	if _, err := s.BeginIndexRun(ctx, RunKindIndex, aID, false); err != nil {
		t.Fatalf("BeginIndexRun: %v", err)
	}

	id, blocker, err := s.BeginOffloadRunIfClear(ctx, bID)
	if err != nil {
		t.Fatalf("BeginOffloadRunIfClear: %v", err)
	}
	if blocker != nil || id == 0 {
		t.Fatalf("other volume's run blocked offload: id=%d blocker=%+v", id, blocker)
	}
}

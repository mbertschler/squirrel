package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// deltaKey flattens a PathDelta to the (path, status) pair assertions
// key on.
type deltaKey struct{ path, status string }

func deltaKeys(delta []PathDelta) []deltaKey {
	out := make([]deltaKey, 0, len(delta))
	for _, d := range delta {
		out = append(out, deltaKey{d.Path, d.Status})
	}
	return out
}

// TestListPathDeltaSince drives one volume through two "sync epochs"
// and checks the delta read returns exactly the rows whose status
// changed after the watermark: the full state at watermark 0, and the
// add/supersede/missing slice after the second index pass.
func TestListPathDeltaSince(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	upsert := func(runID int64, path string, digestByte byte) {
		t.Helper()
		if err := s.Upsert(ctx, FileRow{
			VolumeID: vID, Path: path, Blake3: digest(digestByte), SizeBytes: 1,
			MtimeNs: runID, Status: StatusPresent,
			FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: runID,
		}, nil); err != nil {
			t.Fatalf("Upsert %s: %v", path, err)
		}
	}

	r1 := makeRun(t, s, vID)
	upsert(r1, "a.txt", 0xaa)
	upsert(r1, "c.txt", 0xcc)

	full, err := s.ListPathDeltaSince(ctx, vID, 0)
	if err != nil {
		t.Fatalf("ListPathDeltaSince(0): %v", err)
	}
	wantFull := []deltaKey{{"a.txt", StatusPresent}, {"c.txt", StatusPresent}}
	if got := deltaKeys(full); len(got) != 2 || got[0] != wantFull[0] || got[1] != wantFull[1] {
		t.Fatalf("full delta = %v, want %v", got, wantFull)
	}

	// Second epoch: a.txt changes content, c.txt disappears, d.txt is
	// new. TouchSeen on the changed rows mirrors what a real index run
	// does for unchanged files (none here beyond the upserts).
	r2 := makeRun(t, s, vID)
	upsert(r2, "a.txt", 0xab)
	upsert(r2, "d.txt", 0xdd)
	if _, err := s.MarkMissing(ctx, vID, r2); err != nil {
		t.Fatalf("MarkMissing: %v", err)
	}

	delta, err := s.ListPathDeltaSince(ctx, vID, r1)
	if err != nil {
		t.Fatalf("ListPathDeltaSince(%d): %v", r1, err)
	}
	want := []deltaKey{
		{"a.txt", StatusPresent},
		{"a.txt", StatusSuperseded},
		{"c.txt", StatusMissing},
		{"d.txt", StatusPresent},
	}
	got := deltaKeys(delta)
	if len(got) != len(want) {
		t.Fatalf("delta = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delta[%d] = %v, want %v", i, got[i], want[i])
		}
	}

	// A watermark past every change yields the empty delta.
	empty, err := s.ListPathDeltaSince(ctx, vID, r2)
	if err != nil {
		t.Fatalf("ListPathDeltaSince(%d): %v", r2, err)
	}
	if len(empty) != 0 {
		t.Fatalf("delta past every change = %v, want empty", deltaKeys(empty))
	}
}

// TestListPathDeltaSinceExcludesReservedSubtrees: rows under the
// squirrel-reserved directories never travel to a destination, so they
// must not surface in the manifest delta either.
func TestListPathDeltaSinceExcludesReservedSubtrees(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	r1 := makeRun(t, s, vID)

	reserved := []string{
		".squirrel-history/run-1/old.txt",
		".squirrel-conflicts/run-2/x.txt",
		".squirrel-restore-history/run-3/y.txt",
		".squirrel-index/index-snapshot.db",
	}
	for i, p := range reserved {
		if err := s.Upsert(ctx, FileRow{
			VolumeID: vID, Path: p, Blake3: digest(byte(0x50 + i)), SizeBytes: 1,
			MtimeNs: 1, Status: StatusPresent,
			FirstSeenRunID: r1, LastSeenRunID: r1, IndexedAtNs: 1,
		}, nil); err != nil {
			t.Fatalf("Upsert %s: %v", p, err)
		}
	}
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "real.txt", Blake3: digest(0x60), SizeBytes: 1,
		MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: r1, LastSeenRunID: r1, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert real.txt: %v", err)
	}

	delta, err := s.ListPathDeltaSince(ctx, vID, 0)
	if err != nil {
		t.Fatalf("ListPathDeltaSince: %v", err)
	}
	if len(delta) != 1 || delta[0].Path != "real.txt" {
		t.Fatalf("delta = %v, want only real.txt", deltaKeys(delta))
	}
}

// TestLatestSuccessfulSyncRun pins the watermark choice: only a
// status='success' sync of the same (volume, destination) counts —
// failed and partial runs left no confirmed segment, and other
// destinations' successes are someone else's watermark.
func TestLatestSuccessfulSyncRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	if _, err := s.LatestSuccessfulSyncRun(ctx, vID, "offsite"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows before any sync", err)
	}

	finish := func(dest, status string) int64 {
		t.Helper()
		id, err := s.BeginRun(ctx, RunKindSync, vID, dest, true)
		if err != nil {
			t.Fatalf("BeginRun: %v", err)
		}
		if err := s.FinishRun(ctx, id, status, "", 0); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
		return id
	}

	want := finish("offsite", RunStatusSuccess)
	finish("offsite", RunStatusFailed)
	finish("offsite", RunStatusPartial)
	finish("other", RunStatusSuccess)

	got, err := s.LatestSuccessfulSyncRun(ctx, vID, "offsite")
	if err != nil {
		t.Fatalf("LatestSuccessfulSyncRun: %v", err)
	}
	if got.ID != want {
		t.Fatalf("watermark run = %d, want %d", got.ID, want)
	}
}

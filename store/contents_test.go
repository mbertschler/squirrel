package store

import (
	"context"
	"strings"
	"testing"
)

// TestUpsertRejectsSizeMismatchForKnownDigest: a digest the index
// already maps to a different size means corruption or a mis-hashing
// caller, so the upsert errors instead of recording the observation.
func TestUpsertRejectsSizeMismatchForKnownDigest(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run := makeRun(t, s, vID)

	d := digest(0x7a)
	row := FileRow{
		VolumeID: vID, Path: "a.txt", Blake3: d,
		SizeBytes: 10, MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1,
	}
	if err := s.Upsert(ctx, row, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	row.Path = "b.txt"
	row.SizeBytes = 11
	err := s.Upsert(ctx, row, nil)
	if err == nil {
		t.Fatalf("same digest with different size accepted, want error")
	}
	if !strings.Contains(err.Error(), "size") {
		t.Fatalf("error = %v, want one naming the size disagreement", err)
	}
}

// TestContentIntroductionRunID: the introduction run is the earliest
// first_seen_run_id across every observation of the content in the
// volume — later duplicate paths don't move it, and neither does the
// original observation being superseded (introduction is history, not
// the live set).
func TestContentIntroductionRunID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run1 := makeRun(t, s, vID)
	run2 := makeRun(t, s, vID)

	d := digest(0x31)
	row := FileRow{
		VolumeID: vID, Path: "a.txt", Blake3: d,
		SizeBytes: 3, MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: run1, LastSeenRunID: run1, IndexedAtNs: 1,
	}
	if err := s.Upsert(ctx, row, nil); err != nil {
		t.Fatalf("Upsert a.txt: %v", err)
	}
	row.Path = "b.txt"
	row.FirstSeenRunID, row.LastSeenRunID = run2, run2
	if err := s.Upsert(ctx, row, nil); err != nil {
		t.Fatalf("Upsert b.txt: %v", err)
	}

	live, err := s.GetByPath(ctx, vID, "a.txt")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	intro, err := s.ContentIntroductionRunID(ctx, vID, live.ContentID)
	if err != nil {
		t.Fatalf("ContentIntroductionRunID: %v", err)
	}
	if intro != run1 {
		t.Fatalf("introduction run = %d, want %d", intro, run1)
	}

	// Superseding the original observation keeps the introduction run.
	newer := FileRow{
		VolumeID: vID, Path: "a.txt", Blake3: digest(0x32),
		SizeBytes: 3, MtimeNs: 2, Status: StatusPresent,
		FirstSeenRunID: run2, LastSeenRunID: run2, IndexedAtNs: 2,
	}
	if err := s.Upsert(ctx, newer, nil); err != nil {
		t.Fatalf("Upsert newer a.txt: %v", err)
	}
	intro, err = s.ContentIntroductionRunID(ctx, vID, live.ContentID)
	if err != nil {
		t.Fatalf("ContentIntroductionRunID after supersede: %v", err)
	}
	if intro != run1 {
		t.Fatalf("introduction run after supersede = %d, want %d", intro, run1)
	}

	if _, err := s.ContentIntroductionRunID(ctx, vID, live.ContentID+999); !IsNotFound(err) {
		t.Fatalf("unknown content err = %v, want sql.ErrNoRows", err)
	}
}

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

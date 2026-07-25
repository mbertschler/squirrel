package tui

import (
	"database/sql"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

// TestDestinationCell covers the F18 direction arrow in the runs table:
// bucket rows render the plain name, peer rows prefix the initiation arrow
// (→ outbound / ← inbound, keyed off runs.shallow).
func TestDestinationCell(t *testing.T) {
	bucket := store.Run{Destination: sql.NullString{String: "s3archive", Valid: true}}
	if got := destinationCell(bucket); got != "s3archive" {
		t.Errorf("bucket cell = %q, want %q", got, "s3archive")
	}
	outbound := store.Run{
		Destination: sql.NullString{String: "htpc", Valid: true},
		PeerNodeID:  sql.NullInt64{Int64: 3, Valid: true},
		Shallow:     sql.NullBool{Valid: true},
	}
	if got := destinationCell(outbound); got != "→ htpc" {
		t.Errorf("outbound cell = %q, want %q", got, "→ htpc")
	}
	inbound := store.Run{
		Destination: sql.NullString{String: "laptop", Valid: true},
		PeerNodeID:  sql.NullInt64{Int64: 4, Valid: true},
	}
	if got := destinationCell(inbound); got != "← laptop" {
		t.Errorf("inbound cell = %q, want %q", got, "← laptop")
	}
}

// TestApplyFilterFoldsBucketNoOps covers the F19 fold now that it keys on
// runs.changed_count (#182): consecutive bucket pushes that transferred
// nothing collapse behind one marker even though their file_count counts
// the whole volume, while the push that moved content stays on screen.
func TestApplyFilterFoldsBucketNoOps(t *testing.T) {
	bucketRun := func(id, changed int64) store.Run {
		return store.Run{
			ID:           id,
			Kind:         store.RunKindSync,
			Destination:  sql.NullString{String: "s3archive", Valid: true},
			Status:       store.RunStatusSuccess,
			FileCount:    42,
			ChangedCount: sql.NullInt64{Int64: changed, Valid: true},
		}
	}
	m := newRunsModel(nil)
	m.resizeColumns()
	m.rows = []store.Run{bucketRun(4, 0), bucketRun(3, 0), bucketRun(2, 3), bucketRun(1, 0)}
	m.applyFilter()

	if m.foldedCount != 2 {
		t.Errorf("foldedCount = %d, want 2 (the pair of in-sync pushes)", m.foldedCount)
	}
	// Marker, the run that moved content, the lone trailing no-op.
	if len(m.displayRuns) != 3 {
		t.Fatalf("displayed %d rows, want 3: %+v", len(m.displayRuns), m.displayRuns)
	}
	if m.displayRuns[0] != nil {
		t.Errorf("row 0 should be the fold marker, got run %+v", m.displayRuns[0])
	}
	if m.displayRuns[1] == nil || m.displayRuns[1].ID != 2 {
		t.Errorf("row 1 should be run 2 (transferred 3 files), got %+v", m.displayRuns[1])
	}
	if m.displayRuns[2] == nil || m.displayRuns[2].ID != 1 {
		t.Errorf("row 2 should be the lone no-op run 1, got %+v", m.displayRuns[2])
	}
}

// TestApplyFilterKeepsPreV28NoOps covers the fallback: history written
// before changed_count existed keeps its conservative rendering, so a pair
// of bucket pushes with a non-zero file_count and no changed count stays
// visible rather than folding on a guess.
func TestApplyFilterKeepsPreV28NoOps(t *testing.T) {
	legacy := func(id int64) store.Run {
		return store.Run{
			ID:          id,
			Kind:        store.RunKindSync,
			Destination: sql.NullString{String: "s3archive", Valid: true},
			Status:      store.RunStatusSuccess,
			FileCount:   42,
		}
	}
	m := newRunsModel(nil)
	m.resizeColumns()
	m.rows = []store.Run{legacy(2), legacy(1)}
	m.applyFilter()

	if m.foldedCount != 0 {
		t.Errorf("foldedCount = %d, want 0 — unknown changed counts must not fold", m.foldedCount)
	}
	if len(m.displayRuns) != 2 {
		t.Errorf("displayed %d rows, want both runs", len(m.displayRuns))
	}
}

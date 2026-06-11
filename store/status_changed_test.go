package store

import (
	"context"
	"database/sql"
	"testing"
)

// statusChangedRun reads the stamp for the single row at (volume, path)
// carrying the given status. Tests keep at most one row per (path,
// status) so the read is unambiguous.
func statusChangedRun(t *testing.T, s *Store, volumeID int64, path, status string) int64 {
	t.Helper()
	folderPath, name := splitFilePath(path)
	var v sql.NullInt64
	err := s.db.QueryRowContext(context.Background(), `
		SELECT f.status_changed_run_id FROM files f
		JOIN folders fo ON fo.id = f.folder_id
		WHERE fo.volume_id = ? AND fo.path = ? AND f.name = ? AND f.status = ?
	`, volumeID, folderPath, name, status).Scan(&v)
	if err != nil {
		t.Fatalf("read status_changed_run_id for %s (%s): %v", path, status, err)
	}
	if !v.Valid {
		t.Fatalf("status_changed_run_id for %s (%s) is NULL", path, status)
	}
	return v.Int64
}

// TestStatusChangedRunStamps walks one path through every transition
// the row state machine supports and pins where the stamp lands:
// insert, unchanged re-observation, supersession, missing flip,
// reappearance, and content revert. The stamp is what the
// content-addressed manifest delta keys on, so each transition must
// move it exactly once — and the no-op touch must leave it alone.
func TestStatusChangedRunStamps(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	upsert := func(runID int64, digestByte byte) {
		t.Helper()
		if err := s.Upsert(ctx, FileRow{
			VolumeID: vID, Path: "a.txt", Blake3: digest(digestByte), SizeBytes: 1,
			MtimeNs: runID, Status: StatusPresent,
			FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: runID,
		}, nil); err != nil {
			t.Fatalf("Upsert run %d: %v", runID, err)
		}
	}

	r1 := makeRun(t, s, vID)
	upsert(r1, 0x11)
	if got := statusChangedRun(t, s, vID, "a.txt", StatusPresent); got != r1 {
		t.Fatalf("insert stamp = %d, want %d", got, r1)
	}

	r2 := makeRun(t, s, vID)
	if err := s.TouchSeen(ctx, vID, "a.txt", r2); err != nil {
		t.Fatalf("TouchSeen: %v", err)
	}
	if got := statusChangedRun(t, s, vID, "a.txt", StatusPresent); got != r1 {
		t.Fatalf("unchanged re-observation moved the stamp to %d, want %d", got, r1)
	}

	r3 := makeRun(t, s, vID)
	upsert(r3, 0x22)
	if got := statusChangedRun(t, s, vID, "a.txt", StatusSuperseded); got != r3 {
		t.Fatalf("superseded stamp = %d, want %d", got, r3)
	}
	if got := statusChangedRun(t, s, vID, "a.txt", StatusPresent); got != r3 {
		t.Fatalf("replacement stamp = %d, want %d", got, r3)
	}

	r4 := makeRun(t, s, vID)
	if _, err := s.MarkMissing(ctx, vID, r4); err != nil {
		t.Fatalf("MarkMissing: %v", err)
	}
	if got := statusChangedRun(t, s, vID, "a.txt", StatusMissing); got != r4 {
		t.Fatalf("missing stamp = %d, want %d", got, r4)
	}

	r5 := makeRun(t, s, vID)
	if err := s.TouchSeen(ctx, vID, "a.txt", r5); err != nil {
		t.Fatalf("TouchSeen reappear: %v", err)
	}
	if got := statusChangedRun(t, s, vID, "a.txt", StatusPresent); got != r5 {
		t.Fatalf("reappear stamp = %d, want %d", got, r5)
	}

	// Content revert: the original digest comes back, reviving the
	// superseded row and superseding the one that displaced it.
	r6 := makeRun(t, s, vID)
	upsert(r6, 0x11)
	if got := statusChangedRun(t, s, vID, "a.txt", StatusPresent); got != r6 {
		t.Fatalf("revived stamp = %d, want %d", got, r6)
	}
	if got := statusChangedRun(t, s, vID, "a.txt", StatusSuperseded); got != r6 {
		t.Fatalf("displaced stamp = %d, want %d", got, r6)
	}
}

package store

import (
	"context"
	"errors"
	"testing"
)

// TestUpsertDestinationRunIDWritesHistory: every successful advance
// appends one destination_run_ids_history row alongside updating the
// live vector component — the same append-only contract the peer-sync
// watermark has.
func TestUpsertDestinationRunIDWritesHistory(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	node, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}

	for _, run := range []int64{7, 20, 42} {
		if err := s.UpsertDestinationRunID(ctx, vID, "bucket-a", node.ID, run, false); err != nil {
			t.Fatalf("UpsertDestinationRunID(%d): %v", run, err)
		}
	}

	history, err := s.ListDestinationRunIDHistory(ctx, vID, "bucket-a")
	if err != nil {
		t.Fatalf("ListDestinationRunIDHistory: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history rows = %d, want 3", len(history))
	}
	want := []int64{7, 20, 42}
	for i, h := range history {
		if h.OriginRunID != want[i] {
			t.Fatalf("history[%d] origin run = %d, want %d", i, h.OriginRunID, want[i])
		}
		if h.VolumeID != vID || h.Destination != "bucket-a" || h.OriginNodeID != node.ID {
			t.Fatalf("history[%d] key = (%d,%q,%d), want (%d,%q,%d)",
				i, h.VolumeID, h.Destination, h.OriginNodeID, vID, "bucket-a", node.ID)
		}
	}

	got, err := s.GetDestinationRunID(ctx, vID, "bucket-a", node.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.OriginRunID != 42 {
		t.Fatalf("live origin run = %d, want 42", got.OriginRunID)
	}
}

// TestUpsertDestinationRunIDRefusesRewind: a component move below the
// recorded value is refused by default with a *DestinationRewindError
// (wrapping the shared ErrWatermarkRewind), the live row is left
// untouched, and no history row is appended for the rejected move.
// allowRewind overrides for genuine recovery and is logged to history.
func TestUpsertDestinationRunIDRefusesRewind(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	node, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}

	if err := s.UpsertDestinationRunID(ctx, vID, "bucket-a", node.ID, 40, false); err != nil {
		t.Fatalf("seed advance: %v", err)
	}

	err = s.UpsertDestinationRunID(ctx, vID, "bucket-a", node.ID, 3, false)
	if !errors.Is(err, ErrWatermarkRewind) {
		t.Fatalf("rewind err = %v, want ErrWatermarkRewind", err)
	}
	var rewErr *DestinationRewindError
	if !errors.As(err, &rewErr) {
		t.Fatalf("err = %v, want *DestinationRewindError", err)
	}
	if rewErr.Current != 40 || rewErr.Attempted != 3 || rewErr.Destination != "bucket-a" {
		t.Fatalf("rewind detail = %+v, want current=40 attempted=3 destination=bucket-a", rewErr)
	}

	live, err := s.GetDestinationRunID(ctx, vID, "bucket-a", node.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID after refusal: %v", err)
	}
	if live.OriginRunID != 40 {
		t.Fatalf("live origin run = %d after refused rewind, want 40", live.OriginRunID)
	}
	history, err := s.ListDestinationRunIDHistory(ctx, vID, "bucket-a")
	if err != nil {
		t.Fatalf("ListDestinationRunIDHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("history rows = %d after refused rewind, want 1", len(history))
	}

	if err := s.UpsertDestinationRunID(ctx, vID, "bucket-a", node.ID, 3, true); err != nil {
		t.Fatalf("allowRewind override: %v", err)
	}
	live, _ = s.GetDestinationRunID(ctx, vID, "bucket-a", node.ID)
	if live.OriginRunID != 3 {
		t.Fatalf("live origin run = %d after override, want 3", live.OriginRunID)
	}
	history, _ = s.ListDestinationRunIDHistory(ctx, vID, "bucket-a")
	if len(history) != 2 {
		t.Fatalf("history rows = %d after override, want 2 (override is logged)", len(history))
	}
}

// TestDestinationRunIDVector: the vector for one destination carries
// one independent component per origin node, scoped per destination
// and per volume.
func TestDestinationRunIDVector(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	peer, err := s.CreateNode(ctx, "peer", "https://peer.example")
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	advances := []struct {
		destination string
		nodeID      int64
		run         int64
	}{
		{"bucket-a", self.ID, 10},
		{"bucket-a", peer.ID, 4},
		{"bucket-b", self.ID, 2},
	}
	for _, a := range advances {
		if err := s.UpsertDestinationRunID(ctx, vID, a.destination, a.nodeID, a.run, false); err != nil {
			t.Fatalf("advance %+v: %v", a, err)
		}
	}

	vector, err := s.ListDestinationRunIDs(ctx, vID, "bucket-a")
	if err != nil {
		t.Fatalf("ListDestinationRunIDs: %v", err)
	}
	if len(vector) != 2 {
		t.Fatalf("vector components = %d, want 2", len(vector))
	}
	byNode := map[int64]int64{}
	for _, c := range vector {
		byNode[c.OriginNodeID] = c.OriginRunID
	}
	if byNode[self.ID] != 10 || byNode[peer.ID] != 4 {
		t.Fatalf("vector = %+v, want self→10 peer→4", byNode)
	}

	// bucket-b's component for self is independent of bucket-a's.
	got, err := s.GetDestinationRunID(ctx, vID, "bucket-b", self.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID bucket-b: %v", err)
	}
	if got.OriginRunID != 2 {
		t.Fatalf("bucket-b self component = %d, want 2", got.OriginRunID)
	}
}

// TestUpsertDestinationRunIDRejectsEmptyDestination: the destination is
// the vector's identity, so it must be non-empty.
func TestUpsertDestinationRunIDRejectsEmptyDestination(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	node, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	if err := s.UpsertDestinationRunID(ctx, vID, "", node.ID, 1, false); err == nil {
		t.Fatalf("empty destination accepted, want error")
	}
}

// TestAdvanceDestinationVector: the advance computes one component per
// origin node over the volume's present rows — locally-introduced
// content under the self node at its observation's first_seen_run_id,
// forwarded content under its recorded origin verbatim — and excludes
// non-present rows and the reserved sync subtrees.
func TestAdvanceDestinationVector(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run1 := makeRun(t, s, vID)
	run2 := makeRun(t, s, vID)
	run3 := makeRun(t, s, vID)
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	ext, err := s.CreateNode(ctx, "ext", "peer://ext")
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	upsert := func(path string, b byte, status string, firstSeen int64, prov *Provenance) {
		t.Helper()
		if err := s.Upsert(ctx, FileRow{
			VolumeID: vID, Path: path, Blake3: digest(b),
			SizeBytes: 1, MtimeNs: 1, Status: status,
			FirstSeenRunID: firstSeen, LastSeenRunID: firstSeen, IndexedAtNs: 1,
		}, prov); err != nil {
			t.Fatalf("Upsert %s: %v", path, err)
		}
	}
	upsert("a.txt", 0xA1, StatusPresent, run1, nil)
	upsert("b.txt", 0xA2, StatusPresent, run2, nil)
	upsert("c.txt", 0xA3, StatusPresent, run1, &Provenance{NodeID: ext.ID, RunID: 50})
	// Non-present and reserved-subtree rows must not advance anything:
	// gone.txt would push the self component to run3, and the conflict
	// leftover would push ext to 999.
	upsert("gone.txt", 0xA4, StatusMissing, run3, nil)
	upsert(".squirrel-conflicts/run-1/x.bin", 0xA5, StatusPresent, run3, &Provenance{NodeID: ext.ID, RunID: 999})

	if err := s.AdvanceDestinationVector(ctx, vID, "nas"); err != nil {
		t.Fatalf("AdvanceDestinationVector: %v", err)
	}
	vector, err := s.ListDestinationRunIDs(ctx, vID, "nas")
	if err != nil {
		t.Fatalf("ListDestinationRunIDs: %v", err)
	}
	byNode := map[int64]int64{}
	for _, c := range vector {
		byNode[c.OriginNodeID] = c.OriginRunID
	}
	if len(byNode) != 2 || byNode[self.ID] != run2 || byNode[ext.ID] != 50 {
		t.Fatalf("vector = %+v, want self→%d ext→50", byNode, run2)
	}
}

// TestAdvanceDestinationVectorKeepsHigherComponent: a recorded
// component above the computed present-set maximum stays in place (the
// destination is append-only, so the higher floor still holds) and the
// advance reports no error — componentwise max, not a rewind.
func TestAdvanceDestinationVectorKeepsHigherComponent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run1 := makeRun(t, s, vID)
	ext, err := s.CreateNode(ctx, "ext", "peer://ext")
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "c.txt", Blake3: digest(0xB1),
		SizeBytes: 1, MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: run1, LastSeenRunID: run1, IndexedAtNs: 1,
	}, &Provenance{NodeID: ext.ID, RunID: 50}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.UpsertDestinationRunID(ctx, vID, "nas", ext.ID, 60, false); err != nil {
		t.Fatalf("seed component: %v", err)
	}

	if err := s.AdvanceDestinationVector(ctx, vID, "nas"); err != nil {
		t.Fatalf("AdvanceDestinationVector: %v", err)
	}
	got, err := s.GetDestinationRunID(ctx, vID, "nas", ext.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.OriginRunID != 60 {
		t.Fatalf("ext component = %d, want 60 (higher recorded floor kept)", got.OriginRunID)
	}
}

// TestListVolumeDestinationRunIDs returns components across every
// destination of the volume, ordered by destination then origin node.
func TestListVolumeDestinationRunIDs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	otherVol := makeVolume(t, s, "/other")
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	for _, a := range []struct {
		volID int64
		dest  string
		run   int64
	}{
		{vID, "bucket-b", 7},
		{vID, "bucket-a", 3},
		{otherVol, "bucket-a", 99},
	} {
		if err := s.UpsertDestinationRunID(ctx, a.volID, a.dest, self.ID, a.run, false); err != nil {
			t.Fatalf("seed %+v: %v", a, err)
		}
	}

	rows, err := s.ListVolumeDestinationRunIDs(ctx, vID)
	if err != nil {
		t.Fatalf("ListVolumeDestinationRunIDs: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (other volume excluded)", len(rows))
	}
	if rows[0].Destination != "bucket-a" || rows[0].OriginRunID != 3 ||
		rows[1].Destination != "bucket-b" || rows[1].OriginRunID != 7 {
		t.Fatalf("rows = %+v, want bucket-a→3 then bucket-b→7", rows)
	}
}

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

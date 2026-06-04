package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestGetSelfNode verifies the self row inserted at v6 migration is
// the one returned by GetSelfNode (endpoint IS NULL), even after a
// peer row with a non-NULL endpoint has been added.
func TestGetSelfNode(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenWithOptions(dsn, OpenOptions{NodeName: "local"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	if self.Name != "local" {
		t.Fatalf("self name = %q, want local", self.Name)
	}
	if self.Endpoint.Valid {
		t.Fatalf("self endpoint should be NULL, got %q", self.Endpoint.String)
	}

	// Adding a peer row must not shift which row GetSelfNode returns.
	if _, err := s.CreateNode(ctx, "peer", "http://peer.example"); err != nil {
		t.Fatalf("CreateNode peer: %v", err)
	}
	self2, _ := s.GetSelfNode(ctx)
	if self2.ID != self.ID {
		t.Fatalf("self id shifted: %d → %d", self.ID, self2.ID)
	}
}

// TestGetOrCreatePeerNodeIdempotent inserts on first call and
// returns the existing row (without modification) on a second call
// with the same endpoint.
func TestGetOrCreatePeerNodeIdempotent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, _ := Open(dsn)
	defer s.Close()
	ctx := context.Background()

	first, err := s.GetOrCreatePeerNode(ctx, "nas", "https://nas.local")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	again, err := s.GetOrCreatePeerNode(ctx, "nas", "https://nas.local")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.ID != again.ID {
		t.Fatalf("ids differ: %d vs %d", first.ID, again.ID)
	}
}

// TestGetOrCreatePeerNodeRejectsEndpointMismatch is the design
// guarantee: a name presented twice with different endpoints fails
// rather than silently overwriting.
func TestGetOrCreatePeerNodeRejectsEndpointMismatch(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, _ := Open(dsn)
	defer s.Close()
	ctx := context.Background()

	if _, err := s.GetOrCreatePeerNode(ctx, "nas", "https://nas.local"); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := s.GetOrCreatePeerNode(ctx, "nas", "https://nas.different")
	if err == nil || !strings.Contains(err.Error(), "already has endpoint") {
		t.Fatalf("error = %v, want collision-message", err)
	}
}

// TestGetOrCreatePeerNodeRefusesSelfNameCollision: a peer presenting
// the local self-row's name must be rejected (the self row has
// endpoint NULL; overwriting it would corrupt the local identity).
func TestGetOrCreatePeerNodeRefusesSelfNameCollision(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenWithOptions(dsn, OpenOptions{NodeName: "me"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	_, err = s.GetOrCreatePeerNode(ctx, "me", "http://attacker.example")
	if err == nil || !strings.Contains(err.Error(), "self-row") {
		t.Fatalf("error = %v, want self-row refusal", err)
	}
}

// TestPeerSyncStateUpsertRoundtrip writes a watermark and reads it
// back; a second upsert advances the value in place.
func TestPeerSyncStateUpsertRoundtrip(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, _ := Open(dsn)
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	peer, _ := s.GetOrCreatePeerNode(ctx, "nas", "http://nas.example")

	if err := s.UpsertPeerSyncState(ctx, vID, peer.ID, 7, false); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	state, err := s.GetPeerSyncState(ctx, vID, peer.ID)
	if err != nil {
		t.Fatalf("GetPeerSyncState: %v", err)
	}
	if !state.LastSharedRunID.Valid || state.LastSharedRunID.Int64 != 7 {
		t.Fatalf("watermark = %+v, want 7", state.LastSharedRunID)
	}

	if err := s.UpsertPeerSyncState(ctx, vID, peer.ID, 42, false); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	state, _ = s.GetPeerSyncState(ctx, vID, peer.ID)
	if state.LastSharedRunID.Int64 != 42 {
		t.Fatalf("watermark after advance = %d, want 42", state.LastSharedRunID.Int64)
	}
}

// TestBeginPeerSyncRunStampsLinkage exercises the BeginPeerSyncRun /
// GetRun pair: peer_node_id and correlated_run_id must land on the
// row alongside the destination name.
func TestBeginPeerSyncRunStampsLinkage(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, _ := Open(dsn)
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	peer, _ := s.GetOrCreatePeerNode(ctx, "nas", "http://nas.example")

	id, err := s.BeginPeerSyncRun(ctx, vID, peer.ID, 99, "nas")
	if err != nil {
		t.Fatalf("BeginPeerSyncRun: %v", err)
	}
	row, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !row.PeerNodeID.Valid || row.PeerNodeID.Int64 != peer.ID {
		t.Fatalf("PeerNodeID = %+v, want %d", row.PeerNodeID, peer.ID)
	}
	if !row.CorrelatedRunID.Valid || row.CorrelatedRunID.Int64 != 99 {
		t.Fatalf("CorrelatedRunID = %+v, want 99", row.CorrelatedRunID)
	}
	if !row.Destination.Valid || row.Destination.String != "nas" {
		t.Fatalf("Destination = %+v, want 'nas'", row.Destination)
	}
}

// TestSetCorrelatedRunID stamps the correlated id post-BeginRun. The
// initiator side uses this because the receiver's id is only known
// after /v1/sync/begin returns.
func TestSetCorrelatedRunID(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, _ := Open(dsn)
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	peer, _ := s.GetOrCreatePeerNode(ctx, "nas", "http://nas.example")
	id, _ := s.BeginPeerSyncRun(ctx, vID, peer.ID, 0, "nas")

	if err := s.SetCorrelatedRunID(ctx, id, 1234); err != nil {
		t.Fatalf("SetCorrelatedRunID: %v", err)
	}
	row, _ := s.GetRun(ctx, id)
	if row.CorrelatedRunID.Int64 != 1234 {
		t.Fatalf("CorrelatedRunID = %d, want 1234", row.CorrelatedRunID.Int64)
	}
	if err := s.SetCorrelatedRunID(ctx, 99999, 1); err == nil {
		t.Fatalf("expected no-such-run error")
	}
}

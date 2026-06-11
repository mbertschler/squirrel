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

	first, err := s.GetOrCreatePeerNode(ctx, "nas", "https://nas.local", true)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	again, err := s.GetOrCreatePeerNode(ctx, "nas", "https://nas.local", true)
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

	if _, err := s.GetOrCreatePeerNode(ctx, "nas", "https://nas.local", true); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := s.GetOrCreatePeerNode(ctx, "nas", "https://nas.different", true)
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

	_, err = s.GetOrCreatePeerNode(ctx, "me", "http://attacker.example", true)
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
	peer, _ := s.GetOrCreatePeerNode(ctx, "nas", "http://nas.example", true)

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
	peer, _ := s.GetOrCreatePeerNode(ctx, "nas", "http://nas.example", true)

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
	peer, _ := s.GetOrCreatePeerNode(ctx, "nas", "http://nas.example", true)
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

// TestGetOrCreateOriginNode covers the three name-resolution outcomes
// the verbatim origin-propagation path needs: an unknown name creates a
// placeholder-endpoint row (a forwarded origin may name a node this
// host has never peered with), a known peer row is returned as-is, and
// the self name resolves to the self-row rather than colliding with it.
func TestGetOrCreateOriginNode(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenWithOptions(dsn, OpenOptions{NodeName: "local"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	created, err := s.GetOrCreateOriginNode(ctx, "far-away")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode(far-away): %v", err)
	}
	if !created.Endpoint.Valid || created.Endpoint.String != "peer://far-away" {
		t.Fatalf("created endpoint = %+v, want the peer://far-away placeholder", created.Endpoint)
	}
	again, err := s.GetOrCreateOriginNode(ctx, "far-away")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode(far-away) again: %v", err)
	}
	if again.ID != created.ID {
		t.Fatalf("second resolve created a new row: %d → %d", created.ID, again.ID)
	}

	peer, err := s.GetOrCreatePeerNode(ctx, "nas", "https://nas.example", true)
	if err != nil {
		t.Fatalf("GetOrCreatePeerNode: %v", err)
	}
	byOrigin, err := s.GetOrCreateOriginNode(ctx, "nas")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode(nas): %v", err)
	}
	if byOrigin.ID != peer.ID || byOrigin.Endpoint.String != "https://nas.example" {
		t.Fatalf("origin resolve of a peer = %+v, want the existing peer row %+v", byOrigin, peer)
	}

	self, _ := s.GetSelfNode(ctx)
	bySelfName, err := s.GetOrCreateOriginNode(ctx, "local")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode(local): %v", err)
	}
	if bySelfName.ID != self.ID {
		t.Fatalf("self name resolved to row %d, want the self-row %d", bySelfName.ID, self.ID)
	}
}

// TestGetOrCreateOriginNodeRejectsInvalidName pins that the node-name
// rule guards origin creation too — a wire-supplied name that fails
// nodeNameRE must not land a row. ValidNodeName is the predicate the
// protocol layer uses to refuse such names up front.
func TestGetOrCreateOriginNodeRejectsInvalidName(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, _ := Open(dsn)
	defer s.Close()

	if ValidNodeName("../etc") {
		t.Fatalf("ValidNodeName accepted a traversal-shaped name")
	}
	if !ValidNodeName("node-a_2") {
		t.Fatalf("ValidNodeName rejected a compliant name")
	}
	if _, err := s.GetOrCreateOriginNode(context.Background(), "../etc"); err == nil {
		t.Fatalf("invalid origin node name accepted, want error")
	}
}

// TestGetOrCreatePeerNodeUpgradesPlaceholder: a row created from a
// name-only context (a forwarded origin, or a durability pull before
// any sync) carries the peer:// placeholder; an operator-configured
// (trusted) caller presenting an actual endpoint upgrades it in place
// instead of refusing the collision. Real-endpoint mismatches stay
// refused.
func TestGetOrCreatePeerNodeUpgradesPlaceholder(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, _ := Open(dsn)
	defer s.Close()
	ctx := context.Background()

	seeded, err := s.GetOrCreateOriginNode(ctx, "nas")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode: %v", err)
	}
	upgraded, err := s.GetOrCreatePeerNode(ctx, "nas", "https://nas.example:8443", true)
	if err != nil {
		t.Fatalf("GetOrCreatePeerNode after placeholder: %v", err)
	}
	if upgraded.ID != seeded.ID {
		t.Fatalf("upgrade created a new row: %d → %d", seeded.ID, upgraded.ID)
	}
	if upgraded.Endpoint.String != "https://nas.example:8443" {
		t.Fatalf("endpoint = %q, want the upgraded real endpoint", upgraded.Endpoint.String)
	}
	persisted, _ := s.GetNodeByName(ctx, "nas")
	if persisted.Endpoint.String != "https://nas.example:8443" {
		t.Fatalf("persisted endpoint = %q, want the upgrade written through", persisted.Endpoint.String)
	}

	if _, err := s.GetOrCreatePeerNode(ctx, "nas", "https://other.example", true); err == nil {
		t.Fatalf("real-endpoint mismatch accepted, want refusal")
	}
}

// TestGetOrCreatePeerNodeUntrustedKeepsPlaceholder is the #110b guard:
// an untrusted caller (allowEndpointUpgrade=false, the receiver-side
// /begin path whose endpoint derives from wire input) must not rebind a
// placeholder row to a presented endpoint. The placeholder stays put so
// a peer cannot point an arbitrary node-name's dial-back URL at an
// attacker address; the existing row is returned unchanged.
func TestGetOrCreatePeerNodeUntrustedKeepsPlaceholder(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, _ := Open(dsn)
	defer s.Close()
	ctx := context.Background()

	seeded, err := s.GetOrCreateOriginNode(ctx, "nas")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode: %v", err)
	}
	got, err := s.GetOrCreatePeerNode(ctx, "nas", "https://attacker.example:8443", false)
	if err != nil {
		t.Fatalf("GetOrCreatePeerNode untrusted: %v", err)
	}
	if got.ID != seeded.ID {
		t.Fatalf("untrusted call created a new row: %d → %d", seeded.ID, got.ID)
	}
	if got.Endpoint.String != "peer://nas" {
		t.Fatalf("returned endpoint = %q, want the untouched peer://nas placeholder", got.Endpoint.String)
	}
	persisted, _ := s.GetNodeByName(ctx, "nas")
	if persisted.Endpoint.String != "peer://nas" {
		t.Fatalf("persisted endpoint = %q, want the placeholder left in place", persisted.Endpoint.String)
	}
}

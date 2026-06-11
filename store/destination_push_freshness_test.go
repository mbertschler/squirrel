package store

import (
	"context"
	"testing"
)

// TestUpsertDestinationPushFreshnessOverwrites: the push-side upsert
// overwrites the coordinate to the latest push's value, including
// lowering it when a later push covered less (content removed from the
// pushing node's present set). The non-monotonic overwrite is what lets
// the offload gate refuse a relayed file the most recent push no longer
// covers.
func TestUpsertDestinationPushFreshnessOverwrites(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}

	if err := s.UpsertDestinationPushFreshness(ctx, vID, "offsite", self.ID, 10); err != nil {
		t.Fatalf("upsert 10: %v", err)
	}
	if err := s.UpsertDestinationPushFreshness(ctx, vID, "offsite", self.ID, 4); err != nil {
		t.Fatalf("upsert 4: %v", err)
	}
	fresh, err := s.ListDestinationPushFreshness(ctx, vID, "offsite")
	if err != nil {
		t.Fatalf("ListDestinationPushFreshness: %v", err)
	}
	if len(fresh) != 1 || fresh[0].OriginRunID != 4 {
		t.Fatalf("freshness = %+v, want one coordinate at 4 (overwrite, not max)", fresh)
	}
}

// TestMergeDestinationPushFreshnessMonotonic: the pull-side merge raises
// the coordinate only, so a stale pull never lowers the puller's cached
// evidence. Append-only targets make the highest coordinate ever observed
// the soundest cached fact.
func TestMergeDestinationPushFreshnessMonotonic(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}

	if err := s.MergeDestinationPushFreshness(ctx, vID, "offsite", self.ID, 10); err != nil {
		t.Fatalf("merge 10: %v", err)
	}
	if err := s.MergeDestinationPushFreshness(ctx, vID, "offsite", self.ID, 4); err != nil {
		t.Fatalf("merge 4: %v", err)
	}
	if err := s.MergeDestinationPushFreshness(ctx, vID, "offsite", self.ID, 12); err != nil {
		t.Fatalf("merge 12: %v", err)
	}
	fresh, err := s.ListDestinationPushFreshness(ctx, vID, "offsite")
	if err != nil {
		t.Fatalf("ListDestinationPushFreshness: %v", err)
	}
	if len(fresh) != 1 || fresh[0].OriginRunID != 12 {
		t.Fatalf("freshness = %+v, want one coordinate at 12 (monotonic max)", fresh)
	}
}

// TestAdvanceDestinationVectorRecordsFreshness: the snapshot-pinned
// advance path records push freshness from the same components it
// advances the vector with, so every gating whole-volume push leaves
// origin-space freshness behind for a downstream relayed target.
func TestAdvanceDestinationVectorRecordsFreshness(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	ext, err := s.CreateNode(ctx, "ext", "peer://ext")
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	components := []OriginComponent{
		{OriginNodeID: self.ID, OriginRunID: 5},
		{OriginNodeID: ext.ID, OriginRunID: 42},
	}
	if err := s.AdvanceDestinationVectorTo(ctx, vID, "offsite", VerifyMethodBlake3, components); err != nil {
		t.Fatalf("AdvanceDestinationVectorTo: %v", err)
	}
	fresh, err := s.ListDestinationPushFreshness(ctx, vID, "offsite")
	if err != nil {
		t.Fatalf("ListDestinationPushFreshness: %v", err)
	}
	byNode := map[int64]int64{}
	for _, f := range fresh {
		byNode[f.OriginNodeID] = f.OriginRunID
	}
	if len(byNode) != 2 || byNode[self.ID] != 5 || byNode[ext.ID] != 42 {
		t.Fatalf("freshness = %+v, want self→5 ext→42", byNode)
	}
}

// TestListVolumeDestinationPushFreshness returns coordinates across every
// destination of the volume, ordered by destination then origin node —
// the listing the durability endpoint serves.
func TestListVolumeDestinationPushFreshness(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	other := makeVolume(t, s, "/other")
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	for _, a := range []struct {
		volID int64
		dest  string
		run   int64
	}{
		{vID, "offsite-b", 7},
		{vID, "offsite-a", 3},
		{other, "offsite-a", 99},
	} {
		if err := s.UpsertDestinationPushFreshness(ctx, a.volID, a.dest, self.ID, a.run); err != nil {
			t.Fatalf("upsert %s: %v", a.dest, err)
		}
	}
	got, err := s.ListVolumeDestinationPushFreshness(ctx, vID)
	if err != nil {
		t.Fatalf("ListVolumeDestinationPushFreshness: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d coordinates, want 2 (the other volume excluded)", len(got))
	}
	if got[0].Destination != "offsite-a" || got[1].Destination != "offsite-b" {
		t.Fatalf("order = %q, %q, want offsite-a then offsite-b", got[0].Destination, got[1].Destination)
	}
}

package sync

import (
	"context"
	"strings"
	"testing"
)

// seedReceiverDurability records vector components on the receiver so
// the pull tests have something to fetch. Returns the receiver's self
// name (the origin-node identity its components travel under).
func seedReceiverDurability(t *testing.T, f *nodeFixture, components map[string]int64) string {
	t.Helper()
	ctx := context.Background()
	v, err := f.recvStore.CreateVolume(ctx, f.recvVol.Name, f.recvVol.Path)
	if err != nil {
		t.Fatalf("CreateVolume on receiver: %v", err)
	}
	self, err := f.recvStore.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode on receiver: %v", err)
	}
	for dest, run := range components {
		if err := f.recvStore.UpsertDestinationRunID(ctx, v.ID, dest, self.ID, run, false); err != nil {
			t.Fatalf("seed %s→%d: %v", dest, run, err)
		}
	}
	return self.Name
}

// seedReceiverFreshness records push-freshness coordinates on the
// receiver, mirroring seedReceiverDurability for the freshness table.
// Reuses the receiver volume the durability seed created when present.
func seedReceiverFreshness(t *testing.T, f *nodeFixture, coords map[string]int64) string {
	t.Helper()
	ctx := context.Background()
	v, err := f.recvStore.GetOrCreateVolume(ctx, f.recvVol.Path)
	if err != nil {
		t.Fatalf("GetOrCreateVolume on receiver: %v", err)
	}
	self, err := f.recvStore.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode on receiver: %v", err)
	}
	for dest, run := range coords {
		if err := f.recvStore.UpsertDestinationPushFreshness(ctx, v.ID, dest, self.ID, run); err != nil {
			t.Fatalf("seed freshness %s→%d: %v", dest, run, err)
		}
	}
	return self.Name
}

// TestPullDurabilityMergesFreshness: the pull fetches the peer's
// push-freshness coordinates alongside the vector and merges them into
// the LOCAL destination_push_freshness, so a relayed target's freshness
// evidence reaches a node that never pushes there. The merge is
// monotonic: a stale pull below a higher local value is ignored.
func TestPullDurabilityMergesFreshness(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()
	originName := seedReceiverFreshness(t, f, map[string]int64{
		"offsite-a": 12,
		"offsite-b": 5,
	})

	v, err := f.initStore.CreateVolume(ctx, f.initVol.Name, f.initVol.Path)
	if err != nil {
		t.Fatalf("CreateVolume on initiator: %v", err)
	}
	origin, err := f.initStore.GetOrCreateOriginNode(ctx, originName)
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode: %v", err)
	}
	// A higher local floor on offsite-b: the stale pull must not lower it.
	if err := f.initStore.MergeDestinationPushFreshness(ctx, v.ID, "offsite-b", origin.ID, 8); err != nil {
		t.Fatalf("seed local freshness floor: %v", err)
	}

	if _, err := PullDurability(ctx, f.initStore, f.initVol, f.node, false); err != nil {
		t.Fatalf("PullDurability: %v", err)
	}

	for dest, want := range map[string]int64{"offsite-a": 12, "offsite-b": 8} {
		fresh, err := f.initStore.ListDestinationPushFreshness(ctx, v.ID, dest)
		if err != nil {
			t.Fatalf("ListDestinationPushFreshness %s: %v", dest, err)
		}
		if len(fresh) != 1 || fresh[0].OriginRunID != want {
			t.Fatalf("%s freshness = %+v, want one coordinate at %d", dest, fresh, want)
		}
	}
}

// TestPullDurabilityCopiesComponents: the pull fetches the peer's
// vector components and lands them in the LOCAL destination_run_ids
// under the same destination names, with origin node names mapped to
// local rows (created on first contact).
func TestPullDurabilityCopiesComponents(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()
	originName := seedReceiverDurability(t, f, map[string]int64{
		"offsite-a": 12,
		"offsite-b": 5,
	})

	v, err := f.initStore.CreateVolume(ctx, f.initVol.Name, f.initVol.Path)
	if err != nil {
		t.Fatalf("CreateVolume on initiator: %v", err)
	}
	rep, err := PullDurability(ctx, f.initStore, f.initVol, f.node, false)
	if err != nil {
		t.Fatalf("PullDurability: %v", err)
	}
	if rep.Fetched != 2 || rep.Applied != 2 || len(rep.Rewinds) != 0 {
		t.Fatalf("report = %+v, want fetched=2 applied=2 no rewinds", rep)
	}

	origin, err := f.initStore.GetNodeByName(ctx, originName)
	if err != nil {
		t.Fatalf("origin node %q was not created locally: %v", originName, err)
	}
	for dest, want := range map[string]int64{"offsite-a": 12, "offsite-b": 5} {
		got, err := f.initStore.GetDestinationRunID(ctx, v.ID, dest, origin.ID)
		if err != nil {
			t.Fatalf("GetDestinationRunID %s: %v", dest, err)
		}
		if got.OriginRunID != want {
			t.Fatalf("%s component = %d, want %d", dest, got.OriginRunID, want)
		}
	}
}

// TestPullDurabilityRefusesRewind: a peer component below the locally
// recorded value is refused and reported, leaving the local value in
// place; the allow-rewind opt-in accepts it.
func TestPullDurabilityRefusesRewind(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()
	originName := seedReceiverDurability(t, f, map[string]int64{
		"offsite-a": 12,
		"offsite-b": 5,
	})

	v, err := f.initStore.CreateVolume(ctx, f.initVol.Name, f.initVol.Path)
	if err != nil {
		t.Fatalf("CreateVolume on initiator: %v", err)
	}
	origin, err := f.initStore.GetOrCreateOriginNode(ctx, originName)
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode: %v", err)
	}
	if err := f.initStore.UpsertDestinationRunID(ctx, v.ID, "offsite-b", origin.ID, 9, false); err != nil {
		t.Fatalf("seed local floor: %v", err)
	}

	rep, err := PullDurability(ctx, f.initStore, f.initVol, f.node, false)
	if err != nil {
		t.Fatalf("PullDurability: %v", err)
	}
	if rep.Applied != 1 || len(rep.Rewinds) != 1 {
		t.Fatalf("report = %+v, want applied=1 rewinds=1", rep)
	}
	rw := rep.Rewinds[0]
	if rw.Destination != "offsite-b" || rw.OriginNode != originName || rw.Current != 9 || rw.Attempted != 5 {
		t.Fatalf("rewind = %+v, want offsite-b/%s 9→5 refused", rw, originName)
	}
	got, err := f.initStore.GetDestinationRunID(ctx, v.ID, "offsite-b", origin.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.OriginRunID != 9 {
		t.Fatalf("offsite-b component = %d after refused rewind, want 9", got.OriginRunID)
	}

	rep, err = PullDurability(ctx, f.initStore, f.initVol, f.node, true)
	if err != nil {
		t.Fatalf("PullDurability allowRewind: %v", err)
	}
	if rep.Applied != 2 || len(rep.Rewinds) != 0 {
		t.Fatalf("override report = %+v, want applied=2 no rewinds", rep)
	}
	got, _ = f.initStore.GetDestinationRunID(ctx, v.ID, "offsite-b", origin.ID)
	if got.OriginRunID != 5 {
		t.Fatalf("offsite-b component = %d after override, want 5", got.OriginRunID)
	}
}

// TestPullDurabilityRequiresLocalVolume: the pull lands rows under a
// local volume id, so a volume with no local index row fails fast with
// a pointer at `index` rather than inventing a row.
func TestPullDurabilityRequiresLocalVolume(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	seedReceiverDurability(t, f, map[string]int64{"offsite-a": 12})

	_, err := PullDurability(context.Background(), f.initStore, f.initVol, f.node, false)
	if err == nil || !strings.Contains(err.Error(), "no local index row") {
		t.Fatalf("err = %v, want the no-local-index-row guard", err)
	}
}

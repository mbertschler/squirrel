package sync

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/syncproto"
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
	f.initVol.OffloadRequires = []string{"offsite-a"}
	f.initVol.SyncTo = []string{"offsite-b"}
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
	f.initVol.OffloadRequires = []string{"offsite-a"}
	f.initVol.SyncTo = []string{"offsite-b"}
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

// TestPullDurabilityTagsSourcePeer: the pull tags every merged component
// with the asserting peer's local node id (the residual of #104), so the
// offload gate can weigh peer-asserted evidence as a distinct, revocable
// class. Locally-verified components written here directly stay untagged.
func TestPullDurabilityTagsSourcePeer(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()
	f.initVol.OffloadRequires = []string{"offsite-a"}
	f.initVol.SyncTo = nil
	originName := seedReceiverDurability(t, f, map[string]int64{"offsite-a": 12})

	v, err := f.initStore.CreateVolume(ctx, f.initVol.Name, f.initVol.Path)
	if err != nil {
		t.Fatalf("CreateVolume on initiator: %v", err)
	}
	self, err := f.initStore.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	if err := f.initStore.UpsertDestinationRunIDVerified(ctx, v.ID, "offsite-a", self.ID, 4, store.VerifyMethodBlake3, false); err != nil {
		t.Fatalf("seed local verified component: %v", err)
	}

	if _, err := PullDurability(ctx, f.initStore, f.initVol, f.node, false); err != nil {
		t.Fatalf("PullDurability: %v", err)
	}

	peer, err := f.initStore.GetNodeByName(ctx, f.node.Name)
	if err != nil {
		t.Fatalf("source peer %q not resolved locally: %v", f.node.Name, err)
	}
	origin, err := f.initStore.GetNodeByName(ctx, originName)
	if err != nil {
		t.Fatalf("origin node %q not created locally: %v", originName, err)
	}

	pulled, err := f.initStore.GetDestinationRunID(ctx, v.ID, "offsite-a", origin.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID(pulled): %v", err)
	}
	if !pulled.SourceNodeID.Valid || pulled.SourceNodeID.Int64 != peer.ID {
		t.Fatalf("pulled component source = %+v, want peer %q (id %d)", pulled.SourceNodeID, f.node.Name, peer.ID)
	}
	local, err := f.initStore.GetDestinationRunID(ctx, v.ID, "offsite-a", self.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID(local): %v", err)
	}
	if local.SourceNodeID.Valid {
		t.Fatalf("locally-verified component tagged with source %d, want NULL", local.SourceNodeID.Int64)
	}
}

// TestPullDurabilityDropsUnconfiguredDestinations: the pull merges
// components for destinations the volume references (one via
// offload_requires, one via sync_to) and drops one for an unconfigured
// destination — counted and reported, never stored, and without
// aborting the merge of the legitimate components.
func TestPullDurabilityDropsUnconfiguredDestinations(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()
	f.initVol.OffloadRequires = []string{"offload-target"}
	f.initVol.SyncTo = []string{"sync-target"}
	originName := seedReceiverDurability(t, f, map[string]int64{
		"offload-target": 12,
		"sync-target":    5,
		"junk":           99,
	})

	v, err := f.initStore.CreateVolume(ctx, f.initVol.Name, f.initVol.Path)
	if err != nil {
		t.Fatalf("CreateVolume on initiator: %v", err)
	}
	rep, err := PullDurability(ctx, f.initStore, f.initVol, f.node, false)
	if err != nil {
		t.Fatalf("PullDurability: %v", err)
	}
	if rep.Fetched != 3 || rep.Applied != 2 || rep.Dropped != 1 || len(rep.Rewinds) != 0 {
		t.Fatalf("report = %+v, want fetched=3 applied=2 dropped=1 no rewinds", rep)
	}
	if len(rep.Drops) != 1 || rep.Drops[0].Destination != "junk" || rep.Drops[0].Kind != "component" {
		t.Fatalf("drops = %+v, want one component drop for junk", rep.Drops)
	}

	origin, err := f.initStore.GetNodeByName(ctx, originName)
	if err != nil {
		t.Fatalf("origin node %q was not created locally: %v", originName, err)
	}
	for dest, want := range map[string]int64{"offload-target": 12, "sync-target": 5} {
		got, err := f.initStore.GetDestinationRunID(ctx, v.ID, dest, origin.ID)
		if err != nil {
			t.Fatalf("GetDestinationRunID %s: %v", dest, err)
		}
		if got.OriginRunID != want {
			t.Fatalf("%s component = %d, want %d", dest, got.OriginRunID, want)
		}
	}
	if _, err := f.initStore.GetDestinationRunID(ctx, v.ID, "junk", origin.ID); err == nil {
		t.Fatal("junk component was stored, want it dropped")
	}
}

// TestPullDurabilityDropsUnconfiguredFreshness: a freshness coordinate
// for a destination outside the volume's accepted set is dropped and
// counted (Kind "freshness") just like a stray vector component, so a
// peer can't seed push-freshness for a destination this node never uses.
func TestPullDurabilityDropsUnconfiguredFreshness(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()
	f.initVol.OffloadRequires = []string{"offsite-a"}
	f.initVol.SyncTo = nil
	seedReceiverFreshness(t, f, map[string]int64{
		"offsite-a": 12,
		"junk":      99,
	})

	if _, err := f.initStore.CreateVolume(ctx, f.initVol.Name, f.initVol.Path); err != nil {
		t.Fatalf("CreateVolume on initiator: %v", err)
	}
	rep, err := PullDurability(ctx, f.initStore, f.initVol, f.node, false)
	if err != nil {
		t.Fatalf("PullDurability: %v", err)
	}
	if rep.Dropped != 1 || len(rep.Drops) != 1 {
		t.Fatalf("report = %+v, want exactly one drop", rep)
	}
	if rep.Drops[0].Destination != "junk" || rep.Drops[0].Kind != "freshness" {
		t.Fatalf("drop = %+v, want a freshness drop for junk", rep.Drops[0])
	}
	if rep.Fetched < 1 || rep.Applied < 1 {
		t.Fatalf("report = %+v, want the accepted offsite-a freshness still applied and counted", rep)
	}
}

// TestPullDurabilityCapsDropSamples: a peer flooding many out-of-scope
// destinations keeps the exact Dropped count but bounds the sampled
// Drops slice, so neither the report nor the output it feeds can grow
// unbounded under an adversarial peer.
func TestPullDurabilityCapsDropSamples(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()
	junk := make(map[string]int64, 50)
	for i := range 50 {
		junk[fmt.Sprintf("junk-%02d", i)] = 1
	}
	seedReceiverDurability(t, f, junk)

	if _, err := f.initStore.CreateVolume(ctx, f.initVol.Name, f.initVol.Path); err != nil {
		t.Fatalf("CreateVolume on initiator: %v", err)
	}
	rep, err := PullDurability(ctx, f.initStore, f.initVol, f.node, false)
	if err != nil {
		t.Fatalf("PullDurability: %v", err)
	}
	if rep.Fetched != 50 || rep.Applied != 0 || rep.Dropped != 50 {
		t.Fatalf("report = fetched=%d applied=%d dropped=%d, want 50/0/50", rep.Fetched, rep.Applied, rep.Dropped)
	}
	if len(rep.Drops) > 16 {
		t.Fatalf("len(Drops) = %d, want capped at 16", len(rep.Drops))
	}
}

// TestPullDurabilityRefusesRewind: a peer component below the locally
// recorded value is refused and reported, leaving the local value in
// place; the allow-rewind opt-in accepts it.
func TestPullDurabilityRefusesRewind(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()
	f.initVol.OffloadRequires = []string{"offsite-a"}
	f.initVol.SyncTo = []string{"offsite-b"}
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

// TestValidateComponentVerifyMethod: the wire-boundary guard accepts the
// empty method (a legitimate "unverified" state) and every method this
// build defines, and refuses an unrecognised non-empty method so a peer
// bug or version-skew string is loud at the pull rather than a
// silently-inert local row (SAFETY-AUDIT.md D1). Freshness carries no
// method and is unaffected.
func TestValidateComponentVerifyMethod(t *testing.T) {
	base := syncproto.DurabilityComponent{Destination: "offsite-a", OriginNode: "laptop", OriginRun: 5}
	for _, method := range []string{
		"",
		store.VerifyMethodBlake3,
		store.VerifyMethodSizeMtime,
		store.VerifyMethodPeer,
		store.VerifyMethodKopia,
		store.VerifyMethodPresenceSize,
	} {
		c := base
		c.VerifyMethod = method
		if err := validateComponent(c); err != nil {
			t.Errorf("validateComponent(method=%q) = %v, want nil", method, err)
		}
	}
	c := base
	c.VerifyMethod = "totally-bogus"
	if err := validateComponent(c); err == nil || !strings.Contains(err.Error(), "recognised verification method") {
		t.Fatalf("validateComponent(unknown method) = %v, want the unrecognised-method refusal", err)
	}
}

// TestPullDurabilityCapsOriginNodeCreation: a pull that names more than
// maxOriginNodesPerPull distinct origins is refused before it grows the
// local nodes table without bound (SAFETY-AUDIT.md D1). Seeds cap+1
// distinct-origin components on the receiver, all on one accepted
// destination, and asserts the pull fails with the cap message.
func TestPullDurabilityCapsOriginNodeCreation(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()
	f.initVol.OffloadRequires = []string{"offsite-a"}

	rv, err := f.recvStore.CreateVolume(ctx, f.recvVol.Name, f.recvVol.Path)
	if err != nil {
		t.Fatalf("CreateVolume on receiver: %v", err)
	}
	for i := 0; i <= maxOriginNodesPerPull; i++ {
		origin, err := f.recvStore.GetOrCreateOriginNode(ctx, fmt.Sprintf("origin-%04d", i))
		if err != nil {
			t.Fatalf("seed origin %d: %v", i, err)
		}
		if err := f.recvStore.UpsertDestinationRunID(ctx, rv.ID, "offsite-a", origin.ID, int64(i+1), false); err != nil {
			t.Fatalf("seed component %d: %v", i, err)
		}
	}

	if _, err := f.initStore.CreateVolume(ctx, f.initVol.Name, f.initVol.Path); err != nil {
		t.Fatalf("CreateVolume on initiator: %v", err)
	}
	_, err = PullDurability(ctx, f.initStore, f.initVol, f.node, false)
	if err == nil || !strings.Contains(err.Error(), "distinct origin nodes") {
		t.Fatalf("PullDurability = %v, want the origin-node cap refusal", err)
	}
}

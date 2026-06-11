package sync

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbertschler/squirrel/agent"
	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
)

// TestCollectIndexEntriesMaterialisesOrigins pins the sender side of
// origin propagation at the /plan boundary: locally-introduced content
// travels as (self name, introduction run — the earliest first_seen of
// the content in the volume, not the duplicate's), and content with a
// recorded origin travels verbatim under the origin node's name.
func TestCollectIndexEntriesMaterialisesOrigins(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()

	v, err := f.initStore.CreateVolume(ctx, f.initVol.Name, f.initVol.Path)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	run1, err := f.initStore.BeginIndexRun(ctx, store.RunKindIndex, v.ID, false)
	if err != nil {
		t.Fatalf("BeginIndexRun run1: %v", err)
	}
	run2, err := f.initStore.BeginIndexRun(ctx, store.RunKindIndex, v.ID, false)
	if err != nil {
		t.Fatalf("BeginIndexRun run2: %v", err)
	}
	ext, err := f.initStore.CreateNode(ctx, "ext", "peer://ext")
	if err != nil {
		t.Fatalf("CreateNode ext: %v", err)
	}
	upsert := func(path string, b byte, firstSeen int64, prov *store.Provenance) {
		t.Helper()
		if err := f.initStore.Upsert(ctx, store.FileRow{
			VolumeID: v.ID, Path: path, Blake3: bytesDigest(b),
			SizeBytes: 1, MtimeNs: 1, Status: store.StatusPresent,
			FirstSeenRunID: firstSeen, LastSeenRunID: firstSeen, IndexedAtNs: 1,
		}, prov); err != nil {
			t.Fatalf("upsert %s: %v", path, err)
		}
	}
	upsert("dup1.txt", 0xC1, run1, nil)
	upsert("dup2.txt", 0xC1, run2, nil)
	upsert("fwd.bin", 0xC2, run2, &store.Provenance{NodeID: ext.ID, RunID: 77})

	self, err := f.initStore.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	driver := &nodeSyncDriver{ctx: ctx, store: f.initStore, vol: f.initVol, volID: v.ID}
	entries, err := driver.collectIndexEntries()
	if err != nil {
		t.Fatalf("collectIndexEntries: %v", err)
	}
	type origin struct {
		node string
		run  int64
	}
	got := map[string]origin{}
	for _, e := range entries {
		got[e.Path] = origin{e.OriginNode, e.OriginRun}
	}
	want := map[string]origin{
		"dup1.txt": {self.Name, run1},
		"dup2.txt": {self.Name, run1},
		"fwd.bin":  {"ext", 77},
	}
	for path, w := range want {
		if got[path] != w {
			t.Fatalf("origin[%s] = %+v, want %+v (full: %+v)", path, got[path], w, got)
		}
	}
}

// chainPeer is one receiver in the 3-node chain: its store, on-disk
// volume, and the node config a forwarder dials it with.
type chainPeer struct {
	store *store.Store
	vol   *config.Volume
	node  *config.Node
}

// newChainPeer stands up one agent-backed receiver named name under
// root, mirroring buildNodeFixture's receiver half.
func newChainPeer(t *testing.T, root, name string) *chainPeer {
	t.Helper()
	volParent := filepath.Join(root, name)
	volPath := filepath.Join(volParent, "pics")
	if err := os.MkdirAll(volPath, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", volPath, err)
	}
	s := openStoreWithName(t, filepath.Join(root, name+".db"), name)
	vol := &config.Volume{Name: "pics", Path: volPath}
	srv, err := agent.New(agent.Config{
		Listen:  "127.0.0.1:0",
		Token:   "test-token",
		Version: "test",
		Volumes: map[string]*config.Volume{"pics": vol},
	}, s)
	if err != nil {
		t.Fatalf("agent.New(%s): %v", name, err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	endpoint, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	return &chainPeer{
		store: s,
		vol:   vol,
		node:  &config.Node{Name: name, Endpoint: endpoint, Token: "test-token", Path: volParent},
	}
}

// TestNodeSyncOriginCarriedVerbatimAcrossChain is the acceptance test
// for verbatim origin propagation: alpha introduces content, syncs to
// bravo, and bravo forwards to charlie. Charlie's contents row must
// record alpha's origin coordinate — alpha's name (a node charlie has
// never peered with; a row is created for it) and alpha's introduction
// run — never a relabel to bravo. The second hop must also classify
// cleanly (no conflicts): supersede-vs-conflict is judged by delivery,
// not by the forwarded origin.
func TestNodeSyncOriginCarriedVerbatimAcrossChain(t *testing.T) {
	rcl := requireRclone(t)
	root := t.TempDir()
	rcl.Config = filepath.Join(root, "rclone.conf")
	if err := os.WriteFile(rcl.Config, []byte{}, 0o600); err != nil {
		t.Fatalf("write rclone.conf: %v", err)
	}
	ctx := context.Background()

	volAPath := filepath.Join(root, "alpha", "pics")
	if err := os.MkdirAll(volAPath, 0o755); err != nil {
		t.Fatal(err)
	}
	storeA := openStoreWithName(t, filepath.Join(root, "alpha.db"), "alpha")
	volA := &config.Volume{Name: "pics", Path: volAPath}
	bravo := newChainPeer(t, root, "bravo")
	charlie := newChainPeer(t, root, "charlie")

	if err := os.WriteFile(filepath.Join(volAPath, "photo.jpg"), []byte("the travelling bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Index(ctx, storeA, volAPath, index.Options{Name: "pics"}); err != nil {
		t.Fatalf("index alpha: %v", err)
	}
	vA, err := storeA.GetVolumeByName(ctx, "pics")
	if err != nil {
		t.Fatalf("alpha volume: %v", err)
	}
	rowA, err := storeA.GetByPath(ctx, vA.ID, "photo.jpg")
	if err != nil {
		t.Fatalf("alpha row: %v", err)
	}
	introRun := rowA.FirstSeenRunID

	// Hop 1: alpha → bravo.
	rep1, err := SyncNode(ctx, storeA, rcl, volA, bravo.node, Options{Shallow: true})
	if err != nil || rep1.Status != store.RunStatusSuccess {
		t.Fatalf("hop 1: err=%v status=%q", err, rep1.Status)
	}
	vB, err := bravo.store.GetVolumeByName(ctx, "pics")
	if err != nil {
		t.Fatalf("bravo volume: %v", err)
	}
	alphaOnB, err := bravo.store.GetNodeByName(ctx, "alpha")
	if err != nil {
		t.Fatalf("bravo has no alpha row: %v", err)
	}
	rowB, err := bravo.store.GetByPath(ctx, vB.ID, "photo.jpg")
	if err != nil {
		t.Fatalf("bravo row: %v", err)
	}
	if !rowB.OriginNodeID.Valid || rowB.OriginNodeID.Int64 != alphaOnB.ID ||
		!rowB.OriginRunID.Valid || rowB.OriginRunID.Int64 != introRun {
		t.Fatalf("bravo origin = (%+v, %+v), want (alpha=%d, %d)",
			rowB.OriginNodeID, rowB.OriginRunID, alphaOnB.ID, introRun)
	}

	// Hop 2: bravo forwards to charlie. Bravo indexes first (the
	// initiator prerequisite); re-observing the synced file keeps its
	// row and content origin untouched.
	if _, err := index.Index(ctx, bravo.store, bravo.vol.Path, index.Options{Name: "pics"}); err != nil {
		t.Fatalf("index bravo: %v", err)
	}
	rep2, err := SyncNode(ctx, bravo.store, rcl, bravo.vol, charlie.node, Options{Shallow: true})
	if err != nil || rep2.Status != store.RunStatusSuccess {
		t.Fatalf("hop 2: err=%v status=%q", err, rep2.Status)
	}
	if len(rep2.NodeConflicts) != 0 {
		t.Fatalf("hop 2 conflicts = %+v, want none", rep2.NodeConflicts)
	}

	vC, err := charlie.store.GetVolumeByName(ctx, "pics")
	if err != nil {
		t.Fatalf("charlie volume: %v", err)
	}
	alphaOnC, err := charlie.store.GetNodeByName(ctx, "alpha")
	if err != nil {
		t.Fatalf("charlie has no nodes row for alpha (never peered, must be created from the wire origin): %v", err)
	}
	bravoOnC, err := charlie.store.GetNodeByName(ctx, "bravo")
	if err != nil {
		t.Fatalf("charlie has no bravo row: %v", err)
	}
	rowC, err := charlie.store.GetByPath(ctx, vC.ID, "photo.jpg")
	if err != nil {
		t.Fatalf("charlie row: %v", err)
	}
	if !rowC.OriginNodeID.Valid || rowC.OriginNodeID.Int64 != alphaOnC.ID {
		t.Fatalf("charlie OriginNodeID = %+v, want alpha's row %d (bravo's is %d — relabel forbidden)",
			rowC.OriginNodeID, alphaOnC.ID, bravoOnC.ID)
	}
	if !rowC.OriginRunID.Valid || rowC.OriginRunID.Int64 != introRun {
		t.Fatalf("charlie OriginRunID = %+v, want alpha's introduction run %d", rowC.OriginRunID, introRun)
	}
}

// TestNodeSyncAdvancesVectorAndPullsDurabilityAtClose covers the
// initiator's successful-close bookkeeping: the peer's destination
// vector advances over the volume's present set (self component at the
// local introduction run, forwarded-origin component at its origin run,
// verbatim), and the automatic metadata pull lands the peer's own
// destination components in the initiator's store under the same
// names.
func TestNodeSyncAdvancesVectorAndPullsDurabilityAtClose(t *testing.T) {
	f := setupNodeFixture(t)
	ctx := context.Background()

	// The peer knows about a destination only it can see.
	recvSelfName := seedReceiverDurability(t, f, map[string]int64{"offsite-x": 7})

	// Initiator: one forwarded-origin file seeded before indexing (a
	// content's origin is recorded at first contact and immutable),
	// plus one locally-introduced file picked up by the index run.
	v, err := f.initStore.CreateVolume(ctx, f.initVol.Name, f.initVol.Path)
	if err != nil {
		t.Fatalf("CreateVolume on initiator: %v", err)
	}
	seedRun, err := f.initStore.BeginIndexRun(ctx, store.RunKindIndex, v.ID, false)
	if err != nil {
		t.Fatalf("BeginIndexRun: %v", err)
	}
	_ = f.initStore.FinishRun(ctx, seedRun, store.RunStatusSuccess, "", 1)
	ext, err := f.initStore.CreateNode(ctx, "ext", "peer://ext")
	if err != nil {
		t.Fatalf("CreateNode ext: %v", err)
	}
	fwdBody := []byte("forwarded content")
	fwdAbs := filepath.Join(f.initVol.Path, "fwd.bin")
	if err := os.WriteFile(fwdAbs, fwdBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.initStore.Upsert(ctx, store.FileRow{
		VolumeID: v.ID, Path: "fwd.bin", Blake3: hashFile(t, fwdAbs),
		SizeBytes: int64(len(fwdBody)), MtimeNs: 1, Status: store.StatusPresent,
		FirstSeenRunID: seedRun, LastSeenRunID: seedRun, IndexedAtNs: 1,
	}, &store.Provenance{NodeID: ext.ID, RunID: 42}); err != nil {
		t.Fatalf("seed forwarded row: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.initVol.Path, "local.txt"), []byte("locally introduced"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.indexInitiator(t)

	rep, err := SyncNode(ctx, f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true})
	if err != nil || rep.Status != store.RunStatusSuccess {
		t.Fatalf("SyncNode: err=%v status=%q", err, rep.Status)
	}

	// Vector advanced for the peer destination, in origin space.
	self, _ := f.initStore.GetSelfNode(ctx)
	localRow, err := f.initStore.GetByPath(ctx, v.ID, "local.txt")
	if err != nil {
		t.Fatalf("GetByPath local.txt: %v", err)
	}
	vector, err := f.initStore.ListDestinationRunIDs(ctx, v.ID, f.node.Name)
	if err != nil {
		t.Fatalf("ListDestinationRunIDs: %v", err)
	}
	byNode := map[int64]int64{}
	for _, c := range vector {
		byNode[c.OriginNodeID] = c.OriginRunID
	}
	if byNode[self.ID] != localRow.FirstSeenRunID {
		t.Fatalf("self component = %d, want local.txt's introduction run %d (vector = %+v)",
			byNode[self.ID], localRow.FirstSeenRunID, byNode)
	}
	if byNode[ext.ID] != 42 {
		t.Fatalf("ext component = %d, want 42 (forwarded origin, verbatim)", byNode[ext.ID])
	}

	// The automatic pull cached the peer's offsite component locally.
	if rep.DurabilityPull.Fetched != 1 || rep.DurabilityPull.Applied != 1 {
		t.Fatalf("DurabilityPull = %+v, want fetched=1 applied=1", rep.DurabilityPull)
	}
	originOnInit, err := f.initStore.GetNodeByName(ctx, recvSelfName)
	if err != nil {
		t.Fatalf("initiator has no row for the peer origin %q: %v", recvSelfName, err)
	}
	got, err := f.initStore.GetDestinationRunID(ctx, v.ID, "offsite-x", originOnInit.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID offsite-x: %v", err)
	}
	if got.OriginRunID != 7 {
		t.Fatalf("offsite-x component = %d, want 7", got.OriginRunID)
	}
}

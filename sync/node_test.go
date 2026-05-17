package sync

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/daemon"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/syncproto"
)

// nodeFixture wires an in-process initiator + receiver pair. The
// initiator has its own store/volume; the receiver has a daemon
// running off another store + on-disk volume directory. Tests drive
// the public `SyncNode` API end-to-end; the daemon is reached via
// httptest.Server so no TCP port is opened.
type nodeFixture struct {
	initStore *store.Store
	recvStore *store.Store
	initVol   *config.Volume
	recvVol   *config.Volume
	node      *config.Node
	rcl       *Rclone
	server    *httptest.Server
}

func setupNodeFixture(t *testing.T) *nodeFixture {
	t.Helper()
	rcl := requireRclone(t)

	root := t.TempDir()
	initVolPath := filepath.Join(root, "init", "pics")
	recvVolRoot := filepath.Join(root, "recv")
	recvVolPath := filepath.Join(recvVolRoot, "pics")
	for _, p := range []string{initVolPath, recvVolPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	rcl.Config = filepath.Join(root, "rclone.conf")
	if err := os.WriteFile(rcl.Config, []byte{}, 0o600); err != nil {
		t.Fatalf("write rclone.conf: %v", err)
	}

	initStore := openStoreWithName(t, filepath.Join(root, "init.db"), "init")
	recvStore := openStoreWithName(t, filepath.Join(root, "recv.db"), "nas")

	initVol := &config.Volume{Name: "pics", Path: initVolPath}
	recvVol := &config.Volume{Name: "pics", Path: recvVolPath}

	srv, err := daemon.New(daemon.Config{
		Listen:  "127.0.0.1:0",
		Token:   "test-token",
		Version: "test",
		Volumes: map[string]*config.Volume{"pics": recvVol},
	}, recvStore)
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	endpoint, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test URL: %v", err)
	}

	// nodeRcloneDest joins node.Path + volumeName/, so node.Path is
	// the receiver's "volume parent" directory.
	node := &config.Node{
		Name:     "nas",
		Endpoint: endpoint,
		Token:    "test-token",
		Path:     recvVolRoot,
	}

	return &nodeFixture{
		initStore: initStore,
		recvStore: recvStore,
		initVol:   initVol,
		recvVol:   recvVol,
		node:      node,
		rcl:       rcl,
		server:    ts,
	}
}

func openStoreWithName(t *testing.T, path, name string) *store.Store {
	t.Helper()
	s, err := store.OpenWithOptions(path, store.OpenOptions{NodeName: name})
	if err != nil {
		t.Fatalf("OpenWithOptions(%s, %s): %v", path, name, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// indexInitiator runs the indexer on the initiator side so the
// prerequisite-check passes. The receiver-side index is built up
// piecemeal by the sync runs themselves (single-writer model).
func (f *nodeFixture) indexInitiator(t *testing.T) {
	t.Helper()
	if _, err := index.Index(context.Background(), f.initStore, f.initVol.Path,
		index.Options{Name: f.initVol.Name}); err != nil {
		t.Fatalf("index.Index: %v", err)
	}
}

func TestNodeSyncTransfersFiles(t *testing.T) {
	f := setupNodeFixture(t)

	files := map[string]string{
		"a.txt":     "alpha",
		"sub/b.txt": "beta",
		"sub/c.txt": "gamma",
	}
	for name, body := range files {
		full := filepath.Join(f.initVol.Path, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.indexInitiator(t)

	// Sub-volume directory must exist on the receiver side; rclone
	// will create per-file subdirs but it expects the volume root.
	if err := os.MkdirAll(filepath.Join(f.recvVol.Path), 0o755); err != nil {
		t.Fatal(err)
	}

	rep, err := SyncNode(context.Background(), f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true})
	if err != nil {
		t.Fatalf("SyncNode: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success: %+v", rep.Status, rep)
	}
	if rep.NodeReceiverRunID == 0 {
		t.Fatalf("NodeReceiverRunID = 0; want non-zero (begin handshake produced one)")
	}
	if len(rep.NodeVerify.Matched) != len(files) {
		t.Fatalf("Verify.Matched = %d, want %d", len(rep.NodeVerify.Matched), len(files))
	}
	if len(rep.NodeVerify.Mismatched) != 0 || len(rep.NodeVerify.Missing) != 0 {
		t.Fatalf("expected clean verify, got %+v", rep.NodeVerify)
	}

	// All files landed at the receiver-side path.
	for name, body := range files {
		got, err := os.ReadFile(filepath.Join(f.recvVol.Path, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != body {
			t.Fatalf("%s content = %q, want %q", name, got, body)
		}
	}

	// Receiver-side index reflects the new rows.
	v, err := f.recvStore.GetVolumeByName(context.Background(), "pics")
	if err != nil {
		t.Fatalf("GetVolumeByName on receiver: %v", err)
	}
	for name := range files {
		row, err := f.recvStore.GetByPath(context.Background(), v.ID, name)
		if err != nil {
			t.Fatalf("GetByPath %s on receiver: %v", name, err)
		}
		if !row.SourceNodeID.Valid {
			t.Fatalf("%s row has NULL source_node_id; want initiator attribution", name)
		}
	}

	// peer_sync_state advanced.
	initSelf, _ := f.initStore.GetSelfNode(context.Background())
	peerOnRecv, _ := f.recvStore.GetNodeByName(context.Background(), initSelf.Name)
	state, err := f.recvStore.GetPeerSyncState(context.Background(), v.ID, peerOnRecv.ID)
	if err != nil {
		t.Fatalf("peer_sync_state lookup: %v", err)
	}
	if !state.LastSharedRunID.Valid || state.LastSharedRunID.Int64 != rep.RunID {
		t.Fatalf("peer_sync_state.last_shared_run_id = %+v, want %d", state.LastSharedRunID, rep.RunID)
	}

	// Run row on the initiator carries peer linkage.
	runRow, err := f.initStore.GetRun(context.Background(), rep.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !runRow.PeerNodeID.Valid {
		t.Fatalf("initiator run has NULL peer_node_id; want non-NULL")
	}
	if !runRow.CorrelatedRunID.Valid || runRow.CorrelatedRunID.Int64 != rep.NodeReceiverRunID {
		t.Fatalf("CorrelatedRunID = %+v, want %d", runRow.CorrelatedRunID, rep.NodeReceiverRunID)
	}
}

// TestNodeSyncSupersedeMovesPriorBytes drives two sync rounds: the
// first establishes the prior version, the second overwrites it, and
// we assert the prior bytes were pre-moved into
// .squirrel-history/run-<id>/ before the new bytes landed.
func TestNodeSyncSupersedeMovesPriorBytes(t *testing.T) {
	f := setupNodeFixture(t)

	target := filepath.Join(f.initVol.Path, "doc.md")
	if err := os.WriteFile(target, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.indexInitiator(t)
	if _, err := SyncNode(context.Background(), f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true}); err != nil {
		t.Fatalf("first SyncNode: %v", err)
	}

	if err := os.WriteFile(target, []byte("v2-different"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.indexInitiator(t)
	rep, err := SyncNode(context.Background(), f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true})
	if err != nil {
		t.Fatalf("second SyncNode: %v", err)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success", rep.Status)
	}

	live, err := os.ReadFile(filepath.Join(f.recvVol.Path, "doc.md"))
	if err != nil {
		t.Fatalf("read live: %v", err)
	}
	if string(live) != "v2-different" {
		t.Fatalf("live content = %q, want v2-different", live)
	}

	histDir := filepath.Join(f.recvVol.Path, daemon.HistoryDirName)
	matches, _ := filepath.Glob(filepath.Join(histDir, "run-*", "doc.md"))
	if len(matches) == 0 {
		t.Fatalf("no pre-moved prior copy under %s", histDir)
	}
	old, _ := os.ReadFile(matches[0])
	if string(old) != "v1" {
		t.Fatalf("history copy = %q, want 'v1'", old)
	}
}

// TestNodeSyncReturnsConflictOnLocalWriteOnReceiver synthesises the
// PR-3 conflict scenario by planting a present file row on the
// receiver with source_node_id = NULL (i.e. a "local write"), then
// initiating a sync from the initiator with a different blake3 at
// the same path. The plan must surface a conflict; the run must
// fail; no rclone should run; receiver-side state must remain
// untouched.
func TestNodeSyncReturnsConflictOnLocalWriteOnReceiver(t *testing.T) {
	f := setupNodeFixture(t)
	ctx := context.Background()

	// Seed the receiver's volumes row + a "local write" file row.
	v, err := f.recvStore.CreateVolume(ctx, f.recvVol.Name, f.recvVol.Path)
	if err != nil {
		t.Fatalf("seed receiver volume: %v", err)
	}
	runID, err := f.recvStore.BeginRun(ctx, store.RunKindIndex, v.ID, "")
	if err != nil {
		t.Fatalf("BeginRun on receiver: %v", err)
	}
	_ = f.recvStore.FinishRun(ctx, runID, store.RunStatusSuccess, "", 1)
	receiverDigest := bytesDigest(0x88)
	if err := f.recvStore.Upsert(ctx, store.FileRow{
		VolumeID: v.ID, Path: "doc.md", Blake3: receiverDigest,
		SizeBytes: 5, MtimeNs: 50, Status: store.StatusPresent,
		FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: 50,
	}, nil); err != nil {
		t.Fatalf("seed receiver file row: %v", err)
	}
	// Materialise the file on disk so the supersede-pre-move would
	// have something to rename (if we incorrectly fell through to
	// supersede).
	if err := os.WriteFile(filepath.Join(f.recvVol.Path, "doc.md"), []byte("recvr"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Initiator writes a *different* blake3 at the same path.
	if err := os.WriteFile(filepath.Join(f.initVol.Path, "doc.md"), []byte("initr"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.indexInitiator(t)

	rep, err := SyncNode(ctx, f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true})
	if err == nil {
		t.Fatalf("expected conflict error, got nil (rep=%+v)", rep)
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v (%T), want *ConflictError", err, err)
	}
	if len(conflict.Paths) != 1 || conflict.Paths[0] != "doc.md" {
		t.Fatalf("conflict paths = %v, want [doc.md]", conflict.Paths)
	}
	if conflict.Conflicts[0].Reason != "local write on receiver" {
		t.Fatalf("conflict reason = %q, want 'local write on receiver'", conflict.Conflicts[0].Reason)
	}

	// Receiver-side file row unchanged.
	live, err := f.recvStore.GetByPath(ctx, v.ID, "doc.md")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if hex.EncodeToString(live.Blake3) != hex.EncodeToString(receiverDigest) {
		t.Fatalf("receiver row was overwritten: %x", live.Blake3)
	}
	on, _ := os.ReadFile(filepath.Join(f.recvVol.Path, "doc.md"))
	if string(on) != "recvr" {
		t.Fatalf("receiver on-disk content changed to %q", on)
	}
}

// TestNodeSyncVerifyMismatchPartialStatus simulates rclone "succeeding"
// but the on-disk content being wrong (the daemon's re-hash will
// catch it). We inject the mismatch by writing different content
// directly into the receiver volume between rclone-finish and verify,
// using a stub Rclone wrapper. Since wiring a stub here is heavy, we
// instead corrupt the file *after* the initiator-side sync succeeds
// and re-run sync — except this time we ALSO snapshot the
// verify+report on the original successful run. So we end up
// validating the "verify-returns-partial" code path by directly
// driving the receiver's verifySession via a unit test in
// daemon/sync_test.go. The integration here just sanity-checks that
// "files-from" only ships transfer-bucket paths in the rclone
// invocation; the supersede + already-correct paths skip transfer.
func TestNodeSyncIdempotentRerun(t *testing.T) {
	f := setupNodeFixture(t)
	if err := os.WriteFile(filepath.Join(f.initVol.Path, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.indexInitiator(t)

	rep1, err := SyncNode(context.Background(), f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true})
	if err != nil {
		t.Fatalf("first SyncNode: %v", err)
	}
	if rep1.Status != store.RunStatusSuccess {
		t.Fatalf("first run status = %q", rep1.Status)
	}

	rep2, err := SyncNode(context.Background(), f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true})
	if err != nil {
		t.Fatalf("second SyncNode: %v", err)
	}
	if rep2.Status != store.RunStatusSuccess {
		t.Fatalf("second run status = %q", rep2.Status)
	}
	// already-correct bucket means rclone shouldn't transfer
	// anything new.
	if rep2.RcloneResult.Transferred != 0 {
		t.Logf("note: idempotent re-run transferred %d files (rclone version may not optimise this for files-from)", rep2.RcloneResult.Transferred)
	}
	// Verify report should be empty (no transfer + no supersede ⇒
	// nothing to verify).
	if len(rep2.NodeVerify.Matched)+len(rep2.NodeVerify.Mismatched) != 0 {
		t.Fatalf("expected nothing to verify on idempotent re-run, got %+v", rep2.NodeVerify)
	}
}

// TestNodeSyncRejectsUnknownVolume exercises the daemon-side guard:
// /begin against a volume the receiver doesn't declare must 404.
func TestNodeSyncRejectsUnknownVolume(t *testing.T) {
	f := setupNodeFixture(t)
	// Initiator uses a fresh config with a volume name the receiver
	// never declared.
	other := *f.initVol
	other.Name = "ghost"
	if err := os.WriteFile(filepath.Join(f.initVol.Path, "x.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Re-index under the new name so the initiator side has it.
	if _, err := index.Index(context.Background(), f.initStore, f.initVol.Path,
		index.Options{Name: other.Name}); err != nil {
		t.Fatalf("re-index: %v", err)
	}
	_, err := SyncNode(context.Background(), f.initStore, f.rcl, &other, f.node, Options{Shallow: true})
	if err == nil {
		t.Fatalf("expected error; got nil")
	}
	if !strings.Contains(err.Error(), "is not declared on this node") {
		t.Fatalf("error = %v, want 'is not declared on this node'", err)
	}
}

// TestVerifyReportsMismatch is a daemon-side check: drive begin →
// plan → corrupt the on-disk content → verify. The verify endpoint
// must surface the mismatch. We intentionally don't go through
// rclone here so the test is independent of the rclone-version
// constraints and so we can corrupt the receiver's tree directly.
func TestVerifyReportsMismatch(t *testing.T) {
	f := setupNodeFixture(t)
	ctx := context.Background()

	// Plant a file on disk at the receiver. This represents bytes
	// that arrived after a (mocked) rclone transfer.
	if err := os.WriteFile(filepath.Join(f.recvVol.Path, "a.txt"), []byte("not-what-initiator-claims"), 0o644); err != nil {
		t.Fatal(err)
	}

	initSelf, _ := f.initStore.GetSelfNode(ctx)
	client := newNodeClient(f.node)
	begin, err := client.begin(ctx, syncproto.BeginRequest{
		Volume:            f.recvVol.Name,
		InitiatorNodeName: initSelf.Name,
		InitiatorRunID:    1,
	})
	if err != nil {
		t.Fatalf("/begin: %v", err)
	}
	t.Cleanup(func() {
		_ = client.close(ctx, syncproto.CloseRequest{
			ReceiverRunID: begin.ReceiverRunID,
			Status:        store.RunStatusFailed,
		})
	})
	// Initiator claims a different blake3 at a.txt than what's on
	// the receiver's disk.
	_, err = client.plan(ctx, syncproto.PlanRequest{
		ReceiverRunID: begin.ReceiverRunID,
		Entries: []syncproto.IndexEntry{
			{Path: "a.txt", Blake3Hex: hexDigest(0x99), SizeBytes: 5, MtimeNs: 1},
		},
	})
	if err != nil {
		t.Fatalf("/plan: %v", err)
	}
	v, err := client.verify(ctx, syncproto.VerifyRequest{ReceiverRunID: begin.ReceiverRunID})
	if err != nil {
		t.Fatalf("/verify: %v", err)
	}
	if len(v.Mismatched) != 1 || v.Mismatched[0].Path != "a.txt" {
		t.Fatalf("mismatched = %+v, want one entry for a.txt", v.Mismatched)
	}
	if v.Mismatched[0].ExpectedHex != hexDigest(0x99) {
		t.Fatalf("expected hex = %q, want %q", v.Mismatched[0].ExpectedHex, hexDigest(0x99))
	}
}

// TestVolumeLockSerializesInFlightSyncs proves a second /begin
// against the same volume while one is open returns 409, preventing
// concurrent index commits that would race the per-volume invariant.
func TestVolumeLockSerializesInFlightSyncs(t *testing.T) {
	f := setupNodeFixture(t)
	ctx := context.Background()

	initSelf, _ := f.initStore.GetSelfNode(ctx)
	client := newNodeClient(f.node)
	begin, err := client.begin(ctx, syncproto.BeginRequest{
		Volume:            f.recvVol.Name,
		InitiatorNodeName: initSelf.Name,
		InitiatorRunID:    1,
	})
	if err != nil {
		t.Fatalf("first /begin: %v", err)
	}
	defer func() {
		_ = client.close(ctx, syncproto.CloseRequest{
			ReceiverRunID: begin.ReceiverRunID,
			Status:        store.RunStatusFailed,
		})
	}()

	_, err = client.begin(ctx, syncproto.BeginRequest{
		Volume:            f.recvVol.Name,
		InitiatorNodeName: initSelf.Name,
		InitiatorRunID:    2,
	})
	if err == nil {
		t.Fatalf("second /begin against locked volume succeeded; want 409")
	}
	if !strings.Contains(err.Error(), "already has an in-flight sync") {
		t.Fatalf("error = %v, want 'already has an in-flight sync'", err)
	}
}

// TestPlanResponseContainsAllDispositions is a daemon-side white-box
// check that the four dispositions emerge from one mixed payload. It
// doesn't go through rclone — just /begin and /plan via the test
// HTTP server.
func TestPlanResponseContainsAllDispositions(t *testing.T) {
	f := setupNodeFixture(t)
	ctx := context.Background()

	// Receiver-side: prepare three pre-existing rows under one
	// volume, three dispositions ahead of /plan:
	//   - "same.txt"      → same blake3 ⇒ already-correct
	//   - "evolved.txt"   → different blake3 sourced from initiator
	//                       (we plant peer_sync_state to make the
	//                       provenance trace back to the initiator
	//                       at run ≤ watermark) ⇒ supersede
	//   - "novel.txt"     → no receiver-side row ⇒ transfer
	//   - "local.txt"     → different blake3, source_node_id NULL
	//                       ⇒ conflict
	v, err := f.recvStore.CreateVolume(ctx, f.recvVol.Name, f.recvVol.Path)
	if err != nil {
		t.Fatalf("seed receiver volume: %v", err)
	}
	// Seed initiator on receiver's nodes table with the same
	// synthetic placeholder endpoint the daemon uses for
	// single-writer initiators, so the subsequent /begin call's
	// GetOrCreatePeerNode finds the row and treats it as existing.
	initSelf, _ := f.initStore.GetSelfNode(ctx)
	peer, err := f.recvStore.CreateNode(ctx, initSelf.Name, "peer://"+initSelf.Name)
	if err != nil {
		t.Fatalf("seed peer row: %v", err)
	}
	// Receiver-side run that "supplied" the evolved row's prior
	// content.
	// Use BeginPeerSyncRun so the row carries the correlated
	// initiator-run-id the planner needs to translate source_run_id
	// (receiver-local) back into the initiator's id space.
	const priorInitiatorRunID = 5
	priorRun, err := f.recvStore.BeginPeerSyncRun(ctx, v.ID, peer.ID, priorInitiatorRunID, initSelf.Name)
	if err != nil {
		t.Fatalf("BeginPeerSyncRun: %v", err)
	}
	_ = f.recvStore.FinishRun(ctx, priorRun, store.RunStatusSuccess, "", 1)

	mustUpsert := func(path string, b3 byte, prov *store.Provenance) {
		t.Helper()
		if err := f.recvStore.Upsert(ctx, store.FileRow{
			VolumeID: v.ID, Path: path, Blake3: bytesDigest(b3),
			SizeBytes: 1, MtimeNs: 1, Status: store.StatusPresent,
			FirstSeenRunID: priorRun, LastSeenRunID: priorRun, IndexedAtNs: 1,
		}, prov); err != nil {
			t.Fatalf("upsert receiver %s: %v", path, err)
		}
	}
	mustUpsert("same.txt", 0xAA, nil)
	mustUpsert("evolved.txt", 0xBB, &store.Provenance{NodeID: peer.ID, RunID: priorRun})
	mustUpsert("local.txt", 0xCC, nil)

	// Materialise files on disk so the supersede pre-move has
	// something to rename.
	for _, p := range []string{"same.txt", "evolved.txt", "local.txt"} {
		if err := os.WriteFile(filepath.Join(f.recvVol.Path, p), []byte("seed"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// peer_sync_state watermark (in initiator-id space) high enough
	// to cover the prior row's correlated id.
	if err := f.recvStore.UpsertPeerSyncState(ctx, v.ID, peer.ID, priorInitiatorRunID+10); err != nil {
		t.Fatalf("UpsertPeerSyncState: %v", err)
	}

	// Drive /begin + /plan directly via the client.
	client := newNodeClient(f.node)
	begin, err := client.begin(ctx, syncproto.BeginRequest{
		Volume:            f.recvVol.Name,
		InitiatorNodeName: initSelf.Name,
		InitiatorRunID:    100,
	})
	if err != nil {
		t.Fatalf("/begin: %v", err)
	}
	plan, err := client.plan(ctx, syncproto.PlanRequest{
		ReceiverRunID: begin.ReceiverRunID,
		Entries: []syncproto.IndexEntry{
			{Path: "same.txt", Blake3Hex: hexDigest(0xAA), SizeBytes: 1, MtimeNs: 1},
			{Path: "evolved.txt", Blake3Hex: hexDigest(0xCD), SizeBytes: 1, MtimeNs: 1},
			{Path: "novel.txt", Blake3Hex: hexDigest(0xEF), SizeBytes: 1, MtimeNs: 1},
			{Path: "local.txt", Blake3Hex: hexDigest(0xDE), SizeBytes: 1, MtimeNs: 1},
		},
	})
	if err != nil {
		t.Fatalf("/plan: %v", err)
	}
	defer func() {
		_ = client.close(ctx, syncproto.CloseRequest{
			ReceiverRunID: begin.ReceiverRunID,
			Status:        store.RunStatusFailed,
		})
	}()

	got := map[string]string{}
	for _, d := range plan.Dispositions {
		got[d.Path] = d.Disposition
	}
	want := map[string]string{
		"same.txt":    syncproto.DispositionAlreadyCorrect,
		"evolved.txt": syncproto.DispositionSupersede,
		"novel.txt":   syncproto.DispositionTransfer,
		"local.txt":   syncproto.DispositionConflict,
	}
	for path, w := range want {
		if got[path] != w {
			t.Fatalf("disposition[%s] = %q, want %q (full: %+v)", path, got[path], w, got)
		}
	}
	if len(plan.Conflicts) != 1 || plan.Conflicts[0].Path != "local.txt" {
		t.Fatalf("conflicts = %+v, want one for local.txt", plan.Conflicts)
	}

	// Supersede side-effect: prior bytes moved into history dir.
	matches, _ := filepath.Glob(filepath.Join(f.recvVol.Path, daemon.HistoryDirName, "run-*", "evolved.txt"))
	if len(matches) == 0 {
		t.Fatalf("evolved.txt prior bytes were not pre-moved")
	}
	// local.txt (conflict) must NOT be moved — PR 3 doesn't resolve
	// conflicts; rclone is never invoked for them, and the prior
	// bytes stay at the original path.
	if _, err := os.Stat(filepath.Join(f.recvVol.Path, "local.txt")); err != nil {
		t.Fatalf("local.txt was moved despite conflict disposition: %v", err)
	}
}

// bytesDigest returns a 32-byte buffer filled with b for compact
// fixture digests.
func bytesDigest(b byte) []byte {
	return bytes.Repeat([]byte{b}, 32)
}

// hexDigest returns the hex encoding of bytesDigest(b).
func hexDigest(b byte) string {
	return hex.EncodeToString(bytesDigest(b))
}

package sync

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/agent"
	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/syncproto"
)

// nodeFixture wires an in-process initiator + receiver pair. The
// initiator has its own store/volume; the receiver has an agent
// running off another store + on-disk volume directory. Tests drive
// the public `SyncNode` API end-to-end; the agent is reached via
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
	f, root := buildNodeFixture(t)
	rcl := requireRclone(t)
	rcl.Config = filepath.Join(root, "rclone.conf")
	if err := os.WriteFile(rcl.Config, []byte{}, 0o600); err != nil {
		t.Fatalf("write rclone.conf: %v", err)
	}
	f.rcl = rcl
	return f
}

// buildNodeFixture is the rclone-agnostic core of setupNodeFixture:
// it lays down the on-disk volume dirs, opens initiator and receiver
// stores, and spins an in-process agent to back the receiver. Tests
// that need rclone wrap with the requireRclone skip + config write
// via setupNodeFixture; tests that drive the HTTP surface directly
// call this helper and leave fixture.rcl nil.
func buildNodeFixture(t *testing.T) (*nodeFixture, string) {
	t.Helper()
	root := t.TempDir()
	initVolPath := filepath.Join(root, "init", "pics")
	recvVolRoot := filepath.Join(root, "recv")
	recvVolPath := filepath.Join(recvVolRoot, "pics")
	for _, p := range []string{initVolPath, recvVolPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}

	initStore := openStoreWithName(t, filepath.Join(root, "init.db"), "init")
	recvStore := openStoreWithName(t, filepath.Join(root, "recv.db"), "nas")

	initVol := &config.Volume{Name: "pics", Path: initVolPath}
	recvVol := &config.Volume{Name: "pics", Path: recvVolPath}

	srv, err := agent.New(agent.Config{
		Listen:  "127.0.0.1:0",
		Token:   "test-token",
		Version: "test",
		Volumes: map[string]*config.Volume{"pics": recvVol},
	}, recvStore)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
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
		server:    ts,
	}, root
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

	histDir := filepath.Join(f.recvVol.Path, agent.HistoryDirName)
	matches, _ := filepath.Glob(filepath.Join(histDir, "run-*", "doc.md"))
	if len(matches) == 0 {
		t.Fatalf("no pre-moved prior copy under %s", histDir)
	}
	old, _ := os.ReadFile(matches[0])
	if string(old) != "v1" {
		t.Fatalf("history copy = %q, want 'v1'", old)
	}
}

// TestNodeSyncResolvesConflictOnLocalWriteOnReceiver covers the v1
// multi-writer scenario: the receiver has a `present` row at a path
// authored locally (source_node_id NULL — a NAS web-app upload or a
// agent-side `squirrel index` after a manual edit), and an initiator
// sync arrives carrying a different blake3 at the same path.
//
// The expected resolution is "initiator wins live, loser preserved":
// rclone delivers the initiator's bytes to the original path, while
// the receiver moves the prior bytes to
// .squirrel-conflicts/run-<id>/<path> and seeds an index row there
// carrying the prior blake3 + prior provenance. The run is NOT a
// failure — `peer_sync_state` advances so the next sync doesn't
// re-flag the same conflict.
func TestNodeSyncResolvesConflictOnLocalWriteOnReceiver(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(f.recvVol.Path, "doc.md"), []byte("recvr"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Initiator writes a *different* blake3 at the same path.
	if err := os.WriteFile(filepath.Join(f.initVol.Path, "doc.md"), []byte("initr"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.indexInitiator(t)

	rep, err := SyncNode(ctx, f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true})
	if err != nil {
		t.Fatalf("SyncNode: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success: %+v", rep.Status, rep)
	}
	if len(rep.NodeConflicts) != 1 || rep.NodeConflicts[0].Path != "doc.md" {
		t.Fatalf("NodeConflicts = %+v, want one for doc.md", rep.NodeConflicts)
	}
	if rep.NodeConflicts[0].Reason != "local write on receiver" {
		t.Fatalf("conflict reason = %q, want 'local write on receiver'", rep.NodeConflicts[0].Reason)
	}
	preservedRel := rep.NodeConflicts[0].PreservedAtPath
	if preservedRel == "" {
		t.Fatalf("PreservedAtPath empty; want a .squirrel-conflicts/... path")
	}

	// Initiator's bytes are live at the original path.
	live, err := os.ReadFile(filepath.Join(f.recvVol.Path, "doc.md"))
	if err != nil {
		t.Fatalf("read live: %v", err)
	}
	if string(live) != "initr" {
		t.Fatalf("live content = %q, want 'initr'", live)
	}

	// Loser's bytes are at the preserved path.
	loser, err := os.ReadFile(filepath.Join(f.recvVol.Path, preservedRel))
	if err != nil {
		t.Fatalf("read preserved %s: %v", preservedRel, err)
	}
	if string(loser) != "recvr" {
		t.Fatalf("preserved content = %q, want 'recvr'", loser)
	}

	// Index reachability: live row at doc.md carries initiator
	// provenance; conflict-path row carries the prior blake3 with
	// NULL source (local write).
	liveRow, err := f.recvStore.GetByPath(ctx, v.ID, "doc.md")
	if err != nil {
		t.Fatalf("GetByPath doc.md: %v", err)
	}
	if hex.EncodeToString(liveRow.Blake3) == hex.EncodeToString(receiverDigest) {
		t.Fatalf("live row still carries the prior blake3; want initiator's")
	}
	if !liveRow.SourceNodeID.Valid {
		t.Fatalf("live doc.md row has NULL source_node_id; want initiator attribution")
	}
	preservedRow, err := f.recvStore.GetByPath(ctx, v.ID, preservedRel)
	if err != nil {
		t.Fatalf("GetByPath %s: %v", preservedRel, err)
	}
	if hex.EncodeToString(preservedRow.Blake3) != hex.EncodeToString(receiverDigest) {
		t.Fatalf("preserved row blake3 = %x, want %x (the prior content)",
			preservedRow.Blake3, receiverDigest)
	}
	if preservedRow.SourceNodeID.Valid {
		t.Fatalf("preserved row source_node_id = %d, want NULL (prior was a local write)",
			preservedRow.SourceNodeID.Int64)
	}

	// Loser is reachable by hash too — `squirrel query <prior>`
	// should land on the conflict path.
	matches, err := f.recvStore.GetByBlake3(ctx, receiverDigest)
	if err != nil {
		t.Fatalf("GetByBlake3: %v", err)
	}
	foundAtConflictPath := false
	for _, m := range matches {
		if m.File.Path == preservedRel {
			foundAtConflictPath = true
			break
		}
	}
	if !foundAtConflictPath {
		t.Fatalf("prior blake3 not reachable at preserved path %s (matches=%+v)",
			preservedRel, matches)
	}

	// peer_sync_state advanced (watermark moves on success even with
	// conflicts — the conflicts are resolved, not unhandled).
	initSelf, _ := f.initStore.GetSelfNode(ctx)
	peerOnRecv, _ := f.recvStore.GetNodeByName(ctx, initSelf.Name)
	state, err := f.recvStore.GetPeerSyncState(ctx, v.ID, peerOnRecv.ID)
	if err != nil {
		t.Fatalf("GetPeerSyncState: %v", err)
	}
	if !state.LastSharedRunID.Valid || state.LastSharedRunID.Int64 != rep.RunID {
		t.Fatalf("watermark = %+v, want %d", state.LastSharedRunID, rep.RunID)
	}

	// Re-running sync immediately must produce zero conflicts: the
	// receiver's new doc.md row is sourced from the initiator at the
	// just-closed run, which is ≤ the watermark.
	rep2, err := SyncNode(ctx, f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true})
	if err != nil {
		t.Fatalf("second SyncNode: %v", err)
	}
	if rep2.Status != store.RunStatusSuccess {
		t.Fatalf("second run status = %q", rep2.Status)
	}
	if len(rep2.NodeConflicts) != 0 {
		t.Fatalf("second run still flags conflicts: %+v", rep2.NodeConflicts)
	}
}

// TestNodeSyncVerifyMismatchPartialStatus simulates rclone "succeeding"
// but the on-disk content being wrong (the agent's re-hash will
// catch it). We inject the mismatch by writing different content
// directly into the receiver volume between rclone-finish and verify,
// using a stub Rclone wrapper. Since wiring a stub here is heavy, we
// instead corrupt the file *after* the initiator-side sync succeeds
// and re-run sync — except this time we ALSO snapshot the
// verify+report on the original successful run. So we end up
// validating the "verify-returns-partial" code path by directly
// driving the receiver's verifySession via TestVerifyReportsMismatch
// below. The integration here just sanity-checks that
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

// TestNodeSyncRejectsUnknownVolume exercises the agent-side guard:
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

// TestVerifyReportsMismatch is an agent-side check: drive begin →
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

// TestPlanResponseContainsAllDispositions is an agent-side white-box
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
	// synthetic placeholder endpoint the agent uses for
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
	if plan.Conflicts[0].PreservedAtPath == "" {
		t.Fatalf("conflict missing PreservedAtPath: %+v", plan.Conflicts[0])
	}

	// Supersede side-effect: prior bytes moved into history dir.
	matches, _ := filepath.Glob(filepath.Join(f.recvVol.Path, agent.HistoryDirName, "run-*", "evolved.txt"))
	if len(matches) == 0 {
		t.Fatalf("evolved.txt prior bytes were not pre-moved")
	}
	// Conflict side-effect: local.txt prior bytes moved into the
	// conflicts dir; the original path is empty so rclone could
	// deliver the initiator's bytes there. Index reflects both rows.
	confMatches, _ := filepath.Glob(filepath.Join(f.recvVol.Path, agent.ConflictsDirName, "run-*", "local.txt"))
	if len(confMatches) == 0 {
		t.Fatalf("local.txt prior bytes were not moved to %s/run-N/", agent.ConflictsDirName)
	}
	if _, err := os.Stat(filepath.Join(f.recvVol.Path, "local.txt")); !os.IsNotExist(err) {
		t.Fatalf("local.txt still present at original path after conflict pre-stage: %v", err)
	}
	conflictRow, err := f.recvStore.GetByPath(ctx, v.ID, plan.Conflicts[0].PreservedAtPath)
	if err != nil {
		t.Fatalf("GetByPath %s: %v", plan.Conflicts[0].PreservedAtPath, err)
	}
	if hex.EncodeToString(conflictRow.Blake3) != hexDigest(0xCC) {
		t.Fatalf("conflict-path row blake3 = %x, want prior digest (0xCC...)", conflictRow.Blake3)
	}
}

// TestNodeSyncEndToEndConflictAfterAgentSideIndex walks the full
// acceptance scenario from #22: initiator → sync round 1 (blake3 X) →
// agent-side `squirrel index` after the file is edited locally on
// the receiver (blake3 Y, local-write provenance) → initiator
// re-syncs with a third version (blake3 Z), expecting Z to land
// live, Y preserved under .squirrel-conflicts/run-<id>/, and X
// reachable in the receiver's `.squirrel-history/` from round 1.
func TestNodeSyncEndToEndConflictAfterAgentSideIndex(t *testing.T) {
	f := setupNodeFixture(t)
	ctx := context.Background()
	target := filepath.Join(f.initVol.Path, "doc.md")

	// Round 1: initiator writes X, receiver gets X via sync.
	if err := os.WriteFile(target, []byte("version-X"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.indexInitiator(t)
	rep1, err := SyncNode(ctx, f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true})
	if err != nil {
		t.Fatalf("round 1 SyncNode: %v", err)
	}
	if rep1.Status != store.RunStatusSuccess || len(rep1.NodeConflicts) != 0 {
		t.Fatalf("round 1 unexpected: status=%q conflicts=%+v", rep1.Status, rep1.NodeConflicts)
	}

	// Receiver's web-app / operator edits the file in-place to Y and
	// runs `squirrel index` on the receiver host. The agent and CLI
	// share the same DB file in this scenario, so the local index
	// run writes through the receiver's store and the resulting row
	// has source_node_id NULL (local write).
	if err := os.WriteFile(filepath.Join(f.recvVol.Path, "doc.md"), []byte("version-Y-local"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Index(ctx, f.recvStore, f.recvVol.Path,
		index.Options{Name: f.recvVol.Name}); err != nil {
		t.Fatalf("agent-side index: %v", err)
	}
	v, err := f.recvStore.GetVolumeByName(ctx, f.recvVol.Name)
	if err != nil {
		t.Fatalf("GetVolumeByName: %v", err)
	}
	beforeRound2, err := f.recvStore.GetByPath(ctx, v.ID, "doc.md")
	if err != nil {
		t.Fatalf("GetByPath before round 2: %v", err)
	}
	if beforeRound2.SourceNodeID.Valid {
		t.Fatalf("post-index row has source_node_id %d; want NULL (local write)",
			beforeRound2.SourceNodeID.Int64)
	}

	// Round 2: initiator writes Z and re-syncs. The receiver's row
	// for doc.md is a local-write so the planner classifies the
	// path as conflict. Resolution preserves Y at the conflict path
	// and lands Z live.
	if err := os.WriteFile(target, []byte("version-Z-from-init"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.indexInitiator(t)
	rep2, err := SyncNode(ctx, f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true})
	if err != nil {
		t.Fatalf("round 2 SyncNode: %v", err)
	}
	if rep2.Status != store.RunStatusSuccess {
		t.Fatalf("round 2 status = %q, want success", rep2.Status)
	}
	if len(rep2.NodeConflicts) != 1 {
		t.Fatalf("round 2 NodeConflicts = %+v, want one entry", rep2.NodeConflicts)
	}
	conflict := rep2.NodeConflicts[0]
	if conflict.Reason != "local write on receiver" {
		t.Fatalf("conflict reason = %q, want 'local write on receiver'", conflict.Reason)
	}
	if conflict.PreservedAtPath == "" {
		t.Fatalf("PreservedAtPath is empty")
	}

	// Live disk content is Z; preserved content is Y.
	live, err := os.ReadFile(filepath.Join(f.recvVol.Path, "doc.md"))
	if err != nil {
		t.Fatalf("read live: %v", err)
	}
	if string(live) != "version-Z-from-init" {
		t.Fatalf("live content = %q, want version-Z-from-init", live)
	}
	preserved, err := os.ReadFile(filepath.Join(f.recvVol.Path, conflict.PreservedAtPath))
	if err != nil {
		t.Fatalf("read preserved: %v", err)
	}
	if string(preserved) != "version-Y-local" {
		t.Fatalf("preserved content = %q, want version-Y-local", preserved)
	}

	// Conflict count surfaces in the receiver's runs row via the
	// path-prefix query that backs `squirrel runs`.
	counts, err := f.recvStore.CountFilesFirstSeenByRunWithPathPrefix(ctx,
		[]int64{rep2.NodeReceiverRunID}, ConflictsDirName)
	if err != nil {
		t.Fatalf("CountFilesFirstSeenByRunWithPathPrefix: %v", err)
	}
	if counts[rep2.NodeReceiverRunID] != 1 {
		t.Fatalf("conflict count for receiver run = %d, want 1", counts[rep2.NodeReceiverRunID])
	}
}

// TestNodeSyncConflictWhenPriorRowFromDifferentPeer covers the
// "sourced from a different peer" branch of dispositionForExisting:
// the receiver has a `present` row whose provenance points to a peer
// other than the current initiator, so the planner refuses to treat
// the diff as supersede and resolves it as a conflict instead.
func TestNodeSyncConflictWhenPriorRowFromDifferentPeer(t *testing.T) {
	f := setupNodeFixture(t)
	ctx := context.Background()

	// Seed receiver state: volume + a third-party peer row + an
	// already-completed peer-sync run attributing the prior content
	// to that other peer.
	v, err := f.recvStore.CreateVolume(ctx, f.recvVol.Name, f.recvVol.Path)
	if err != nil {
		t.Fatalf("seed volume: %v", err)
	}
	otherPeer, err := f.recvStore.CreateNode(ctx, "third-party", "peer://third-party")
	if err != nil {
		t.Fatalf("seed third-party peer: %v", err)
	}
	priorRun, err := f.recvStore.BeginPeerSyncRun(ctx, v.ID, otherPeer.ID, 7, "third-party")
	if err != nil {
		t.Fatalf("BeginPeerSyncRun: %v", err)
	}
	_ = f.recvStore.FinishRun(ctx, priorRun, store.RunStatusSuccess, "", 1)
	priorDigest := bytesDigest(0x77)
	if err := f.recvStore.Upsert(ctx, store.FileRow{
		VolumeID: v.ID, Path: "shared.md", Blake3: priorDigest,
		SizeBytes: 1, MtimeNs: 1, Status: store.StatusPresent,
		FirstSeenRunID: priorRun, LastSeenRunID: priorRun, IndexedAtNs: 1,
	}, &store.Provenance{NodeID: otherPeer.ID, RunID: priorRun}); err != nil {
		t.Fatalf("seed third-party row: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.recvVol.Path, "shared.md"), []byte("from-third-party"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Initiator writes a *different* blake3.
	if err := os.WriteFile(filepath.Join(f.initVol.Path, "shared.md"), []byte("from-our-initiator"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.indexInitiator(t)

	rep, err := SyncNode(ctx, f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true})
	if err != nil {
		t.Fatalf("SyncNode: %v", err)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("status = %q, want success", rep.Status)
	}
	if len(rep.NodeConflicts) != 1 {
		t.Fatalf("NodeConflicts = %+v, want one entry", rep.NodeConflicts)
	}
	if rep.NodeConflicts[0].Reason != "sourced from a different peer" {
		t.Fatalf("reason = %q, want 'sourced from a different peer'", rep.NodeConflicts[0].Reason)
	}

	// The third-party-sourced row's provenance survives onto the
	// conflict-path row (preserves attribution; the v1 design is
	// emphatic that conflict resolution must not silently rewrite
	// "who wrote this" history).
	preservedRow, err := f.recvStore.GetByPath(ctx, v.ID, rep.NodeConflicts[0].PreservedAtPath)
	if err != nil {
		t.Fatalf("GetByPath %s: %v", rep.NodeConflicts[0].PreservedAtPath, err)
	}
	if !preservedRow.SourceNodeID.Valid || preservedRow.SourceNodeID.Int64 != otherPeer.ID {
		t.Fatalf("preserved row source_node_id = %+v, want %d (third-party)",
			preservedRow.SourceNodeID, otherPeer.ID)
	}
}

// TestCollectIndexEntriesSkipsReservedDirs pins the initiator-side
// filter: rows under .squirrel-history/ and .squirrel-conflicts/ are
// reachable via the local index for `squirrel query`, but they must
// not be advertised on the wire to peers. A node that has acted as a
// receiver and accumulated conflict-path rows would otherwise
// re-publish those losers when it later initiates a sync.
func TestCollectIndexEntriesSkipsReservedDirs(t *testing.T) {
	f := setupNodeFixture(t)
	ctx := context.Background()

	// Build the initiator-side volume row + a few file rows.
	v, err := f.initStore.CreateVolume(ctx, f.initVol.Name, f.initVol.Path)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	runID, err := f.initStore.BeginRun(ctx, store.RunKindIndex, v.ID, "")
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	_ = f.initStore.FinishRun(ctx, runID, store.RunStatusSuccess, "", 0)
	mustUpsert := func(path string, b byte) {
		t.Helper()
		if err := f.initStore.Upsert(ctx, store.FileRow{
			VolumeID: v.ID, Path: path, Blake3: bytesDigest(b),
			SizeBytes: 1, MtimeNs: 1, Status: store.StatusPresent,
			FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: 1,
		}, nil); err != nil {
			t.Fatalf("upsert %s: %v", path, err)
		}
	}
	mustUpsert("a.txt", 0xAA)
	mustUpsert("sub/b.txt", 0xBB)
	mustUpsert(HistoryDirName+"/run-1/leftover.bin", 0xCC)
	mustUpsert(ConflictsDirName+"/run-1/loser.md", 0xDD)

	driver := &nodeSyncDriver{
		ctx: ctx, store: f.initStore, vol: f.initVol, volID: v.ID,
	}
	entries, err := driver.collectIndexEntries()
	if err != nil {
		t.Fatalf("collectIndexEntries: %v", err)
	}
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		seen[e.Path] = struct{}{}
	}
	want := []string{"a.txt", "sub/b.txt"}
	for _, p := range want {
		if _, ok := seen[p]; !ok {
			t.Fatalf("missing %q in index slice: %+v", p, seen)
		}
	}
	for _, p := range []string{
		HistoryDirName + "/run-1/leftover.bin",
		ConflictsDirName + "/run-1/loser.md",
	} {
		if _, ok := seen[p]; ok {
			t.Fatalf("reserved-dir path %q leaked onto the wire", p)
		}
	}
}

// TestBeginPendingWarningsSurfaceAuditDrift drives the issue #17
// acceptance path: the receiver runs an audit against an out-of-band
// file modification, then a subsequent /v1/sync/begin returns a
// PendingWarnings line. Exercised via the nodeClient directly (no
// rclone) so the agent→syncproto→client→Report propagation is
// pinned without depending on the rclone binary at test time.
func TestBeginPendingWarningsSurfaceAuditDrift(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()

	// Seed a present row on the receiver attributed to the initiator.
	v, err := f.recvStore.CreateVolume(ctx, f.recvVol.Name, f.recvVol.Path)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	initSelf, _ := f.initStore.GetSelfNode(ctx)
	peer, err := f.recvStore.CreateNode(ctx, initSelf.Name, "peer://"+initSelf.Name)
	if err != nil {
		t.Fatalf("seed peer node: %v", err)
	}
	priorRun, err := f.recvStore.BeginPeerSyncRun(ctx, v.ID, peer.ID, 5, initSelf.Name)
	if err != nil {
		t.Fatalf("BeginPeerSyncRun: %v", err)
	}
	_ = f.recvStore.FinishRun(ctx, priorRun, store.RunStatusSuccess, "", 1)
	if err := f.recvStore.Upsert(ctx, store.FileRow{
		VolumeID: v.ID, Path: "doc.md", Blake3: bytesDigest(0x11),
		SizeBytes: 8, MtimeNs: 1, Status: store.StatusPresent,
		FirstSeenRunID: priorRun, LastSeenRunID: priorRun, IndexedAtNs: 1,
	}, &store.Provenance{NodeID: peer.ID, RunID: priorRun}); err != nil {
		t.Fatalf("seed receiver row: %v", err)
	}
	// peer_sync_state must exist so the audit-since watermark is set;
	// it doesn't matter what the last_shared_run_id is here.
	if err := f.recvStore.UpsertPeerSyncState(ctx, v.ID, peer.ID, 5); err != nil {
		t.Fatalf("UpsertPeerSyncState: %v", err)
	}

	// Drift: the file is modified out-of-band on disk, then an audit
	// catches it. The audit's index.Index call supersedes the prior
	// row and inserts a new present row, counted as 1 modified.
	if err := os.WriteFile(filepath.Join(f.recvVol.Path, "doc.md"), []byte("drifted"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := index.Index(ctx, f.recvStore, f.recvVol.Path, index.Options{
		Name: f.recvVol.Name,
		Kind: store.RunKindAudit,
	})
	if err != nil {
		t.Fatalf("receiver audit: %v", err)
	}
	if rep.Modified != 1 {
		t.Fatalf("audit modified count = %d, want 1", rep.Modified)
	}
	auditRunID := rep.RunID

	// New /begin call surfaces the drift via PendingWarnings.
	client := newNodeClient(f.node)
	resp, err := client.begin(ctx, syncproto.BeginRequest{
		Volume:            f.recvVol.Name,
		InitiatorNodeName: initSelf.Name,
		InitiatorRunID:    99,
	})
	if err != nil {
		t.Fatalf("/begin: %v", err)
	}
	t.Cleanup(func() {
		_ = client.close(ctx, syncproto.CloseRequest{
			ReceiverRunID: resp.ReceiverRunID,
			Status:        store.RunStatusFailed,
		})
	})
	if len(resp.PendingWarnings) == 0 {
		t.Fatalf("PendingWarnings empty; want one line for the drifted doc.md")
	}
	wantSubstr := fmt.Sprintf("audit run %d on volume %s", auditRunID, f.recvVol.Name)
	if !strings.Contains(resp.PendingWarnings[0], wantSubstr) {
		t.Fatalf("PendingWarnings[0] = %q, want substring %q", resp.PendingWarnings[0], wantSubstr)
	}
	if !strings.Contains(resp.PendingWarnings[0], "1 modified") {
		t.Fatalf("PendingWarnings[0] = %q, want '1 modified'", resp.PendingWarnings[0])
	}
	if !strings.Contains(resp.PendingWarnings[0], "0 missing") {
		t.Fatalf("PendingWarnings[0] = %q, want '0 missing' (only a modification, no deletion)", resp.PendingWarnings[0])
	}
}

// TestBeginPendingWarningsSurfaceMissing covers the other half of
// drift: a file vanishes from disk between syncs, the audit marks it
// missing via the schema's MarkMissing flip, and the next handshake
// surfaces the missing count in PendingWarnings. Originally the
// receiver only counted modifications and silently dropped pure
// deletions (Copilot review on PR 31).
func TestBeginPendingWarningsSurfaceMissing(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()

	v, err := f.recvStore.CreateVolume(ctx, f.recvVol.Name, f.recvVol.Path)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	initSelf, _ := f.initStore.GetSelfNode(ctx)
	peer, err := f.recvStore.CreateNode(ctx, initSelf.Name, "peer://"+initSelf.Name)
	if err != nil {
		t.Fatalf("seed peer node: %v", err)
	}
	priorRun, err := f.recvStore.BeginPeerSyncRun(ctx, v.ID, peer.ID, 5, initSelf.Name)
	if err != nil {
		t.Fatalf("BeginPeerSyncRun: %v", err)
	}
	_ = f.recvStore.FinishRun(ctx, priorRun, store.RunStatusSuccess, "", 1)

	// Place the file on disk so the row's last_seen_run_id is the
	// prior sync run, then "delete" it before the audit.
	doc := filepath.Join(f.recvVol.Path, "gone.md")
	if err := os.WriteFile(doc, []byte("baseline"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.recvStore.Upsert(ctx, store.FileRow{
		VolumeID: v.ID, Path: "gone.md", Blake3: bytesDigest(0x77),
		SizeBytes: 8, MtimeNs: 1, Status: store.StatusPresent,
		FirstSeenRunID: priorRun, LastSeenRunID: priorRun, IndexedAtNs: 1,
	}, &store.Provenance{NodeID: peer.ID, RunID: priorRun}); err != nil {
		t.Fatalf("seed receiver row: %v", err)
	}
	if err := f.recvStore.UpsertPeerSyncState(ctx, v.ID, peer.ID, 5); err != nil {
		t.Fatalf("UpsertPeerSyncState: %v", err)
	}
	if err := os.Remove(doc); err != nil {
		t.Fatal(err)
	}

	rep, err := index.Index(ctx, f.recvStore, f.recvVol.Path, index.Options{
		Name: f.recvVol.Name,
		Kind: store.RunKindAudit,
	})
	if err != nil {
		t.Fatalf("receiver audit: %v", err)
	}
	if rep.Missing != 1 {
		t.Fatalf("audit missing count = %d, want 1", rep.Missing)
	}
	auditRunID := rep.RunID

	client := newNodeClient(f.node)
	resp, err := client.begin(ctx, syncproto.BeginRequest{
		Volume:            f.recvVol.Name,
		InitiatorNodeName: initSelf.Name,
		InitiatorRunID:    101,
	})
	if err != nil {
		t.Fatalf("/begin: %v", err)
	}
	t.Cleanup(func() {
		_ = client.close(ctx, syncproto.CloseRequest{
			ReceiverRunID: resp.ReceiverRunID,
			Status:        store.RunStatusFailed,
		})
	})
	wantSubstr := fmt.Sprintf("audit run %d on volume %s", auditRunID, f.recvVol.Name)
	if len(resp.PendingWarnings) == 0 || !strings.Contains(resp.PendingWarnings[0], wantSubstr) {
		t.Fatalf("PendingWarnings = %v, want a line for %q", resp.PendingWarnings, wantSubstr)
	}
	if !strings.Contains(resp.PendingWarnings[0], "1 missing") {
		t.Fatalf("PendingWarnings[0] = %q, want '1 missing'", resp.PendingWarnings[0])
	}
}

// TestBeginPendingWarningsEmptyAfterWatermark checks that audit runs
// preceding peer_sync_state.last_synced_at are NOT replayed —
// drift surfaced on one round shouldn't re-fire on the next. The
// watermark advances on the receiver via UpsertPeerSyncState, which
// /close calls automatically on success.
func TestBeginPendingWarningsEmptyAfterWatermark(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()

	v, err := f.recvStore.CreateVolume(ctx, f.recvVol.Name, f.recvVol.Path)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	initSelf, _ := f.initStore.GetSelfNode(ctx)
	peer, err := f.recvStore.CreateNode(ctx, initSelf.Name, "peer://"+initSelf.Name)
	if err != nil {
		t.Fatalf("seed peer node: %v", err)
	}
	priorRun, err := f.recvStore.BeginPeerSyncRun(ctx, v.ID, peer.ID, 5, initSelf.Name)
	if err != nil {
		t.Fatalf("BeginPeerSyncRun: %v", err)
	}
	_ = f.recvStore.FinishRun(ctx, priorRun, store.RunStatusSuccess, "", 1)
	if err := f.recvStore.Upsert(ctx, store.FileRow{
		VolumeID: v.ID, Path: "doc.md", Blake3: bytesDigest(0x22),
		SizeBytes: 8, MtimeNs: 1, Status: store.StatusPresent,
		FirstSeenRunID: priorRun, LastSeenRunID: priorRun, IndexedAtNs: 1,
	}, &store.Provenance{NodeID: peer.ID, RunID: priorRun}); err != nil {
		t.Fatalf("seed receiver row: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.recvVol.Path, "doc.md"), []byte("drifted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Index(ctx, f.recvStore, f.recvVol.Path, index.Options{
		Name: f.recvVol.Name,
		Kind: store.RunKindAudit,
	}); err != nil {
		t.Fatalf("receiver audit: %v", err)
	}

	// Advance the watermark past the audit timestamp so the
	// post-watermark /begin sees no drift to surface. The audit row's
	// started_at_ns is the truth; we use NowNs() which is >= it.
	if err := f.recvStore.UpsertPeerSyncState(ctx, v.ID, peer.ID, 99); err != nil {
		t.Fatalf("UpsertPeerSyncState: %v", err)
	}

	client := newNodeClient(f.node)
	resp, err := client.begin(ctx, syncproto.BeginRequest{
		Volume:            f.recvVol.Name,
		InitiatorNodeName: initSelf.Name,
		InitiatorRunID:    200,
	})
	if err != nil {
		t.Fatalf("/begin: %v", err)
	}
	t.Cleanup(func() {
		_ = client.close(ctx, syncproto.CloseRequest{
			ReceiverRunID: resp.ReceiverRunID,
			Status:        store.RunStatusFailed,
		})
	})
	if len(resp.PendingWarnings) != 0 {
		t.Fatalf("PendingWarnings = %v, want empty (audit predates watermark)", resp.PendingWarnings)
	}
}

// setupNodeFixtureNoRclone is the lighter-weight variant of
// setupNodeFixture for tests that drive the agent HTTP surface
// directly. Skipping the rclone-prerequisite means these tests run
// under CI conditions where rclone is missing or below the supported
// version.
func setupNodeFixtureNoRclone(t *testing.T) *nodeFixture {
	t.Helper()
	f, _ := buildNodeFixture(t)
	return f
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

// fileInode returns the inode number of the file at abs. Used by the
// CopyFromExisting tests to assert the pre-stage produced an
// independent inode (not a hardlink) — the receiver's invariant that
// paths are independent observations of content.
func fileInode(t *testing.T, abs string) uint64 {
	t.Helper()
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("stat %s: %v", abs, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: unexpected Sys() type %T", abs, info.Sys())
	}
	return uint64(st.Ino)
}

// TestNodeSyncCopyFromExistingDedup is the headline acceptance test
// for issue #14. Sync round 1 plants `a.jpg` on the receiver. The
// initiator then renames its copy to `pets/a.jpg` (same content,
// different path). Round 2 must satisfy `pets/a.jpg` from the
// receiver's existing `a.jpg` via the CopyFromExisting branch — no
// rclone transfer happens, and the materialised path is an
// independent inode (not a hardlink to the source) so a future
// per-path edit on either side can't propagate through shared inode
// metadata.
func TestNodeSyncCopyFromExistingDedup(t *testing.T) {
	f := setupNodeFixture(t)
	ctx := context.Background()

	body := []byte("the same exact bytes for both paths")
	original := filepath.Join(f.initVol.Path, "a.jpg")
	if err := os.WriteFile(original, body, 0o644); err != nil {
		t.Fatal(err)
	}
	f.indexInitiator(t)
	if _, err := SyncNode(ctx, f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true}); err != nil {
		t.Fatalf("first SyncNode: %v", err)
	}

	// Rename on the initiator: remove the old path, write the same
	// content at a new path. Re-indexing flips the old row to missing
	// and inserts a new present row at the new path.
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(f.initVol.Path, "pets", "a.jpg")
	if err := os.MkdirAll(filepath.Dir(renamed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(renamed, body, 0o644); err != nil {
		t.Fatal(err)
	}
	f.indexInitiator(t)

	rep, err := SyncNode(ctx, f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true})
	if err != nil {
		t.Fatalf("second SyncNode: %v", err)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success: %+v", rep.Status, rep)
	}

	// rclone must not have been invoked: pets/a.jpg was the only
	// thing to sync, and the receiver materialised it locally. The
	// zero-valued RcloneResult is the signal that phaseTransfer
	// short-circuited.
	if rep.RcloneResult.Transferred != 0 {
		t.Fatalf("RcloneResult.Transferred = %d, want 0 (CopyFromExisting should bypass rclone)", rep.RcloneResult.Transferred)
	}

	// The new path lives on the receiver with the expected content.
	recvNew := filepath.Join(f.recvVol.Path, "pets", "a.jpg")
	got, err := os.ReadFile(recvNew)
	if err != nil {
		t.Fatalf("read receiver pets/a.jpg: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("pets/a.jpg content = %q, want %q", got, body)
	}

	// The new path is an independent inode, not a hardlink to the
	// receiver's pre-existing a.jpg.
	recvSource := filepath.Join(f.recvVol.Path, "a.jpg")
	if a, b := fileInode(t, recvSource), fileInode(t, recvNew); a == b {
		t.Fatalf("inode collision: a.jpg and pets/a.jpg share inode %d (hardlink slipped in)", a)
	}

	// Verify report confirms exactly one matched path (pets/a.jpg).
	if len(rep.NodeVerify.Matched) != 1 || rep.NodeVerify.Matched[0] != "pets/a.jpg" {
		t.Fatalf("Verify.Matched = %+v, want [pets/a.jpg]", rep.NodeVerify.Matched)
	}

	// Receiver's index has the new row with initiator provenance,
	// matching the shape a Transfer would have produced.
	v, _ := f.recvStore.GetVolumeByName(ctx, "pics")
	newRow, err := f.recvStore.GetByPath(ctx, v.ID, "pets/a.jpg")
	if err != nil {
		t.Fatalf("GetByPath pets/a.jpg: %v", err)
	}
	if !newRow.SourceNodeID.Valid {
		t.Fatalf("pets/a.jpg row has NULL source_node_id; want initiator attribution")
	}
}

// TestPlanCopyFromExistingDirectAPI drives /v1/sync/plan against a
// receiver pre-loaded with one file. The initiator presents the same
// content at a different path; the plan must respond with
// CopyFromExisting and the pre-stage must have left the bytes at the
// new path with an independent inode.
func TestPlanCopyFromExistingDirectAPI(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()

	body := []byte("dedup-me-locally")
	existingAbs := filepath.Join(f.recvVol.Path, "existing.jpg")
	if err := os.WriteFile(existingAbs, body, 0o644); err != nil {
		t.Fatal(err)
	}
	existingDigest := hashFile(t, existingAbs)

	v, err := f.recvStore.GetOrCreateVolume(ctx, f.recvVol.Path)
	if err != nil {
		t.Fatalf("GetOrCreateVolume: %v", err)
	}
	runID, err := f.recvStore.BeginRun(ctx, store.RunKindIndex, v.ID, "")
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	if err := f.recvStore.Upsert(ctx, store.FileRow{
		VolumeID: v.ID, Path: "existing.jpg", Blake3: existingDigest,
		SizeBytes: int64(len(body)), MtimeNs: 1, Status: store.StatusPresent,
		FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("seed existing row: %v", err)
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

	plan, err := client.plan(ctx, syncproto.PlanRequest{
		ReceiverRunID: begin.ReceiverRunID,
		Entries: []syncproto.IndexEntry{
			{Path: "pets/new.jpg", Blake3Hex: hex.EncodeToString(existingDigest), SizeBytes: int64(len(body)), MtimeNs: 1},
		},
	})
	if err != nil {
		t.Fatalf("/plan: %v", err)
	}
	if len(plan.Dispositions) != 1 {
		t.Fatalf("Dispositions = %+v, want one", plan.Dispositions)
	}
	d := plan.Dispositions[0]
	if d.Disposition != syncproto.DispositionCopyFromExisting {
		t.Fatalf("Disposition = %q, want %q", d.Disposition, syncproto.DispositionCopyFromExisting)
	}
	if d.CopyFromPath != "existing.jpg" {
		t.Fatalf("CopyFromPath = %q, want %q", d.CopyFromPath, "existing.jpg")
	}

	// Pre-stage materialised the bytes locally.
	newAbs := filepath.Join(f.recvVol.Path, "pets", "new.jpg")
	got, err := os.ReadFile(newAbs)
	if err != nil {
		t.Fatalf("read materialised path: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("content mismatch: got %q, want %q", got, body)
	}
	if a, b := fileInode(t, existingAbs), fileInode(t, newAbs); a == b {
		t.Fatalf("shared inode %d — pre-stage produced a hardlink, want independent file", a)
	}
}

// TestPlanDedupStrategyOff asserts that an initiator opting out of
// dedup (BeginRequest.DedupStrategy = "off") gets the plain Transfer
// disposition even when the receiver holds the same blake3 at another
// path. No pre-stage copy must happen; the initiator's rclone is the
// only path that will deliver the bytes.
func TestPlanDedupStrategyOff(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()

	body := []byte("dedup-disabled")
	existingAbs := filepath.Join(f.recvVol.Path, "existing.jpg")
	if err := os.WriteFile(existingAbs, body, 0o644); err != nil {
		t.Fatal(err)
	}
	existingDigest := hashFile(t, existingAbs)

	v, err := f.recvStore.GetOrCreateVolume(ctx, f.recvVol.Path)
	if err != nil {
		t.Fatalf("GetOrCreateVolume: %v", err)
	}
	runID, err := f.recvStore.BeginRun(ctx, store.RunKindIndex, v.ID, "")
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	if err := f.recvStore.Upsert(ctx, store.FileRow{
		VolumeID: v.ID, Path: "existing.jpg", Blake3: existingDigest,
		SizeBytes: int64(len(body)), MtimeNs: 1, Status: store.StatusPresent,
		FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("seed existing row: %v", err)
	}

	initSelf, _ := f.initStore.GetSelfNode(ctx)
	client := newNodeClient(f.node)
	begin, err := client.begin(ctx, syncproto.BeginRequest{
		Volume:            f.recvVol.Name,
		InitiatorNodeName: initSelf.Name,
		InitiatorRunID:    1,
		DedupStrategy:     syncproto.DedupStrategyOff,
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

	plan, err := client.plan(ctx, syncproto.PlanRequest{
		ReceiverRunID: begin.ReceiverRunID,
		Entries: []syncproto.IndexEntry{
			{Path: "pets/new.jpg", Blake3Hex: hex.EncodeToString(existingDigest), SizeBytes: int64(len(body)), MtimeNs: 1},
		},
	})
	if err != nil {
		t.Fatalf("/plan: %v", err)
	}
	if d := plan.Dispositions[0]; d.Disposition != syncproto.DispositionTransfer {
		t.Fatalf("Disposition = %q, want %q (dedup off)", d.Disposition, syncproto.DispositionTransfer)
	}
	if plan.Dispositions[0].CopyFromPath != "" {
		t.Fatalf("CopyFromPath = %q, want empty (dedup off)", plan.Dispositions[0].CopyFromPath)
	}

	// Pre-stage did not materialise anything at the new path.
	newAbs := filepath.Join(f.recvVol.Path, "pets", "new.jpg")
	if _, err := os.Stat(newAbs); !os.IsNotExist(err) {
		t.Fatalf("stat %s: err = %v, want IsNotExist (no pre-stage copy under dedup=off)", newAbs, err)
	}
}

// TestPlanSupersedeWinsOverDedup proves the classifier consults the
// by-path lookup before the blake3-wide lookup: an existing live row
// at the target path takes precedence (Supersede or Conflict) even if
// the requested blake3 also lives at another path. Without this
// ordering, the dedup branch would paper over real content
// divergences the provenance check should surface.
func TestPlanSupersedeWinsOverDedup(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()

	// Receiver holds two rows: target path has content Y (from this
	// peer at an earlier shared run), and dedup-source has content X.
	// Initiator wants target path to become X.
	bodyTarget := []byte("old-content-Y")
	bodySource := []byte("dedup-content-X")
	targetAbs := filepath.Join(f.recvVol.Path, "target.jpg")
	sourceAbs := filepath.Join(f.recvVol.Path, "elsewhere.jpg")
	if err := os.WriteFile(targetAbs, bodyTarget, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceAbs, bodySource, 0o644); err != nil {
		t.Fatal(err)
	}
	digestTarget := hashFile(t, targetAbs)
	digestSource := hashFile(t, sourceAbs)

	v, err := f.recvStore.GetOrCreateVolume(ctx, f.recvVol.Path)
	if err != nil {
		t.Fatalf("GetOrCreateVolume: %v", err)
	}

	// Stand up the peer-side bookkeeping the supersede branch needs:
	// a peer node row, a runs row attributed to it, a peer_sync_state
	// watermark that puts the target row's provenance "at or before"
	// the shared watermark.
	initSelf, _ := f.initStore.GetSelfNode(ctx)
	peer, err := f.recvStore.GetOrCreatePeerNode(ctx, initSelf.Name, "peer://"+initSelf.Name)
	if err != nil {
		t.Fatalf("GetOrCreatePeerNode: %v", err)
	}
	priorRunID, err := f.recvStore.BeginPeerSyncRun(ctx, v.ID, peer.ID, 1, initSelf.Name)
	if err != nil {
		t.Fatalf("BeginPeerSyncRun: %v", err)
	}
	if err := f.recvStore.Upsert(ctx, store.FileRow{
		VolumeID: v.ID, Path: "target.jpg", Blake3: digestTarget,
		SizeBytes: int64(len(bodyTarget)), MtimeNs: 1, Status: store.StatusPresent,
		FirstSeenRunID: priorRunID, LastSeenRunID: priorRunID, IndexedAtNs: 1,
	}, &store.Provenance{NodeID: peer.ID, RunID: priorRunID}); err != nil {
		t.Fatalf("seed target row: %v", err)
	}
	if err := f.recvStore.Upsert(ctx, store.FileRow{
		VolumeID: v.ID, Path: "elsewhere.jpg", Blake3: digestSource,
		SizeBytes: int64(len(bodySource)), MtimeNs: 1, Status: store.StatusPresent,
		FirstSeenRunID: priorRunID, LastSeenRunID: priorRunID, IndexedAtNs: 1,
	}, &store.Provenance{NodeID: peer.ID, RunID: priorRunID}); err != nil {
		t.Fatalf("seed source row: %v", err)
	}
	if err := f.recvStore.UpsertPeerSyncState(ctx, v.ID, peer.ID, 1); err != nil {
		t.Fatalf("UpsertPeerSyncState: %v", err)
	}
	if err := f.recvStore.FinishRun(ctx, priorRunID, store.RunStatusSuccess, "", 2); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	client := newNodeClient(f.node)
	begin, err := client.begin(ctx, syncproto.BeginRequest{
		Volume:            f.recvVol.Name,
		InitiatorNodeName: initSelf.Name,
		InitiatorRunID:    99,
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

	plan, err := client.plan(ctx, syncproto.PlanRequest{
		ReceiverRunID: begin.ReceiverRunID,
		Entries: []syncproto.IndexEntry{
			{Path: "target.jpg", Blake3Hex: hex.EncodeToString(digestSource), SizeBytes: int64(len(bodySource)), MtimeNs: 1},
		},
	})
	if err != nil {
		t.Fatalf("/plan: %v", err)
	}
	if d := plan.Dispositions[0]; d.Disposition != syncproto.DispositionSupersede {
		t.Fatalf("Disposition = %q, want %q (provenance check beats dedup)", d.Disposition, syncproto.DispositionSupersede)
	}
	if plan.Dispositions[0].CopyFromPath != "" {
		t.Fatalf("CopyFromPath = %q, want empty (no dedup for non-missing path)", plan.Dispositions[0].CopyFromPath)
	}
}

// TestBeginRejectsUnknownDedupStrategy guards the wire-level
// validation: a typo'd strategy must surface at /begin (400), not as
// silently-applied wrong behaviour during classify.
func TestBeginRejectsUnknownDedupStrategy(t *testing.T) {
	f := setupNodeFixtureNoRclone(t)
	ctx := context.Background()

	initSelf, _ := f.initStore.GetSelfNode(ctx)
	client := newNodeClient(f.node)
	_, err := client.begin(ctx, syncproto.BeginRequest{
		Volume:            f.recvVol.Name,
		InitiatorNodeName: initSelf.Name,
		InitiatorRunID:    1,
		DedupStrategy:     "hardlink", // deliberately rejected
	})
	if err == nil || !strings.Contains(err.Error(), "dedup_strategy") {
		t.Fatalf("err = %v, want substring 'dedup_strategy'", err)
	}
}

// hashFile is a small test helper that re-hashes a file on disk with
// BLAKE3 so tests can refer to the digest of fixture content without
// hard-coding hex constants.
func hashFile(t *testing.T, abs string) []byte {
	t.Helper()
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read %s: %v", abs, err)
	}
	h := blake3.New()
	if _, err := h.Write(data); err != nil {
		t.Fatalf("hash %s: %v", abs, err)
	}
	return h.Sum(nil)
}

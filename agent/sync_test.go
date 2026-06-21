package agent

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/syncproto"
	"github.com/zeebo/blake3"
)

// blakeHex returns the lowercase hex BLAKE3 digest of b, matching the
// wire encoding /plan entries carry.
func blakeHex(b []byte) string {
	sum := blake3.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// preStageFixture is a minimal receiver: an on-disk volume plus a store
// with that volume's row, ready to drive planSession against. Tests
// seed the index and the volume tree, then call plan with one initiator
// entry per path.
type preStageFixture struct {
	router   *peerSyncRouter
	store    *store.Store
	vol      *config.Volume
	volID    int64
	peerID   int64
	peerRun  int64
	localRun int64
	recvRun  int64
}

// newPreStageFixture builds the fixture: a fresh volume directory, a
// migrated store carrying its volume row, a peer node, and a finished
// peer-sync run whose correlated id sits at/below the watermark so a
// peer-sourced prior row classifies as Supersede.
func newPreStageFixture(t *testing.T) *preStageFixture {
	t.Helper()
	ctx := context.Background()
	volRoot := t.TempDir()
	vol := &config.Volume{Name: "pics", Path: volRoot}
	srv := newTestServer(t, Config{Volumes: map[string]*config.Volume{vol.Name: vol}})

	v, err := srv.store.CreateVolume(ctx, vol.Name, vol.Path)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	peer, err := srv.store.CreateNode(ctx, "peerA", "peer://peerA")
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	const correlated = int64(5)
	peerRun, err := srv.store.BeginPeerSyncRun(ctx, v.ID, peer.ID, correlated, "peerA")
	if err != nil {
		t.Fatalf("BeginPeerSyncRun: %v", err)
	}
	if err := srv.store.FinishRun(ctx, peerRun, store.RunStatusSuccess, "", 0); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	// Watermark at/above the prior run's correlated id so the supersede
	// branch of dispositionForExisting fires for peer-sourced rows.
	if err := srv.store.UpsertPeerSyncState(ctx, v.ID, peer.ID, correlated, false); err != nil {
		t.Fatalf("UpsertPeerSyncState: %v", err)
	}
	// A finished local index run for rows that must classify as
	// receiver-local writes (delivery is judged by the first-seen run's
	// peer linkage, and an index run has none).
	localRun, err := srv.store.BeginIndexRun(ctx, store.RunKindIndex, v.ID, false)
	if err != nil {
		t.Fatalf("BeginIndexRun: %v", err)
	}
	if err := srv.store.FinishRun(ctx, localRun, store.RunStatusSuccess, "", 0); err != nil {
		t.Fatalf("FinishRun local: %v", err)
	}
	recvRun, err := srv.store.BeginPeerSyncRun(ctx, v.ID, peer.ID, correlated+1, "peerA")
	if err != nil {
		t.Fatalf("BeginPeerSyncRun (receiver): %v", err)
	}
	return &preStageFixture{
		router:   srv.router,
		store:    srv.store,
		vol:      vol,
		volID:    v.ID,
		peerID:   peer.ID,
		peerRun:  peerRun,
		localRun: localRun,
		recvRun:  recvRun,
	}
}

// seedIndexedFile writes content to path on disk and records a present
// index row for it attributed to the fixture's peer (so classify treats
// a divergent initiator hash as Supersede). It returns the recorded
// digest.
func (f *preStageFixture) seedIndexedFile(t *testing.T, ctx context.Context, path string, content []byte) []byte {
	t.Helper()
	abs := filepath.Join(f.vol.Path, path)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(abs, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	sum := blake3.Sum256(content)
	digest := sum[:]
	row := store.FileRow{
		VolumeID:       f.volID,
		Path:           path,
		Blake3:         digest,
		SizeBytes:      int64(len(content)),
		MtimeNs:        store.NowNs(),
		Status:         store.StatusPresent,
		FirstSeenRunID: f.peerRun,
		LastSeenRunID:  f.peerRun,
		IndexedAtNs:    store.NowNs(),
	}
	prov := &store.Provenance{NodeID: f.peerID, RunID: f.peerRun}
	if err := f.store.Upsert(ctx, row, prov); err != nil {
		t.Fatalf("Upsert %s: %v", path, err)
	}
	return digest
}

// newSession returns a peerSession wired to the fixture's peer and the
// receiver run id, ready to hand to planSession.
func (f *preStageFixture) newSession() *peerSession {
	return &peerSession{
		receiverRunID:   f.recvRun,
		volume:          f.vol,
		volumeID:        f.volID,
		peerNodeID:      f.peerID,
		correlatedRunID: 6,
		dedupStrategy:   syncproto.DedupStrategyOff,
		dispositions:    make(map[string]*sessionEntry),
	}
}

// TestPreStageReHashDetectsSupersedeDrift is the issue acceptance: the
// receiver indexed content X at P (peer-sourced, so the verdict would be
// Supersede), then X drifts to Y out-of-band before the sync. The
// re-hash before the pre-move must catch the drift, downgrade P to a
// conflict, and preserve Y under .squirrel-conflicts/ with a row whose
// blake3 is hash(Y) — not the stale hash(X) the index still records.
func TestPreStageReHashDetectsSupersedeDrift(t *testing.T) {
	ctx := context.Background()
	f := newPreStageFixture(t)

	contentX := []byte("the originally-indexed bytes")
	contentY := []byte("DRIFT: a NAS web app replaced these bytes")
	contentZ := []byte("the initiator's incoming bytes")
	hashX := f.seedIndexedFile(t, ctx, "doc.md", contentX)

	// Mutate on disk after indexing — the classify→pre-stage TOCTOU.
	abs := filepath.Join(f.vol.Path, "doc.md")
	if err := os.WriteFile(abs, contentY, 0o644); err != nil {
		t.Fatalf("drift write: %v", err)
	}

	sess := f.newSession()
	plan, err := f.router.planSession(ctx, sess, []syncproto.IndexEntry{
		{Path: "doc.md", Blake3Hex: blakeHex(contentZ), SizeBytes: int64(len(contentZ))},
	})
	if err != nil {
		t.Fatalf("planSession: %v", err)
	}

	if len(plan.Conflicts) != 1 {
		t.Fatalf("plan conflicts = %d, want 1 (drift downgrades supersede to conflict): %+v", len(plan.Conflicts), plan.Conflicts)
	}
	if d := sess.dispositions["doc.md"].disposition; d != syncproto.DispositionConflict {
		t.Fatalf("disposition = %q, want conflict", d)
	}
	if plan.Conflicts[0].Reason != "on-disk drift during sync" {
		t.Fatalf("reason = %q, want %q", plan.Conflicts[0].Reason, "on-disk drift during sync")
	}

	preservedRel := plan.Conflicts[0].PreservedAtPath
	if preservedRel == "" {
		t.Fatalf("PreservedAtPath empty; want a .squirrel-conflicts/... path")
	}

	// The bytes preserved on disk are Y.
	got, err := os.ReadFile(filepath.Join(f.vol.Path, preservedRel))
	if err != nil {
		t.Fatalf("read preserved file: %v", err)
	}
	if string(got) != string(contentY) {
		t.Fatalf("preserved bytes = %q, want the drifted Y", got)
	}

	// The preserved index row's blake3 is hash(Y), not the stale hash(X).
	row, err := f.store.GetByPath(ctx, f.volID, preservedRel)
	if err != nil {
		t.Fatalf("GetByPath preserved: %v", err)
	}
	wantY := blake3.Sum256(contentY)
	if !bytesEqual(row.Blake3, wantY[:]) {
		t.Fatalf("preserved row blake3 = %x, want hash(Y) %x", row.Blake3, wantY[:])
	}
	if bytesEqual(row.Blake3, hashX) {
		t.Fatalf("preserved row carries the stale hash(X) %x — drift not relabelled", hashX)
	}
	if row.SizeBytes != int64(len(contentY)) {
		t.Fatalf("preserved row size = %d, want %d (hash(Y)'s length)", row.SizeBytes, len(contentY))
	}

	// The wire report's receiver hash matches the preserved bytes too.
	if plan.Conflicts[0].ReceiverBlake3Hex != blakeHex(contentY) {
		t.Fatalf("ReceiverBlake3Hex = %s, want hash(Y) %s", plan.Conflicts[0].ReceiverBlake3Hex, blakeHex(contentY))
	}

	// Nothing landed in .squirrel-history for this path — a drift must
	// not be silently filed as a clean supersede.
	histDir := filepath.Join(f.vol.Path, HistoryDirName)
	if entries, err := os.ReadDir(histDir); err == nil && len(entries) > 0 {
		t.Fatalf("history dir is non-empty %v; drift should bypass the supersede bucket", entries)
	}
}

// TestPreStageReHashRelabelsConflictDrift covers the path classify
// already calls a conflict (a different-peer or local prior write):
// when the on-disk bytes drifted, the preserved conflict row must still
// carry the actual on-disk blake3, not the stale indexed one.
func TestPreStageReHashRelabelsConflictDrift(t *testing.T) {
	ctx := context.Background()
	f := newPreStageFixture(t)

	contentX := []byte("indexed bytes for the conflict path")
	contentY := []byte("out-of-band replacement bytes Y Y Y")
	contentZ := []byte("incoming initiator bytes Z")

	// Seed a present row first-seen by a local index run (no peer
	// linkage) so classify returns Conflict rather than Supersede.
	abs := filepath.Join(f.vol.Path, "local.txt")
	if err := os.WriteFile(abs, contentX, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	sumX := blake3.Sum256(contentX)
	hashX := sumX[:]
	if err := f.store.Upsert(ctx, store.FileRow{
		VolumeID:       f.volID,
		Path:           "local.txt",
		Blake3:         hashX,
		SizeBytes:      int64(len(contentX)),
		MtimeNs:        store.NowNs(),
		Status:         store.StatusPresent,
		FirstSeenRunID: f.localRun,
		LastSeenRunID:  f.localRun,
		IndexedAtNs:    store.NowNs(),
	}, nil); err != nil {
		t.Fatalf("Upsert local: %v", err)
	}

	// Drift on disk before the sync.
	if err := os.WriteFile(abs, contentY, 0o644); err != nil {
		t.Fatalf("drift write: %v", err)
	}

	sess := f.newSession()
	plan, err := f.router.planSession(ctx, sess, []syncproto.IndexEntry{
		{Path: "local.txt", Blake3Hex: blakeHex(contentZ), SizeBytes: int64(len(contentZ))},
	})
	if err != nil {
		t.Fatalf("planSession: %v", err)
	}
	if len(plan.Conflicts) != 1 {
		t.Fatalf("conflicts = %d, want 1: %+v", len(plan.Conflicts), plan.Conflicts)
	}
	if plan.Conflicts[0].Reason != "local write on receiver" {
		t.Fatalf("reason = %q, want 'local write on receiver' (classify-time conflict, not the drift downgrade)", plan.Conflicts[0].Reason)
	}
	preservedRel := plan.Conflicts[0].PreservedAtPath
	row, err := f.store.GetByPath(ctx, f.volID, preservedRel)
	if err != nil {
		t.Fatalf("GetByPath preserved: %v", err)
	}
	wantY := blake3.Sum256(contentY)
	if !bytesEqual(row.Blake3, wantY[:]) {
		t.Fatalf("preserved row blake3 = %x, want hash(Y) %x", row.Blake3, wantY[:])
	}
	if bytesEqual(row.Blake3, hashX) {
		t.Fatalf("preserved row carries the stale hash(X) — conflict drift not relabelled")
	}
}

// TestPreStageSupersedeNoDriftStillBuckets is the regression guard: when
// the on-disk bytes still match the index, a supersede behaves exactly
// as before — prior bytes move into .squirrel-history/ and no conflict
// is raised.
func TestPreStageSupersedeNoDriftStillBuckets(t *testing.T) {
	ctx := context.Background()
	f := newPreStageFixture(t)

	contentX := []byte("stable indexed bytes")
	contentZ := []byte("incoming bytes from the initiator")
	f.seedIndexedFile(t, ctx, "doc.md", contentX)

	sess := f.newSession()
	plan, err := f.router.planSession(ctx, sess, []syncproto.IndexEntry{
		{Path: "doc.md", Blake3Hex: blakeHex(contentZ), SizeBytes: int64(len(contentZ))},
	})
	if err != nil {
		t.Fatalf("planSession: %v", err)
	}
	if len(plan.Conflicts) != 0 {
		t.Fatalf("conflicts = %d, want 0 (no drift): %+v", len(plan.Conflicts), plan.Conflicts)
	}
	if d := sess.dispositions["doc.md"].disposition; d != syncproto.DispositionSupersede {
		t.Fatalf("disposition = %q, want supersede", d)
	}

	// Prior bytes are in the history bucket, not at the conflict bucket.
	histPath := filepath.Join(f.vol.Path, HistoryDirName, "run-"+strconv.FormatInt(f.recvRun, 10), "doc.md")
	got, err := os.ReadFile(histPath)
	if err != nil {
		t.Fatalf("read history file %s: %v", histPath, err)
	}
	if string(got) != string(contentX) {
		t.Fatalf("history bytes = %q, want X", got)
	}
	if _, err := os.Stat(filepath.Join(f.vol.Path, ConflictsDirName)); !os.IsNotExist(err) {
		t.Fatalf("conflicts dir exists for a clean supersede (err=%v)", err)
	}
}

// TestCloseRecordsVerbatimOrigin: a /plan entry declaring a forwarded
// origin lands on the contents row exactly as declared — the origin
// node name is resolved (created) locally and the origin-space run id
// is stored untranslated. The receiver has never peered with "delta";
// only the forwarding initiator has.
func TestCloseRecordsVerbatimOrigin(t *testing.T) {
	ctx := context.Background()
	f := newPreStageFixture(t)

	content := []byte("forwarded bytes")
	sess := f.newSession()
	if _, err := f.router.planSession(ctx, sess, []syncproto.IndexEntry{
		{Path: "fwd.txt", Blake3Hex: blakeHex(content), SizeBytes: int64(len(content)),
			OriginNode: "delta", OriginRun: 5},
	}); err != nil {
		t.Fatalf("planSession: %v", err)
	}
	if _, err := f.router.closeSession(ctx, sess, store.RunStatusSuccess, nil); err != nil {
		t.Fatalf("closeSession: %v", err)
	}

	row, err := f.store.GetByPath(ctx, f.volID, "fwd.txt")
	if err != nil {
		t.Fatalf("GetByPath fwd.txt: %v", err)
	}
	delta, err := f.store.GetNodeByName(ctx, "delta")
	if err != nil {
		t.Fatalf("origin node row for delta was not created: %v", err)
	}
	if !row.OriginNodeID.Valid || row.OriginNodeID.Int64 != delta.ID {
		t.Fatalf("OriginNodeID = %+v, want delta's row %d (verbatim, not the initiator %d)",
			row.OriginNodeID, delta.ID, f.peerID)
	}
	if !row.OriginRunID.Valid || row.OriginRunID.Int64 != 5 {
		t.Fatalf("OriginRunID = %+v, want 5 (origin run space, untranslated)", row.OriginRunID)
	}
}

// TestCloseFallbackAttributesToInitiator: an entry without the origin
// pair (a pre-origin-exchange initiator) is attributed to the
// initiator itself at its declared sync run — the initiator-run-space
// coordinate the session already carries.
func TestCloseFallbackAttributesToInitiator(t *testing.T) {
	ctx := context.Background()
	f := newPreStageFixture(t)

	content := []byte("legacy entry without origin")
	sess := f.newSession()
	if _, err := f.router.planSession(ctx, sess, []syncproto.IndexEntry{
		{Path: "old.txt", Blake3Hex: blakeHex(content), SizeBytes: int64(len(content))},
	}); err != nil {
		t.Fatalf("planSession: %v", err)
	}
	if _, err := f.router.closeSession(ctx, sess, store.RunStatusSuccess, nil); err != nil {
		t.Fatalf("closeSession: %v", err)
	}

	row, err := f.store.GetByPath(ctx, f.volID, "old.txt")
	if err != nil {
		t.Fatalf("GetByPath old.txt: %v", err)
	}
	if !row.OriginNodeID.Valid || row.OriginNodeID.Int64 != f.peerID {
		t.Fatalf("OriginNodeID = %+v, want the initiator %d", row.OriginNodeID, f.peerID)
	}
	if !row.OriginRunID.Valid || row.OriginRunID.Int64 != sess.correlatedRunID {
		t.Fatalf("OriginRunID = %+v, want the initiator's sync run %d", row.OriginRunID, sess.correlatedRunID)
	}
}

// TestPlanRejectsMalformedOrigin: half-declared or invalid origins are
// refused at /plan, before any bytes move or rows commit.
func TestPlanRejectsMalformedOrigin(t *testing.T) {
	ctx := context.Background()
	f := newPreStageFixture(t)
	content := []byte("x")

	cases := []struct {
		name  string
		entry syncproto.IndexEntry
	}{
		{"node without run", syncproto.IndexEntry{
			Path: "a.txt", Blake3Hex: blakeHex(content), OriginNode: "delta"}},
		{"run without node", syncproto.IndexEntry{
			Path: "a.txt", Blake3Hex: blakeHex(content), OriginRun: 9}},
		{"negative run", syncproto.IndexEntry{
			Path: "a.txt", Blake3Hex: blakeHex(content), OriginNode: "delta", OriginRun: -1}},
		{"invalid name", syncproto.IndexEntry{
			Path: "a.txt", Blake3Hex: blakeHex(content), OriginNode: "../up", OriginRun: 9}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sess := f.newSession()
			if _, err := f.router.planSession(ctx, sess, []syncproto.IndexEntry{c.entry}); err == nil {
				t.Fatalf("planSession accepted %+v, want error", c.entry)
			}
		})
	}
}

// TestConflictDriftRelabelClearsOrigin: when the conflict pre-stage
// finds drifted bytes, the preserved row describes a fresh, locally
// introduced content — the prior content's origin coordinate must not
// be copied onto bytes that never travelled from that origin, or the
// durability vector would vouch for content no destination holds.
func TestConflictDriftRelabelClearsOrigin(t *testing.T) {
	ctx := context.Background()
	f := newPreStageFixture(t)

	contentX := []byte("peer-delivered bytes")
	contentY := []byte("out-of-band replacement")
	contentZ := []byte("incoming initiator bytes")
	f.seedIndexedFile(t, ctx, "doc.md", contentX)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "doc.md"), contentY, 0o644); err != nil {
		t.Fatalf("drift write: %v", err)
	}

	sess := f.newSession()
	plan, err := f.router.planSession(ctx, sess, []syncproto.IndexEntry{
		{Path: "doc.md", Blake3Hex: blakeHex(contentZ), SizeBytes: int64(len(contentZ))},
	})
	if err != nil {
		t.Fatalf("planSession: %v", err)
	}
	if len(plan.Conflicts) != 1 {
		t.Fatalf("conflicts = %d, want 1", len(plan.Conflicts))
	}
	row, err := f.store.GetByPath(ctx, f.volID, plan.Conflicts[0].PreservedAtPath)
	if err != nil {
		t.Fatalf("GetByPath preserved: %v", err)
	}
	if row.OriginNodeID.Valid || row.OriginRunID.Valid {
		t.Fatalf("preserved drifted row origin = (%+v, %+v), want NULLs (local introduction)",
			row.OriginNodeID, row.OriginRunID)
	}
}

// TestPlanRejectsSelfAttributedOrigin (#105): a peer is never
// authoritative about the receiver's own introductions, so an origin
// naming the receiver's self node is refused at /plan — whether it
// carries a plausible run id (a peer must re-introduce locally with a
// NULL origin) or an absurd one above the receiver's latest allocated
// run id (the durability-vector poisoning attack). A legitimate
// forwarded third-party origin under the same plan still commits.
func TestPlanRejectsSelfAttributedOrigin(t *testing.T) {
	ctx := context.Background()
	f := newPreStageFixture(t)
	self, err := f.store.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	content := []byte("incoming bytes")

	t.Run("self name with plausible run refused", func(t *testing.T) {
		sess := f.newSession()
		_, err := f.router.planSession(ctx, sess, []syncproto.IndexEntry{
			{Path: "a.txt", Blake3Hex: blakeHex(content), SizeBytes: int64(len(content)),
				OriginNode: self.Name, OriginRun: 1},
		})
		if err == nil {
			t.Fatalf("planSession accepted a self-attributed origin, want refusal")
		}
	})

	t.Run("self name with absurd run refused", func(t *testing.T) {
		sess := f.newSession()
		_, err := f.router.planSession(ctx, sess, []syncproto.IndexEntry{
			{Path: "a.txt", Blake3Hex: blakeHex(content), SizeBytes: int64(len(content)),
				OriginNode: self.Name, OriginRun: sess.receiverRunID + 1_000_000},
		})
		if err == nil {
			t.Fatalf("planSession accepted an absurd self origin_run, want refusal")
		}
	})

	t.Run("forwarded third-party origin accepted", func(t *testing.T) {
		sess := f.newSession()
		if _, err := f.router.planSession(ctx, sess, []syncproto.IndexEntry{
			{Path: "fwd.txt", Blake3Hex: blakeHex(content), SizeBytes: int64(len(content)),
				OriginNode: "delta", OriginRun: 7},
		}); err != nil {
			t.Fatalf("planSession refused a legitimate third-party origin: %v", err)
		}
		if _, err := f.router.closeSession(ctx, sess, store.RunStatusSuccess, nil); err != nil {
			t.Fatalf("closeSession: %v", err)
		}
		row, err := f.store.GetByPath(ctx, f.volID, "fwd.txt")
		if err != nil {
			t.Fatalf("GetByPath fwd.txt: %v", err)
		}
		delta, err := f.store.GetNodeByName(ctx, "delta")
		if err != nil {
			t.Fatalf("origin node delta not created: %v", err)
		}
		if !row.OriginNodeID.Valid || row.OriginNodeID.Int64 != delta.ID {
			t.Fatalf("OriginNodeID = %+v, want delta's row %d", row.OriginNodeID, delta.ID)
		}
		if !row.OriginRunID.Valid || row.OriginRunID.Int64 != 7 {
			t.Fatalf("OriginRunID = %+v, want 7 (origin run space, untranslated)", row.OriginRunID)
		}
	})
}

// TestPreStageTransferPreservesOutOfBandFile (#106a): a regular file
// dropped at a Transfer destination out-of-band (no live index row) must
// be moved into .squirrel-history/run-<id>/ before the rclone pass
// overwrites it, since node syncs run without --backup-dir. The destination
// is freed for the incoming bytes and the prior bytes stay recoverable.
func TestPreStageTransferPreservesOutOfBandFile(t *testing.T) {
	ctx := context.Background()
	f := newPreStageFixture(t)

	outOfBand := []byte("bytes a web app dropped in, never indexed")
	incoming := []byte("the initiator's incoming content")
	abs := filepath.Join(f.vol.Path, "drop.bin")
	if err := os.WriteFile(abs, outOfBand, 0o644); err != nil {
		t.Fatalf("write out-of-band file: %v", err)
	}

	sess := f.newSession()
	plan, err := f.router.planSession(ctx, sess, []syncproto.IndexEntry{
		{Path: "drop.bin", Blake3Hex: blakeHex(incoming), SizeBytes: int64(len(incoming))},
	})
	if err != nil {
		t.Fatalf("planSession: %v", err)
	}
	if got := sess.dispositions["drop.bin"].disposition; got != syncproto.DispositionTransfer {
		t.Fatalf("disposition = %q, want transfer", got)
	}
	if len(plan.Conflicts) != 0 {
		t.Fatalf("conflicts = %d, want 0 (a plain out-of-band file is history, not a conflict)", len(plan.Conflicts))
	}

	// The destination is now free for rclone to deliver the incoming bytes.
	if _, err := os.Lstat(abs); !os.IsNotExist(err) {
		t.Fatalf("Lstat drop.bin err = %v, want the destination cleared for the transfer", err)
	}

	// The prior bytes are preserved verbatim under this run's history dir.
	histPath := filepath.Join(f.vol.Path, HistoryDirName, "run-"+strconv.FormatInt(f.recvRun, 10), "drop.bin")
	got, err := os.ReadFile(histPath)
	if err != nil {
		t.Fatalf("read preserved history file: %v", err)
	}
	if string(got) != string(outOfBand) {
		t.Fatalf("preserved bytes = %q, want the out-of-band bytes", got)
	}
}

// TestValidateRelPathRejectsAllReservedDirs (#106b): the receiver's wire
// path allow-list must reject all four reserved sync directories, matching
// the initiator-side filter. A path under .squirrel-restore-history or
// .squirrel-index could otherwise let a peer overwrite the receiver's only
// pre-restore backup or its index ride-along.
func TestValidateRelPathRejectsAllReservedDirs(t *testing.T) {
	reserved := []string{
		HistoryDirName + "/run-1/x",
		ConflictsDirName + "/run-1/x",
		RestoreHistoryDirName + "/run-1/x",
		IndexDirName + "/index.db",
		RestoreHistoryDirName,
		IndexDirName,
	}
	for _, p := range reserved {
		t.Run(p, func(t *testing.T) {
			if err := validateRelPath(p); err == nil {
				t.Fatalf("validateRelPath(%q) = nil, want a reserved-dir rejection", p)
			}
			if err := validateFolderPath(p); err == nil {
				t.Fatalf("validateFolderPath(%q) = nil, want a reserved-dir rejection", p)
			}
		})
	}
	if err := validateRelPath("photos/2024/img.jpg"); err != nil {
		t.Fatalf("validateRelPath rejected an ordinary path: %v", err)
	}
}

// TestSessionBoundToCaller (#110a): a phase call presenting a caller
// identity that differs from the node that opened the session is refused.
// This exercises the lookup binding directly; the empty-caller case is
// the shared-token path (#110d), which carries no identity and so reaches
// the session unbound. The end-to-end binding over per-peer tokens is
// covered by TestPeerTokenSessionBinding.
func TestSessionBoundToCaller(t *testing.T) {
	f := newPreStageFixture(t)
	r := f.router
	r.storeSession(&peerSession{
		receiverRunID:     f.recvRun,
		volume:            f.vol,
		volumeID:          f.volID,
		peerNodeID:        f.peerID,
		initiatorNodeName: "owner",
		dispositions:      make(map[string]*sessionEntry),
	})

	if _, _, err := r.lookupSession(f.recvRun, "intruder"); err == nil {
		t.Fatalf("lookupSession bound to a foreign caller, want %v", errSessionCallerMismatch)
	}
	if sess, ok, err := r.lookupSession(f.recvRun, "owner"); err != nil || !ok || sess == nil {
		t.Fatalf("lookupSession for the owning caller = (%v, %v, %v), want the session", sess, ok, err)
	}
	if sess, ok, err := r.lookupSession(f.recvRun, ""); err != nil || !ok || sess == nil {
		t.Fatalf("lookupSession with no caller identity = (%v, %v, %v), want the session (pre-#110d)", sess, ok, err)
	}
	// A foreign caller must not be able to take (and thereby abort) the
	// session: it stays in place for the legitimate owner.
	if _, _, err := r.takeSession(f.recvRun, "intruder"); err == nil {
		t.Fatalf("takeSession bound to a foreign caller, want %v", errSessionCallerMismatch)
	}
	if sess, ok, err := r.takeSession(f.recvRun, "owner"); err != nil || !ok || sess == nil {
		t.Fatalf("takeSession for the owning caller = (%v, %v, %v), want the session removed", sess, ok, err)
	}
}

// TestPlanRejectsOversizedBody (#110c): /plan wraps the request body in
// http.MaxBytesReader, so a body past the cap is refused with 400 before
// it can be buffered into memory — a token-holding peer can't OOM the
// agent with one huge body. A separate len(Entries) cap guards against an
// entry count that stays within the byte ceiling.
func TestPlanRejectsOversizedBody(t *testing.T) {
	vol := &config.Volume{Name: "pics", Path: t.TempDir()}
	srv := newTestServer(t, Config{Volumes: map[string]*config.Volume{vol.Name: vol}})

	t.Run("body over the byte cap", func(t *testing.T) {
		prev := maxPlanBodyBytes
		maxPlanBodyBytes = 64
		defer func() { maxPlanBodyBytes = prev }()

		body := append([]byte(`{"receiver_run_id":1,"entries":[`), bytes.Repeat([]byte(" "), 256)...)
		body = append(body, ']', '}')
		if code := postRaw(t, srv, "/v1/sync/plan", body); code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for an over-cap body", code)
		}
	})

	t.Run("entry count over the cap", func(t *testing.T) {
		prev := maxPlanEntries
		maxPlanEntries = 1
		defer func() { maxPlanEntries = prev }()

		req := syncproto.PlanRequest{
			ReceiverRunID: 1,
			Entries: []syncproto.IndexEntry{
				{Path: "a.txt", Blake3Hex: blakeHex([]byte("a"))},
				{Path: "b.txt", Blake3Hex: blakeHex([]byte("b"))},
			},
		}
		encoded, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if code := postRaw(t, srv, "/v1/sync/plan", encoded); code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for an over-cap entry count", code)
		}
	})
}

// TestPeerEndpointIgnoresWireEndpoint (#110b): the receiver derives the
// peer-row endpoint from the node name alone, never the unauthenticated
// InitiatorEndpoint, so a peer cannot bind an arbitrary dial-back URL at
// /begin. A real endpoint is bound only by operator config on the
// initiator side.
func TestPeerEndpointIgnoresWireEndpoint(t *testing.T) {
	got := peerEndpoint(syncproto.BeginRequest{
		InitiatorNodeName: "owner",
		InitiatorEndpoint: "https://attacker.example:8443",
	})
	if got != "peer://owner" {
		t.Fatalf("peerEndpoint = %q, want the name-derived placeholder peer://owner", got)
	}
}

// TestPeerTokenSessionBinding (#110a + safe #110d increment): with
// per-peer tokens configured, a session opened by one peer's token is
// bound to that peer's identity. A phase call carrying the same
// receiver_run_id but a different peer's token is refused with 403, so a
// second token-holder can't hijack (or /close-abort) the session. A
// shared-token caller still authenticates but, carrying no identity,
// reaches the session unbound (the pre-#110d behaviour).
func TestPeerTokenSessionBinding(t *testing.T) {
	vol := &config.Volume{Name: "pics", Path: t.TempDir()}
	srv := newTestServer(t, Config{
		Token:      "shared",
		Volumes:    map[string]*config.Volume{vol.Name: vol},
		PeerTokens: map[string]string{"owner-token": "owner", "intruder-token": "intruder"},
	})

	var begin syncproto.BeginResponse
	if code := postJSON(t, srv, "/v1/sync/begin", "owner-token", syncproto.BeginRequest{
		Volume: vol.Name, InitiatorNodeName: "owner", InitiatorRunID: 1,
	}, &begin); code != http.StatusOK {
		t.Fatalf("begin as owner: status = %d, want 200", code)
	}

	verify := syncproto.VerifyRequest{ReceiverRunID: begin.ReceiverRunID}
	if code := postJSON(t, srv, "/v1/sync/verify", "intruder-token", verify, nil); code != http.StatusForbidden {
		t.Fatalf("verify as intruder: status = %d, want 403", code)
	}
	if code := postJSON(t, srv, "/v1/sync/verify", "owner-token", verify, nil); code != http.StatusOK {
		t.Fatalf("verify as owner: status = %d, want 200", code)
	}
}

// TestBeginRejectsImpersonatedNodeName (safe #110d increment): a caller
// authenticated as one node may not open a session declaring a different
// initiator_node_name, so the declared identity is bound to the
// credential rather than self-asserted.
func TestBeginRejectsImpersonatedNodeName(t *testing.T) {
	vol := &config.Volume{Name: "pics", Path: t.TempDir()}
	srv := newTestServer(t, Config{
		Token:      "shared",
		Volumes:    map[string]*config.Volume{vol.Name: vol},
		PeerTokens: map[string]string{"owner-token": "owner"},
	})
	code := postJSON(t, srv, "/v1/sync/begin", "owner-token", syncproto.BeginRequest{
		Volume: vol.Name, InitiatorNodeName: "someone-else", InitiatorRunID: 1,
	}, nil)
	if code != http.StatusForbidden {
		t.Fatalf("begin impersonating someone-else: status = %d, want 403", code)
	}
}

// postJSON marshals body, POSTs it to urlPath with the given bearer
// token, decodes a 200 response into out (when non-nil), and returns the
// status so a test can assert auth/binding rejections.
func postJSON(t *testing.T, srv *Server, urlPath, token string, body, out any) int {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, urlPath, bytes.NewReader(encoded))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK && out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return rec.Code
}

// postRaw POSTs body verbatim to urlPath with the test bearer token and
// returns the HTTP status, so a malformed or oversized body can be driven
// without the typed marshal helpers rejecting it first.
func postRaw(t *testing.T, srv *Server, urlPath string, body []byte) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, urlPath, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Code
}

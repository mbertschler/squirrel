package agent

import (
	"context"
	"encoding/hex"
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
	router  *peerSyncRouter
	store   *store.Store
	vol     *config.Volume
	volID   int64
	peerID  int64
	peerRun int64
	recvRun int64
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
	if err := srv.store.UpsertPeerSyncState(ctx, v.ID, peer.ID, correlated); err != nil {
		t.Fatalf("UpsertPeerSyncState: %v", err)
	}
	recvRun, err := srv.store.BeginPeerSyncRun(ctx, v.ID, peer.ID, correlated+1, "peerA")
	if err != nil {
		t.Fatalf("BeginPeerSyncRun (receiver): %v", err)
	}
	return &preStageFixture{
		router:  srv.router,
		store:   srv.store,
		vol:     vol,
		volID:   v.ID,
		peerID:  peer.ID,
		peerRun: peerRun,
		recvRun: recvRun,
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

	// Seed a present row with NO provenance (local write) so classify
	// returns Conflict rather than Supersede.
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
		FirstSeenRunID: f.peerRun,
		LastSeenRunID:  f.peerRun,
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

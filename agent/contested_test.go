package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/syncproto"
	"github.com/zeebo/blake3"
)

// seedLocalWrite writes content to path and records a present index row
// attributed to a *local* index run (no peer linkage), so a divergent
// initiator hash classifies as a conflict — the precondition for a freeze.
func (f *preStageFixture) seedLocalWrite(t *testing.T, ctx context.Context, path string, content []byte) []byte {
	t.Helper()
	abs := filepath.Join(f.vol.Path, path)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(abs, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	sum := blake3.Sum256(content)
	if err := f.store.Upsert(ctx, store.FileRow{
		VolumeID: f.volID, Path: path, Blake3: sum[:], SizeBytes: int64(len(content)),
		MtimeNs: store.NowNs(), Status: store.StatusPresent,
		FirstSeenRunID: f.localRun, LastSeenRunID: f.localRun, IndexedAtNs: store.NowNs(),
	}, nil); err != nil {
		t.Fatalf("Upsert %s: %v", path, err)
	}
	return sum[:]
}

// freshSession opens another receiver run and returns a session bound to
// it at the given protocol version, so a test can drive successive /plan
// exchanges the way successive cadence ticks would.
func (f *preStageFixture) freshSession(t *testing.T, protocol int) *peerSession {
	t.Helper()
	run, err := f.store.BeginPeerSyncRun(context.Background(), f.volID, f.peerID, 7, "peerA")
	if err != nil {
		t.Fatalf("BeginPeerSyncRun: %v", err)
	}
	sess := f.newSession()
	sess.receiverRunID = run
	sess.protocolVersion = protocol
	return sess
}

// countConflictRunDirs returns how many run-<id>/ subdirectories exist
// under .squirrel-conflicts — one per distinct conflict-preserving run, so
// the F27 unbounded-growth regression shows up as this number climbing.
func countConflictRunDirs(t *testing.T, volPath string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(volPath, ConflictsDirName))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read conflicts dir: %v", err)
	}
	return len(entries)
}

// TestContestedFreezeStopsPingPong is the #158 acceptance at the /plan
// level: the first conflict preserves the loser and freezes the path; a
// later divergent re-assertion is refused with `contested` (no second
// copy minted); and the frozen winner re-asserting is still allowed
// through so a dropped /close can re-land it.
func TestContestedFreezeStopsPingPong(t *testing.T) {
	ctx := context.Background()
	f := newPreStageFixture(t)

	contentX := []byte("homepc's version, indexed locally on the hub")
	contentZ := []byte("laptop's divergent version — wins the first conflict")
	contentW := []byte("homepc re-asserts yet another edit")
	f.seedLocalWrite(t, ctx, "doc.md", contentX)

	// Tick 1: laptop's divergent bytes conflict with the local write.
	sess1 := f.freshSession(t, syncproto.ProtocolVersionContested)
	plan1, err := f.router.planSession(ctx, sess1, []syncproto.IndexEntry{
		{Path: "doc.md", Blake3Hex: blakeHex(contentZ), SizeBytes: int64(len(contentZ))},
	})
	if err != nil {
		t.Fatalf("planSession tick 1: %v", err)
	}
	if d := sess1.dispositions["doc.md"].disposition; d != syncproto.DispositionConflict {
		t.Fatalf("tick 1 disposition = %q, want conflict", d)
	}
	if len(plan1.Conflicts) != 1 {
		t.Fatalf("tick 1 conflicts = %d, want 1", len(plan1.Conflicts))
	}
	if got := countConflictRunDirs(t, f.vol.Path); got != 1 {
		t.Fatalf("conflict run dirs after tick 1 = %d, want 1", got)
	}
	// The path is now frozen with laptop's bytes as the winner.
	latch, err := f.store.GetContestedPath(ctx, f.volID, "doc.md")
	if err != nil {
		t.Fatalf("GetContestedPath: %v", err)
	}
	if blakeHex(contentZ) != hexOrEmpty(latch.LiveBlake3) {
		t.Fatalf("latch winner = %s, want hash(Z) %s", hexOrEmpty(latch.LiveBlake3), blakeHex(contentZ))
	}

	// Tick 2: homepc re-asserts a third version. The freeze refuses it —
	// no bytes move, no second copy is minted (the F27 fix).
	sess2 := f.freshSession(t, syncproto.ProtocolVersionContested)
	plan2, err := f.router.planSession(ctx, sess2, []syncproto.IndexEntry{
		{Path: "doc.md", Blake3Hex: blakeHex(contentW), SizeBytes: int64(len(contentW))},
	})
	if err != nil {
		t.Fatalf("planSession tick 2: %v", err)
	}
	if d := sess2.dispositions["doc.md"].disposition; d != syncproto.DispositionContested {
		t.Fatalf("tick 2 disposition = %q, want contested", d)
	}
	if len(plan2.Conflicts) != 0 {
		t.Fatalf("tick 2 conflicts = %d, want 0 (frozen, not re-conflicted)", len(plan2.Conflicts))
	}
	if len(plan2.Contested) != 1 || plan2.Contested[0].Path != "doc.md" {
		t.Fatalf("tick 2 contested = %+v, want one doc.md entry", plan2.Contested)
	}
	if plan2.Contested[0].PreservedAtPath == "" || plan2.Contested[0].LiveBlake3Hex != blakeHex(contentZ) {
		t.Fatalf("tick 2 contested detail = %+v, want winner Z + preserved path", plan2.Contested[0])
	}
	if got := countConflictRunDirs(t, f.vol.Path); got != 1 {
		t.Fatalf("conflict run dirs after tick 2 = %d, want 1 (no new copy minted)", got)
	}

	// Tick 3: the frozen winner re-asserts its own bytes — allowed through
	// (Transfer), so a /close that never committed can re-land the winner.
	sess3 := f.freshSession(t, syncproto.ProtocolVersionContested)
	if _, err := f.router.planSession(ctx, sess3, []syncproto.IndexEntry{
		{Path: "doc.md", Blake3Hex: blakeHex(contentZ), SizeBytes: int64(len(contentZ))},
	}); err != nil {
		t.Fatalf("planSession tick 3: %v", err)
	}
	if d := sess3.dispositions["doc.md"].disposition; d != syncproto.DispositionTransfer {
		t.Fatalf("tick 3 disposition = %q, want transfer (winner re-asserting)", d)
	}
}

// TestContestedFreezeVersionGated confirms the disposition is gated: on
// one and the same frozen state, a session negotiated below
// ProtocolVersionContested falls back to the legacy `conflict` it
// understands, while a contested-capable session gets the freeze. Older
// peers stay safe.
func TestContestedFreezeVersionGated(t *testing.T) {
	ctx := context.Background()
	f := newPreStageFixture(t)
	contentX := f.seedLocalWrite(t, ctx, "doc.md", []byte("hub-local content"))

	// Freeze doc.md while its local-write bytes stay live (the winner), so
	// both sessions below classify against an identical present row.
	if err := f.store.RaiseContested(ctx, store.ContestedPath{
		VolumeID: f.volID, Path: "doc.md", LiveBlake3: contentX, RaisedRunID: f.localRun,
	}); err != nil {
		t.Fatalf("RaiseContested: %v", err)
	}
	divergent := blakeHex([]byte("a divergent edit"))

	// An older initiator (Merkle-walk protocol) must get the legacy
	// conflict, not the contested disposition it can't interpret.
	legacy := f.freshSession(t, syncproto.ProtocolVersionMerkleWalk)
	plan, err := f.router.planSession(ctx, legacy, []syncproto.IndexEntry{
		{Path: "doc.md", Blake3Hex: divergent, SizeBytes: 16},
	})
	if err != nil {
		t.Fatalf("planSession legacy: %v", err)
	}
	if d := legacy.dispositions["doc.md"].disposition; d != syncproto.DispositionConflict {
		t.Fatalf("legacy disposition = %q, want conflict (version gate)", d)
	}
	if len(plan.Contested) != 0 {
		t.Fatalf("legacy plan.Contested = %+v, want empty for an older peer", plan.Contested)
	}

	// A contested-capable initiator, same frozen state, gets refused.
	modern := f.freshSession(t, syncproto.ProtocolVersionContested)
	if _, err := f.router.planSession(ctx, modern, []syncproto.IndexEntry{
		{Path: "doc.md", Blake3Hex: divergent, SizeBytes: 16},
	}); err != nil {
		t.Fatalf("planSession modern: %v", err)
	}
	if d := modern.dispositions["doc.md"].disposition; d != syncproto.DispositionContested {
		t.Fatalf("modern disposition = %q, want contested (version gate)", d)
	}
}

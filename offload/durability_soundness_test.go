package offload

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

// seedVerifiedComponent records only the vector component (content-
// verified, blake3) for a target, without the freshness push run —
// isolating the freshness condition.
func seedVerifiedComponent(t *testing.T, s *store.Store, volumeID int64, target string, nodeID, run int64) {
	t.Helper()
	if err := s.UpsertDestinationRunIDVerified(context.Background(), volumeID, target, nodeID, run, store.VerifyMethodBlake3, false); err != nil {
		t.Fatalf("UpsertDestinationRunIDVerified(%s): %v", target, err)
	}
}

// TestOffloadFreshnessRefusesReacquiredFile is the headline #115 fix: a
// path deleted and re-acquired after the last whole-volume push is held
// on disk — the origin vector covers its content, but the freshness
// watermark (last successful push in local run space) is behind the run
// in which the path became present again. A fresh push then clears it.
func TestOffloadFreshnessRefusesReacquiredFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	ctx := context.Background()
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	self := selfNode(t, s)

	// A full verified push at index time: vector covers the content and
	// the freshness watermark sits at this push.
	seedVector(t, s, v.ID, "t1", self.ID, idx.RunID)

	// The user deletes the file on disk, an index run flips it missing,
	// then the file is restored and re-indexed — reviving the row with a
	// fresh status_changed_run_id past the last push. The origin vector
	// is unchanged (same content, same origin run), so only the freshness
	// condition can catch this.
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	indexVolume(t, s, root) // flips a.txt -> missing
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	reacquired := indexVolume(t, s, root) // revives a.txt -> present

	row := rowAt(t, s, v.ID, "a.txt")
	if !row.StatusChangedRunID.Valid || row.StatusChangedRunID.Int64 != reacquired.RunID {
		t.Fatalf("status_changed_run_id = %v, want revive run %d", row.StatusChangedRunID, reacquired.RunID)
	}

	rep, err := Offload(ctx, s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	res := oneResult(t, rep, "a.txt", OutcomeNotDurable)
	if len(res.Reasons) != 1 || !strings.Contains(res.Reasons[0], "not freshly pushed") {
		t.Fatalf("reasons = %v, want one freshness failure", res.Reasons)
	}
	mustExist(t, filepath.Join(root, "a.txt"))

	// A fresh whole-volume push now covers the re-acquired path.
	recordPush(t, s, v.ID, "t1")
	rep, err = Offload(ctx, s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload after fresh push: %v", err)
	}
	oneResult(t, rep, "a.txt", OutcomeOffloaded)
	mustBeGone(t, filepath.Join(root, "a.txt"))
}

// TestOffloadFreshnessRefusesUnpushedTarget: a target with a covering
// vector component but neither a local whole-volume push nor pulled
// push-freshness evidence never gates. With no local push the target is
// treated as relayed, and with no freshness evidence the relayed branch
// refuses — the safe direction. This also covers the #103 over-advance
// windows: a row indexed mid-push leaves a target whose freshness can't
// reach it.
func TestOffloadFreshnessRefusesUnpushedTarget(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	self := selfNode(t, s)

	// Vector covers the content (e.g. a stale advance slipped through),
	// but no push run and no freshness evidence exist.
	seedVerifiedComponent(t, s, v.ID, "t1", self.ID, idx.RunID)

	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	res := oneResult(t, rep, "a.txt", OutcomeNotDurable)
	if len(res.Reasons) != 1 || !strings.Contains(res.Reasons[0], "no whole-volume push freshness") {
		t.Fatalf("reasons = %v, want a no-freshness-evidence failure", res.Reasons)
	}
	mustExist(t, filepath.Join(root, "a.txt"))
}

// seedRelayedFreshness records pulled origin-space push-freshness for a
// relayed target (one the local node never pushes to): the coordinate the
// peer durability pull merges from the pushing node's most recent
// whole-volume push.
func seedRelayedFreshness(t *testing.T, s *store.Store, volumeID int64, target string, nodeID, run int64) {
	t.Helper()
	if err := s.MergeDestinationPushFreshness(context.Background(), volumeID, target, nodeID, run); err != nil {
		t.Fatalf("MergeDestinationPushFreshness(%s): %v", target, err)
	}
}

// TestOffloadPeerRelayedTargetGatesOnPulledFreshness is the anti-wedge
// proof. A peer-relayed target — named in offload_requires but never
// pushed to by this node, so its local whole-volume push watermark is
// always 0 — must NOT wedge into a permanent no-op. It offloads when the
// pulled origin-space push-freshness covers the content's origin run, and
// refuses when the freshness is below it or absent. Without the pulled
// freshness coordinate every file on the offsite tier would fail
// freshness forever — exactly the workflow the feature exists for.
func TestOffloadPeerRelayedTargetGatesOnPulledFreshness(t *testing.T) {
	const target = "remote-archive"

	t.Run("passes when freshness covers origin", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "a.txt"), "alpha")
		s := setupStore(t)
		idx := indexVolume(t, s, root)
		v := testVolume(t, s)
		self := selfNode(t, s)

		// Pulled vector + pulled freshness both cover the content's origin
		// run; there is no local push to this target (watermark 0).
		seedVerifiedComponent(t, s, v.ID, target, self.ID, idx.RunID)
		seedRelayedFreshness(t, s, v.ID, target, self.ID, idx.RunID)

		rep, err := Offload(context.Background(), s, root, Options{
			Name: volName, Paths: []string{"."}, Require: []string{target},
		})
		if err != nil {
			t.Fatalf("Offload: %v", err)
		}
		oneResult(t, rep, "a.txt", OutcomeOffloaded)
		mustBeGone(t, filepath.Join(root, "a.txt"))
	})

	t.Run("refuses when freshness is below origin", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "a.txt"), "alpha")
		s := setupStore(t)
		idx := indexVolume(t, s, root)
		v := testVolume(t, s)
		self := selfNode(t, s)

		seedVerifiedComponent(t, s, v.ID, target, self.ID, idx.RunID)
		// Freshness predates the content's origin run: the pushing node's
		// latest whole-volume push did not cover it.
		seedRelayedFreshness(t, s, v.ID, target, self.ID, idx.RunID-1)

		rep, err := Offload(context.Background(), s, root, Options{
			Name: volName, Paths: []string{"."}, Require: []string{target},
		})
		if err != nil {
			t.Fatalf("Offload: %v", err)
		}
		res := oneResult(t, rep, "a.txt", OutcomeNotDurable)
		if len(res.Reasons) != 1 || !strings.Contains(res.Reasons[0], "push freshness") {
			t.Fatalf("reasons = %v, want a stale-freshness failure", res.Reasons)
		}
		mustExist(t, filepath.Join(root, "a.txt"))
	})

	t.Run("refuses when no freshness evidence", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "a.txt"), "alpha")
		s := setupStore(t)
		idx := indexVolume(t, s, root)
		v := testVolume(t, s)
		self := selfNode(t, s)

		// Vector covers the content but no freshness was ever pulled.
		seedVerifiedComponent(t, s, v.ID, target, self.ID, idx.RunID)

		rep, err := Offload(context.Background(), s, root, Options{
			Name: volName, Paths: []string{"."}, Require: []string{target},
		})
		if err != nil {
			t.Fatalf("Offload: %v", err)
		}
		res := oneResult(t, rep, "a.txt", OutcomeNotDurable)
		if len(res.Reasons) != 1 || !strings.Contains(res.Reasons[0], "no whole-volume push freshness") {
			t.Fatalf("reasons = %v, want a no-freshness-evidence failure", res.Reasons)
		}
		mustExist(t, filepath.Join(root, "a.txt"))
	})
}

// TestOffloadLocalPushTargetIgnoresRelayedFreshness: a target this node
// pushes to directly still gates on the local-run-space watermark, not on
// pulled push-freshness. A local push watermark behind the path's
// became-present run refuses even when relayed freshness would cover it —
// the local determination wins for a locally-pushed target.
func TestOffloadLocalPushTargetIgnoresRelayedFreshness(t *testing.T) {
	const target = "t1"
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	self := selfNode(t, s)

	seedVerifiedComponent(t, s, v.ID, target, self.ID, idx.RunID)
	// Relayed freshness would cover the origin, but this node pushes to
	// the target directly, so the gate uses the local watermark instead.
	seedRelayedFreshness(t, s, v.ID, target, self.ID, idx.RunID)

	// A local push exists but it predates the re-acquisition of the path.
	recordPush(t, s, v.ID, target)
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	indexVolume(t, s, root)
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	indexVolume(t, s, root) // re-acquire past the local push

	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{target},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	res := oneResult(t, rep, "a.txt", OutcomeNotDurable)
	if len(res.Reasons) != 1 || !strings.Contains(res.Reasons[0], "last whole-volume push run") {
		t.Fatalf("reasons = %v, want a local-push freshness failure", res.Reasons)
	}
	mustExist(t, filepath.Join(root, "a.txt"))
}

// TestOffloadPresenceSizeHeldOutUntilFingerprint is the #109 fix: a
// content-addressed component advanced with the presence+size method
// does not gate on its own; once a verified scan-back fingerprint backs
// the object (remote_objects.checksum + verified_at_ns), it does.
func TestOffloadPresenceSizeHeldOutUntilFingerprint(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	ctx := context.Background()
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	self := selfNode(t, s)

	// Whole-volume push + freshness watermark satisfied, but the
	// component is presence+size only (crypt offsite, no content hash).
	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "offsite", self.ID, idx.RunID, store.VerifyMethodPresenceSize, false); err != nil {
		t.Fatalf("UpsertDestinationRunIDVerified: %v", err)
	}
	recordPush(t, s, v.ID, "offsite")

	row := rowAt(t, s, v.ID, "a.txt")
	if err := s.InsertRemoteObject(ctx, store.RemoteObject{
		ContentID:     row.ContentID,
		Destination:   "offsite",
		UploadedRunID: idx.RunID,
	}); err != nil {
		t.Fatalf("InsertRemoteObject: %v", err)
	}

	rep, err := Offload(ctx, s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"offsite"},
	})
	if err != nil {
		t.Fatalf("Offload (pending fingerprint): %v", err)
	}
	res := oneResult(t, rep, "a.txt", OutcomeNotDurable)
	if len(res.Reasons) != 1 || !strings.Contains(res.Reasons[0], "not content-verified") {
		t.Fatalf("reasons = %v, want a not-content-verified failure", res.Reasons)
	}
	mustExist(t, filepath.Join(root, "a.txt"))

	// The scan-back pass records a fingerprint and confirms it: now the
	// presence+size component gates.
	if err := s.SetRemoteObjectChecksum(ctx, row.ContentID, "offsite", "sftp-sha256", "deadbeef"); err != nil {
		t.Fatalf("SetRemoteObjectChecksum: %v", err)
	}
	if err := s.MarkRemoteObjectVerified(ctx, row.ContentID, "offsite", store.NowNs()); err != nil {
		t.Fatalf("MarkRemoteObjectVerified: %v", err)
	}

	rep, err = Offload(ctx, s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"offsite"},
	})
	if err != nil {
		t.Fatalf("Offload (verified fingerprint): %v", err)
	}
	oneResult(t, rep, "a.txt", OutcomeOffloaded)
	mustBeGone(t, filepath.Join(root, "a.txt"))
}

// TestOffloadPresenceSizeUnverifiedFingerprintHeldOut: a recorded but
// not-yet-verified fingerprint (checksum present, verified_at_ns NULL)
// is not enough — the gate requires the re-read confirmation, not just
// the upload-time record.
func TestOffloadPresenceSizeUnverifiedFingerprintHeldOut(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	ctx := context.Background()
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	self := selfNode(t, s)

	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "offsite", self.ID, idx.RunID, store.VerifyMethodPresenceSize, false); err != nil {
		t.Fatalf("UpsertDestinationRunIDVerified: %v", err)
	}
	recordPush(t, s, v.ID, "offsite")
	row := rowAt(t, s, v.ID, "a.txt")
	if err := s.InsertRemoteObject(ctx, store.RemoteObject{
		ContentID:     row.ContentID,
		Destination:   "offsite",
		UploadedRunID: idx.RunID,
	}); err != nil {
		t.Fatalf("InsertRemoteObject: %v", err)
	}
	if err := s.SetRemoteObjectChecksum(ctx, row.ContentID, "offsite", "sftp-sha256", "deadbeef"); err != nil {
		t.Fatalf("SetRemoteObjectChecksum: %v", err)
	}

	rep, err := Offload(ctx, s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"offsite"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	oneResult(t, rep, "a.txt", OutcomeNotDurable)
	mustExist(t, filepath.Join(root, "a.txt"))
}

// TestOffloadGateNamesPeerProvenance: a stale component a durability pull
// asserted names the asserting peer in the gate's refusal — the audit
// trail answers "which evidence (and from which peer) gated this offload"
// (the residual of #104). A locally-verified component reads "locally
// verified" instead.
func TestOffloadGateNamesPeerProvenance(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	ctx := context.Background()
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	self := selfNode(t, s)

	peer, err := s.GetOrCreateOriginNode(ctx, "nas")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode(nas): %v", err)
	}
	if err := s.UpsertDestinationRunIDPulled(ctx, v.ID, "t1", self.ID, idx.RunID-1, store.VerifyMethodBlake3, peer.ID, store.NowNs(), false); err != nil {
		t.Fatalf("UpsertDestinationRunIDPulled: %v", err)
	}
	recordPush(t, s, v.ID, "t1")

	rep, err := Offload(ctx, s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	res := oneResult(t, rep, "a.txt", OutcomeNotDurable)
	if len(res.Reasons) != 1 || !strings.Contains(res.Reasons[0], "asserted by peer nas") {
		t.Fatalf("reasons = %v, want the stale failure naming the asserting peer nas", res.Reasons)
	}
	mustExist(t, filepath.Join(root, "a.txt"))
}

// TestOffloadContentVerifiedMethodsGate: blake3, peer-blake3, and
// kopia-verify components each gate on their own (no fingerprint needed)
// once the vector and freshness conditions hold — the stricter gate does
// not refuse legitimately content-verified copies.
func TestOffloadContentVerifiedMethodsGate(t *testing.T) {
	for _, method := range []string{store.VerifyMethodBlake3, store.VerifyMethodPeer, store.VerifyMethodKopia} {
		t.Run(method, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "a.txt"), "alpha")
			s := setupStore(t)
			ctx := context.Background()
			idx := indexVolume(t, s, root)
			v := testVolume(t, s)
			self := selfNode(t, s)

			if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "t1", self.ID, idx.RunID, method, false); err != nil {
				t.Fatalf("UpsertDestinationRunIDVerified: %v", err)
			}
			recordPush(t, s, v.ID, "t1")

			rep, err := Offload(ctx, s, root, Options{
				Name: volName, Paths: []string{"."}, Require: []string{"t1"},
			})
			if err != nil {
				t.Fatalf("Offload: %v", err)
			}
			oneResult(t, rep, "a.txt", OutcomeOffloaded)
			mustBeGone(t, filepath.Join(root, "a.txt"))
		})
	}
}

// TestOffloadDurableFileStillPasses is the anti-wedge guard: a file with
// a fresh whole-volume push, a content-verified method, and a vector
// that covers its origin still offloads — the stricter gate refuses more
// only where durability is actually in question.
func TestOffloadDurableFileStillPasses(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	writeFile(t, filepath.Join(root, "sub", "b.txt"), "bravo")
	s := setupStore(t)
	ctx := context.Background()
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	self := selfNode(t, s)
	seedVector(t, s, v.ID, "t1", self.ID, idx.RunID)
	seedVector(t, s, v.ID, "t2", self.ID, idx.RunID)

	rep, err := Offload(ctx, s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1", "t2"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if rep.Offloaded != 2 || rep.NotDurable != 0 {
		t.Fatalf("report = %+v, want 2 offloaded 0 not-durable", rep)
	}
	oneResult(t, rep, "a.txt", OutcomeOffloaded)
	oneResult(t, rep, "sub/b.txt", OutcomeOffloaded)
}

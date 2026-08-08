package offload

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedNow is an arbitrary, well-past-epoch wall clock the staleness tests
// reason against so evidence ages are exact and deterministic.
var fixedNow = time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC).UnixNano()

const maxAge = 30 * 24 * time.Hour

func verifiedAt(ns int64) sql.NullInt64 { return sql.NullInt64{Int64: ns, Valid: true} }

// staleGate builds a gate with no store, populated only with the fields
// staleEvidenceFailure reads — the injected now, the max age, and a
// node-name cache for provenance rendering. The pure staleness logic needs
// no database.
func staleGate(maxEvidenceAge time.Duration, names map[int64]string) *gate {
	return &gate{nowNs: fixedNow, maxEvidenceAge: maxEvidenceAge, nodeNames: names}
}

// TestStaleEvidenceRefusesOffload: a locally-verified component last
// verified longer ago than the configured max age is refused, naming the
// age and provenance — even though its version-vector coverage is sound.
func TestStaleEvidenceRefusesOffload(t *testing.T) {
	g := staleGate(maxAge, nil)
	comp := component{coveredRun: 5, method: "blake3", verifiedAt: verifiedAt(fixedNow - int64(90*24*time.Hour))}
	f, refused := g.staleEvidenceFailure(context.Background(), "t1", comp)
	if !refused || f.Kind != FailureEvidenceStale || !strings.Contains(f.Summary, "too old") {
		t.Fatalf("failure = %+v (refused %v), want a stale-evidence refusal", f, refused)
	}
	if !strings.Contains(f.Detail, "locally verified") || !strings.Contains(f.Detail, "limit 720h0m0s") {
		t.Fatalf("detail = %q, want the age, the configured limit, and local provenance", f.Detail)
	}
}

// TestFreshEvidencePasses: a component verified within the max age clears
// the policy, and the policy is a no-op when disabled (zero max age) even
// for evidence verified far in the past.
func TestFreshEvidencePasses(t *testing.T) {
	g := staleGate(maxAge, nil)
	fresh := component{coveredRun: 5, method: "blake3", verifiedAt: verifiedAt(fixedNow - int64(24*time.Hour))}
	if f, refused := g.staleEvidenceFailure(context.Background(), "t1", fresh); refused {
		t.Fatalf("failure = %+v, want none for fresh evidence", f)
	}

	disabled := staleGate(0, nil)
	ancient := component{coveredRun: 5, method: "blake3", verifiedAt: verifiedAt(fixedNow - int64(365*24*time.Hour))}
	if f, refused := disabled.staleEvidenceFailure(context.Background(), "t1", ancient); refused {
		t.Fatalf("failure = %+v, want none when the staleness policy is disabled", f)
	}
}

// TestNullVerifiedAtRefusesUnderMaxAge: a component whose verified_at_ns is
// unknown (a pre-v23 row, or one only ever advanced methodlessly at its
// recorded run) is fail-closed — refused as never re-verified when a max
// age is set, but unaffected when the policy is disabled.
func TestNullVerifiedAtRefusesUnderMaxAge(t *testing.T) {
	comp := component{coveredRun: 5, method: "blake3"}
	f, refused := staleGate(maxAge, nil).staleEvidenceFailure(context.Background(), "t1", comp)
	if !refused || !strings.Contains(f.Summary, "evidence age unknown") ||
		!strings.Contains(f.Detail, "no verification timestamp") {
		t.Fatalf("failure = %+v (refused %v), want a never-re-verified refusal", f, refused)
	}
	if f, refused := staleGate(0, nil).staleEvidenceFailure(context.Background(), "t1", comp); refused {
		t.Fatalf("failure = %+v, want none when the staleness policy is disabled", f)
	}
}

// TestPeerRelayedEvidenceStaleness: a peer-asserted component's
// verified_at_ns records the responder's own verification instant (relayed
// on the pull, capped at now). Evidence within the max age is trusted;
// evidence older than it ages out — whether because the destination went
// dead behind a still-answering peer or the peer itself fell silent — and
// the refusal names the asserting peer, not the local node.
func TestPeerRelayedEvidenceStaleness(t *testing.T) {
	const peerID = int64(42)
	g := staleGate(maxAge, map[int64]string{peerID: "nas"})
	source := sql.NullInt64{Int64: peerID, Valid: true}

	recent := component{coveredRun: 5, method: "blake3", source: source, verifiedAt: verifiedAt(fixedNow - int64(24*time.Hour))}
	if f, refused := g.staleEvidenceFailure(context.Background(), "t1", recent); refused {
		t.Fatalf("failure = %+v, want none for a recently pulled peer assertion", f)
	}

	silent := component{coveredRun: 5, method: "blake3", source: source, verifiedAt: verifiedAt(fixedNow - int64(90*24*time.Hour))}
	f, refused := g.staleEvidenceFailure(context.Background(), "t1", silent)
	if !refused || f.Kind != FailureEvidenceStale || !strings.Contains(f.Detail, "asserted by peer nas") {
		t.Fatalf("failure = %+v (refused %v), want a stale-evidence refusal naming peer nas", f, refused)
	}
}

// TestOffloadEndToEndFreshEvidencePasses drives the full Offload path with
// a configured max age: a freshly verified component (seedVector stamps
// verified_at_ns at the write, moments before the gate's now) is within
// any sane max age, so the file is deleted. This proves the
// Options→gate wiring carries the knob and a freshly pushed copy is not
// collateral of the new policy.
func TestOffloadEndToEndFreshEvidencePasses(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	self := selfNode(t, s)
	seedVector(t, s, v.ID, "t1", self.ID, idx.RunID)

	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"},
		MaxEvidenceAge: maxAge,
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	oneResult(t, rep, "a.txt", OutcomeOffloaded)
	mustBeGone(t, filepath.Join(root, "a.txt"))
}

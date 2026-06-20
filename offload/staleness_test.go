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
	reason := g.staleEvidenceFailure(context.Background(), "t1", comp)
	if !strings.Contains(reason, "stale evidence") || !strings.Contains(reason, "locally verified") {
		t.Fatalf("reason = %q, want a stale-evidence refusal naming local provenance", reason)
	}
	if !strings.Contains(reason, "max age 720h0m0s") {
		t.Fatalf("reason = %q, want it to name the configured max age", reason)
	}
}

// TestFreshEvidencePasses: a component verified within the max age clears
// the policy, and the policy is a no-op when disabled (zero max age) even
// for evidence verified far in the past.
func TestFreshEvidencePasses(t *testing.T) {
	g := staleGate(maxAge, nil)
	fresh := component{coveredRun: 5, method: "blake3", verifiedAt: verifiedAt(fixedNow - int64(24*time.Hour))}
	if reason := g.staleEvidenceFailure(context.Background(), "t1", fresh); reason != "" {
		t.Fatalf("reason = %q, want none for fresh evidence", reason)
	}

	disabled := staleGate(0, nil)
	ancient := component{coveredRun: 5, method: "blake3", verifiedAt: verifiedAt(fixedNow - int64(365*24*time.Hour))}
	if reason := disabled.staleEvidenceFailure(context.Background(), "t1", ancient); reason != "" {
		t.Fatalf("reason = %q, want none when the staleness policy is disabled", reason)
	}
}

// TestNullVerifiedAtRefusesUnderMaxAge: a component whose verified_at_ns is
// unknown (a pre-v23 row, or one only ever advanced methodlessly at its
// recorded run) is fail-closed — refused as never re-verified when a max
// age is set, but unaffected when the policy is disabled.
func TestNullVerifiedAtRefusesUnderMaxAge(t *testing.T) {
	comp := component{coveredRun: 5, method: "blake3"}
	if reason := staleGate(maxAge, nil).staleEvidenceFailure(context.Background(), "t1", comp); !strings.Contains(reason, "never re-verified") {
		t.Fatalf("reason = %q, want a never-re-verified refusal", reason)
	}
	if reason := staleGate(0, nil).staleEvidenceFailure(context.Background(), "t1", comp); reason != "" {
		t.Fatalf("reason = %q, want none when the staleness policy is disabled", reason)
	}
}

// TestPeerRelayedEvidenceStaleness: a peer-asserted component's
// verified_at_ns records when this node last pulled a fresh assertion. A
// recent pull is trusted; a peer gone silent past the max age ages out,
// and the refusal names the asserting peer, not the local node.
func TestPeerRelayedEvidenceStaleness(t *testing.T) {
	const peerID = int64(42)
	g := staleGate(maxAge, map[int64]string{peerID: "nas"})
	source := sql.NullInt64{Int64: peerID, Valid: true}

	recent := component{coveredRun: 5, method: "blake3", source: source, verifiedAt: verifiedAt(fixedNow - int64(24*time.Hour))}
	if reason := g.staleEvidenceFailure(context.Background(), "t1", recent); reason != "" {
		t.Fatalf("reason = %q, want none for a recently pulled peer assertion", reason)
	}

	silent := component{coveredRun: 5, method: "blake3", source: source, verifiedAt: verifiedAt(fixedNow - int64(90*24*time.Hour))}
	reason := g.staleEvidenceFailure(context.Background(), "t1", silent)
	if !strings.Contains(reason, "stale evidence") || !strings.Contains(reason, "asserted by peer nas") {
		t.Fatalf("reason = %q, want a stale-evidence refusal naming peer nas", reason)
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

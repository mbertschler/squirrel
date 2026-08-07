package offload

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// TestOffloadIncapableTargetAbortsUpFront: a required target that can never
// satisfy the gate (a mirror-layout crypt destination — size+mtime forever,
// no fingerprint path) aborts the whole offload before any run row opens or
// any candidate is walked, with a message naming the target and the reason.
// This is the fail-fast surface: without it every file would report
// not-durable forever with no hint the policy itself is the problem.
func TestOffloadIncapableTargetAbortsUpFront(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	indexVolume(t, s, root)
	runsBefore := countRuns(t, s)

	dests := map[string]*config.Destination{
		"backup": {Name: "backup", Type: "sftp", Layout: config.LayoutMirror, Crypt: &config.Crypt{Password: "obscured"}},
	}
	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"backup"}, RequireDests: dests,
	})
	if err == nil {
		t.Fatalf("Offload succeeded, want fail-fast; report = %+v", rep)
	}
	if !strings.Contains(err.Error(), `offload_requires target "backup" can never satisfy the durability gate`) {
		t.Fatalf("err = %v, want it to name target backup and the gate", err)
	}
	if !strings.Contains(err.Error(), "crypt") {
		t.Fatalf("err = %v, want it to name the crypt overlay as the reason", err)
	}
	if len(rep.Results) != 0 || rep.RunID != 0 {
		t.Fatalf("report = %+v, want empty (no candidates walked, no run opened)", rep)
	}
	if got := countRuns(t, s); got != runsBefore {
		t.Fatalf("runs = %d, want unchanged %d (no offload run opened)", got, runsBefore)
	}
	mustExist(t, filepath.Join(root, "a.txt"))
}

// TestCheckTargetsCanGateSkipsNilAndAbsent: a nil value in the dests map
// (a partially-constructed map) is treated like an absent key — skipped,
// left to the per-file gate — never a preflight nil-dereference crash.
func TestCheckTargetsCanGateSkipsNilAndAbsent(t *testing.T) {
	dests := map[string]*config.Destination{"backup": nil}
	if err := checkTargetsCanGate([]string{"backup", "missing"}, dests, nil); err != nil {
		t.Fatalf("checkTargetsCanGate with nil/absent entries = %v, want nil (skipped)", err)
	}
}

// TestCheckTargetsCanGateRelayed exercises the peer-relayed half of the
// pre-check: a relayed target the owning peer reports incapable aborts and
// names target/reason/peer; a relayed target reported capable, or one with
// no verdict at all (peer unreachable / predates the field), never aborts —
// it falls through to the per-file gate.
func TestCheckTargetsCanGateRelayed(t *testing.T) {
	require := []string{"s3archive"}

	// Incapable relayed verdict → abort naming target, reason, and peer.
	incapable := []RelayedTargetCapability{{
		Target: "s3archive", Peer: "nas", CanGate: false,
		Reason: "shallow path-mirrored crypt destination",
	}}
	err := checkTargetsCanGate(require, nil, incapable)
	if err == nil {
		t.Fatal("checkTargetsCanGate with an incapable relayed target = nil, want abort")
	}
	for _, want := range []string{`"s3archive"`, "shallow path-mirrored crypt destination", `peer "nas"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}

	// Capable relayed verdict → no abort (pending is fine).
	capable := []RelayedTargetCapability{{Target: "s3archive", Peer: "nas", CanGate: true}}
	if err := checkTargetsCanGate(require, nil, capable); err != nil {
		t.Fatalf("capable relayed target = %v, want nil (walks per file)", err)
	}

	// No verdict at all → no abort (best-effort fallback).
	if err := checkTargetsCanGate(require, nil, nil); err != nil {
		t.Fatalf("absent relayed capability = %v, want nil (per-file fallback)", err)
	}
}

// TestOffloadRelayedIncapableTargetAbortsUpFront: a peer-relayed required
// target the owning peer reports incapable aborts the whole offload before
// any run row opens, with a message naming the target, the reason, and the
// owning peer — the peer-relayed subset of the #121 fail-fast (#145).
func TestOffloadRelayedIncapableTargetAbortsUpFront(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	indexVolume(t, s, root)
	runsBefore := countRuns(t, s)

	relayed := []RelayedTargetCapability{{
		Target: "cloudbox", Peer: "nas", CanGate: false,
		Reason: "shallow path-mirrored crypt destination",
	}}
	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"cloudbox"}, RelayedCaps: relayed,
	})
	if err == nil {
		t.Fatalf("Offload succeeded, want fail-fast; report = %+v", rep)
	}
	for _, want := range []string{`"cloudbox"`, "shallow path-mirrored crypt destination", `peer "nas"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}
	if len(rep.Results) != 0 || rep.RunID != 0 {
		t.Fatalf("report = %+v, want empty (no candidates walked, no run opened)", rep)
	}
	if got := countRuns(t, s); got != runsBefore {
		t.Fatalf("runs = %d, want unchanged %d (no offload run opened)", got, runsBefore)
	}
	mustExist(t, filepath.Join(root, "a.txt"))
}

// TestOffloadRelayedCapablePendingStillWalks: a peer-relayed required target
// the owning peer reports capable but whose durability is not yet recorded
// must NOT fail fast — a capable-but-pending relayed target walks per file
// and reports not-durable, exactly as a locally-configured capable target.
func TestOffloadRelayedCapablePendingStillWalks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	indexVolume(t, s, root)

	relayed := []RelayedTargetCapability{{Target: "s3archive", Peer: "nas", CanGate: true}}
	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"s3archive"}, RelayedCaps: relayed,
	})
	if err != nil {
		t.Fatalf("Offload: %v (a capable-but-pending relayed target must walk, not fail fast)", err)
	}
	if rep.Offloaded != 0 || rep.NotDurable != 1 || rep.Errors != 0 {
		t.Fatalf("report = %+v, want one not-durable result", rep)
	}
	if rep.RunID == 0 {
		t.Fatal("RunID = 0, want a real offload run (the walk happened)")
	}
	mustExist(t, filepath.Join(root, "a.txt"))
}

// TestOffloadCapableTargetPendingStillWalks: a required target that is
// structurally capable (a plain mirror destination) but whose durability
// evidence is not yet recorded must NOT fail fast — supplying its config to
// the pre-check does not turn a genuinely-pending state into an abort. The
// offload walks per file and reports OutcomeNotDurable, exactly as before
// this feature.
func TestOffloadCapableTargetPendingStillWalks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	indexVolume(t, s, root)
	v := testVolume(t, s)
	self := selfNode(t, s)

	dests := map[string]*config.Destination{
		"t1": {Name: "t1", Type: "sftp", Layout: config.LayoutMirror},
	}
	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"}, RequireDests: dests,
	})
	if err != nil {
		t.Fatalf("Offload: %v (a capable-but-pending target must walk, not fail fast)", err)
	}
	if rep.Offloaded != 0 || rep.NotDurable != 1 || rep.Errors != 0 {
		t.Fatalf("report = %+v, want one not-durable result", rep)
	}
	res := oneResult(t, rep, "a.txt", OutcomeNotDurable)
	f := oneFailure(t, res, "t1", FailureNoEvidence)
	if !strings.Contains(f.Detail, "no component for origin "+self.Name) {
		t.Fatalf("detail = %q, want the per-file no-evidence refusal naming origin %s", f.Detail, self.Name)
	}
	if rep.RunID == 0 {
		t.Fatal("RunID = 0, want a real offload run (the walk happened)")
	}
	mustExist(t, filepath.Join(root, "a.txt"))
	if row := rowAt(t, s, v.ID, "a.txt"); row.Status != store.StatusPresent {
		t.Fatalf("status = %q, want present", row.Status)
	}
}

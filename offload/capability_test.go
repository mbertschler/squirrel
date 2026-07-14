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
	if err := checkTargetsCanGate([]string{"backup", "missing"}, dests); err != nil {
		t.Fatalf("checkTargetsCanGate with nil/absent entries = %v, want nil (skipped)", err)
	}
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
	if len(res.Reasons) != 1 || !strings.Contains(res.Reasons[0], "t1: missing component for origin "+self.Name) {
		t.Fatalf("reasons = %v, want the per-file missing-component failure", res.Reasons)
	}
	if rep.RunID == 0 {
		t.Fatal("RunID = 0, want a real offload run (the walk happened)")
	}
	mustExist(t, filepath.Join(root, "a.txt"))
	if row := rowAt(t, s, v.ID, "a.txt"); row.Status != store.StatusPresent {
		t.Fatalf("status = %q, want present", row.Status)
	}
}

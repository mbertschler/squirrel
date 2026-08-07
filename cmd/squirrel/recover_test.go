package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/sync"
)

// recoverConfig writes a config with one volume syncing to a local
// destination and one peer, which is the smallest shape `recover` has an
// opinion about: something to restore, somewhere to restore it from, and
// somebody to re-pair with.
func recoverConfig(t *testing.T) (cfgPath, destRoot string) {
	t.Helper()
	dir := t.TempDir()
	photos := filepath.Join(dir, "photos")
	dest := filepath.Join(dir, "dest")
	peer := filepath.Join(dir, "peer-mount")
	for _, d := range []string{photos, dest, peer} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	body := "" +
		"node_name = \"thisnode\"\n\n" +
		"[volumes.photos]\npath = \"" + photos + "\"\nsync_to = [\"scratch\"]\n\n" +
		"[destinations.scratch]\ntype = \"local\"\nroot = \"" + dest + "\"\n\n" +
		"[nodes.peer]\nendpoint = \"https://peer.home:8443\"\npath = \"" + peer + "\"\n" +
		"[nodes.peer.auth]\nbearer = \"tok\"\n"
	return writeCheckConfig(t, body), dest
}

// seedSnapshots plants ride-along snapshot files where the destination's
// per-volume .squirrel-index/ directory lives, which is what discovery
// reads. Contents do not matter: nothing in the discovery path opens them.
func seedSnapshots(t *testing.T, destRoot, volume string, names ...string) {
	t.Helper()
	dir := filepath.Join(destRoot, volume, ".squirrel-index")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		writeTestFile(t, filepath.Join(dir, n), "not-a-real-db")
	}
}

// TestRecoverRequiresDestination: the flow cannot start without knowing
// where to look, and the error names the candidates rather than leaving an
// operator mid-disaster to grep their own config.
func TestRecoverRequiresDestination(t *testing.T) {
	cfgPath, _ := recoverConfig(t)
	out, err := runCLIExpectErr(t, "--config", cfgPath, "recover")
	joined := out + err.Error()
	if !strings.Contains(joined, "--from is required") || !strings.Contains(joined, "scratch") {
		t.Fatalf("error should demand --from and list the destinations:\n%s", joined)
	}
}

// TestRecoverRejectsPeerNode: `restore` refuses node destinations and so
// does this, pointing at the reverse-push path instead of failing opaquely.
func TestRecoverRejectsPeerNode(t *testing.T) {
	cfgPath, _ := recoverConfig(t)
	out, err := runCLIExpectErr(t, "--config", cfgPath, "recover", "--from", "peer")
	joined := out + err.Error()
	if !strings.Contains(joined, "peer node") || !strings.Contains(joined, "reverse peer push") {
		t.Fatalf("error should redirect to the reverse-push path:\n%s", joined)
	}
}

// TestRecoverWithoutSnapshotsExplains: a destination holding bytes but no
// catalog is a real state, and the message has to say what is still
// possible rather than only what failed.
func TestRecoverWithoutSnapshotsExplains(t *testing.T) {
	requireRcloneCLI(t) // discovery lists the destination through rclone
	cfgPath, _ := recoverConfig(t)
	out, err := runCLIExpectErr(t, "--config", cfgPath, "recover", "--from", "scratch")
	joined := out + err.Error()
	if !strings.Contains(joined, "no index snapshots found") {
		t.Fatalf("want a no-snapshots explanation:\n%s", joined)
	}
	if !strings.Contains(joined, "squirrel restore") {
		t.Fatalf("message should still name the by-hand path that remains open:\n%s", joined)
	}
}

// TestRecoverDryRunTouchesNothing is the safety property that matters most:
// the default invocation reports the whole plan and stops. An operator who
// runs `squirrel recover --from X` to find out what is there must not
// discover they have started a recovery.
func TestRecoverDryRunTouchesNothing(t *testing.T) {
	requireRcloneCLI(t) // discovery lists the destination through rclone
	cfgPath, destRoot := recoverConfig(t)
	newest := "index-20260807T120000.000Z-run-42.db"
	seedSnapshots(t, destRoot, "photos", "index-20260101T090000.000Z-run-7.db", newest)

	out := runCLI(t, "--config", cfgPath, "recover", "--from", "scratch")

	for _, want := range []string{
		"phase 1", "phase 2", "phase 3",
		newest,
		"Nothing has been touched",
		"--execute",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output missing %q:\n%s", want, out)
		}
	}
	// The newest snapshot is the default choice and is marked as such.
	if !strings.Contains(out, "> photos  "+newest) {
		t.Errorf("newest snapshot should be marked as the chosen one:\n%s", out)
	}
}

// TestRecoverRejectsUnknownSnapshot: naming a snapshot that is not there
// stops before any fetch, and says how to see the real list.
func TestRecoverRejectsUnknownSnapshot(t *testing.T) {
	requireRcloneCLI(t) // discovery lists the destination through rclone
	cfgPath, destRoot := recoverConfig(t)
	seedSnapshots(t, destRoot, "photos", "index-20260807T120000.000Z-run-42.db")

	out, err := runCLIExpectErr(t, "--config", cfgPath, "recover", "--from", "scratch",
		"--snapshot", "index-nope.db")
	joined := out + err.Error()
	if !strings.Contains(joined, "not found") || !strings.Contains(joined, "--snapshot") {
		t.Fatalf("want a not-found error that points at the listing:\n%s", joined)
	}
}

// TestChooseSnapshotPrefersNewest pins the default that the whole flow
// leans on. Discovery sorts newest-first, so "the newest" is the head —
// but an explicit request always wins over it.
func TestChooseSnapshotPrefersNewest(t *testing.T) {
	older := sync.IndexSnapshot{Volume: "photos", Name: "old.db", TakenAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	newer := sync.IndexSnapshot{Volume: "photos", Name: "new.db", TakenAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	snaps := []sync.IndexSnapshot{newer, older}

	got, err := chooseSnapshot(snaps, "", "scratch")
	if err != nil {
		t.Fatalf("chooseSnapshot: %v", err)
	}
	if got.Name != "new.db" {
		t.Errorf("default = %q, want the newest", got.Name)
	}
	if got, err = chooseSnapshot(snaps, "old.db", "scratch"); err != nil || got.Name != "old.db" {
		t.Errorf("explicit choice = %q (%v), want old.db", got.Name, err)
	}
}

// TestSnapshotAgeUnknownIsSaid: a filename that carries no timestamp must
// not render as fresh. In a recovery, "how old is this catalog" is the
// question the operator is actually asking.
func TestSnapshotAgeUnknownIsSaid(t *testing.T) {
	got := snapshotAge(sync.IndexSnapshot{Name: "hand-copied.db"}, time.Now())
	if !strings.Contains(got, "unknown") {
		t.Errorf("age of an unparsed name = %q, want it to say so", got)
	}
}

// TestRecoverDeclinedPhaseStopsTheSequence is the regression guard for the
// worst failure this command could have: a declined confirmation that lets
// the sequence carry on. Continuing past a "no" on phase 1 would restore
// volumes against an index that was never installed, and would print
// "recovery complete" over work the operator explicitly refused.
//
// Stdin is empty, so confirmPhase reads EOF and treats it as a refusal —
// which is itself the property worth pinning: absence of an answer is not
// consent.
func TestRecoverDeclinedPhaseStopsTheSequence(t *testing.T) {
	requireRcloneCLI(t) // discovery lists the destination through rclone
	cfgPath, destRoot := recoverConfig(t)
	seedSnapshots(t, destRoot, "photos", "index-20260807T120000.000Z-run-42.db")

	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetIn(strings.NewReader("n\n"))
	root.SetArgs([]string{"--config", cfgPath, "recover", "--from", "scratch", "--execute"})
	if err := root.Execute(); err != nil {
		t.Fatalf("declining a phase should not be an error: %v\n%s", err, buf.String())
	}

	out := buf.String()
	if strings.Contains(out, "recovery complete") {
		t.Errorf("declined phase 1 still claimed completion:\n%s", out)
	}
	if strings.Contains(out, "phase 2:") || strings.Contains(out, "restoring ") {
		t.Errorf("declined phase 1 still went on to phase 2:\n%s", out)
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("declining should say the sequence stopped:\n%s", out)
	}
}

// TestConfirmPhaseTreatsAnythingButYesAsNo pins the half of the
// stop-the-sequence property that needs no rclone: what counts as consent.
// EOF is the case that matters most — a recovery driven from a script with
// no stdin must stop, not proceed on an unanswered question.
func TestConfirmPhaseTreatsAnythingButYesAsNo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stdin string
		want  bool
	}{
		{"yes", "yes\n", true},
		{"y", "y\n", true},
		{"Y uppercase", "Y\n", true},
		{"y with spaces", "  y  \n", true},
		{"no", "n\n", false},
		{"empty line", "\n", false},
		{"EOF, nothing at all", "", false},
		{"unrelated word", "maybe\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetIn(strings.NewReader(tc.stdin))
			if got := confirmPhase(cmd, recoverOptions{}, "proceed?"); got != tc.want {
				t.Errorf("confirmPhase(%q) = %v, want %v", tc.stdin, got, tc.want)
			}
		})
	}
}

// TestConfirmPhaseAssumeOK: --yes is the rehearsed-recovery escape hatch and
// must not consult stdin at all.
func TestConfirmPhaseAssumeOK(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetIn(strings.NewReader("n\n"))
	if !confirmPhase(cmd, recoverOptions{AssumeOK: true}, "proceed?") {
		t.Error("--yes should answer yes without reading stdin")
	}
}

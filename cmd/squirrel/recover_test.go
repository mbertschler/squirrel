package main

import (
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

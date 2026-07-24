package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/volmark"
)

// syncFixture holds the bits each end-to-end test sets up: a store, a
// configured rclone wrapper, and a squirrel config with one volume + one
// local destination. The helper exists because every test in this file
// repeats the same scaffold.
type syncFixture struct {
	store *store.Store
	rcl   *Rclone
	cfg   *config.Config
	vol   *config.Volume
	dest  *config.Destination
}

func setupFixture(t *testing.T) *syncFixture {
	t.Helper()
	requireRclone(t)

	root := t.TempDir()
	volPath := filepath.Join(root, "src")
	if err := os.MkdirAll(volPath, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	destPath := filepath.Join(root, "dst")
	if err := os.MkdirAll(destPath, 0o755); err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}
	// Pre-bootstrap the destination's per-volume marker so tests can
	// call Sync without each one passing Init: true. The init flow is
	// exercised separately by TestSyncRefusesUninitialisedDestination
	// and TestSyncInitWritesMarker.
	destVolRoot := filepath.Join(destPath, "pics")
	if err := os.MkdirAll(destVolRoot, 0o755); err != nil {
		t.Fatalf("mkdir dst volume: %v", err)
	}
	if err := volmark.Write(destVolRoot, volmark.Marker{Volume: "pics"}); err != nil {
		t.Fatalf("seed destination marker: %v", err)
	}

	dbPath := filepath.Join(root, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	cfgPath := filepath.Join(root, "config.toml")
	cfgBody := "[destinations.scratch]\ntype = \"local\"\nroot = \"" + destPath + "\"\n\n" +
		"[volumes.pics]\npath = \"" + volPath + "\"\nsync_to = [\"scratch\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	rcl, err := Find()
	if err != nil {
		t.Fatalf("Find rclone: %v", err)
	}
	rcl.Config = filepath.Join(root, "rclone.conf")
	if err := os.WriteFile(rcl.Config, []byte{}, 0o600); err != nil {
		t.Fatalf("seed rclone.conf: %v", err)
	}

	return &syncFixture{
		store: s,
		rcl:   rcl,
		cfg:   cfg,
		vol:   cfg.Volumes["pics"],
		dest:  cfg.Destinations["scratch"],
	}
}

// runIndex is a fixture helper that runs the indexer against the volume so
// the prerequisite-check in Sync is satisfied. Returns the path written
// inside the volume so tests can assert on what should end up at the
// destination after a sync.
func (f *syncFixture) runIndex(t *testing.T) {
	t.Helper()
	opts := index.Options{Name: f.vol.Name}
	if _, err := index.Index(context.Background(), f.store, f.vol.Path, opts); err != nil {
		t.Fatalf("index.Index: %v", err)
	}
}

func TestSyncRequiresIndexedVolume(t *testing.T) {
	f := setupFixture(t)
	_, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{})
	if err == nil || !strings.Contains(err.Error(), "never been indexed") {
		t.Fatalf("expected unindexed-volume error, got %v", err)
	}
}

// TestSyncRefusesOnVolumePathMismatch mirrors the restore and offload
// cross-checks (#114): a DB volumes.path that no longer matches the
// config-declared path makes the push handler refuse, so it cannot push
// one tree while the durability advance covers another.
func TestSyncRefusesOnVolumePathMismatch(t *testing.T) {
	f := setupFixture(t)
	// Seed the volume row with a path that differs from f.vol.Path so the
	// shared requireIndexedVolume gate fails before rclone is invoked.
	staleDir := t.TempDir()
	if _, err := f.store.CreateVolume(context.Background(), f.vol.Name, staleDir); err != nil {
		t.Fatalf("seed stale volume row: %v", err)
	}
	_, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{})
	if err == nil || !strings.Contains(err.Error(), "resolve the conflict") {
		t.Fatalf("expected path-mismatch error, got %v", err)
	}
}

func TestSyncHappyPath(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.vol.Path, "b.txt"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)

	rep, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{})
	if err != nil {
		t.Fatalf("Sync: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success: %+v", rep.Status, rep)
	}
	if rep.RcloneResult.Transferred != 2 {
		t.Fatalf("Transferred = %d, want 2", rep.RcloneResult.Transferred)
	}

	// Destination layout: <dest.root>/<volume>/...
	wantPath := filepath.Join(f.dest.Root, f.vol.Name, "a.txt")
	if got, err := os.ReadFile(wantPath); err != nil {
		t.Fatalf("dest missing a.txt at %s: %v", wantPath, err)
	} else if string(got) != "alpha" {
		t.Fatalf("dest a.txt = %q, want alpha", got)
	}

	// The runs row was inserted as kind=sync with the destination name.
	runs, err := f.store.ListRuns(context.Background(), store.ListRunsOpts{Descending: true, Limit: 5})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	var syncRun *store.Run
	for i := range runs {
		if runs[i].Kind == store.RunKindSync {
			syncRun = &runs[i]
			break
		}
	}
	if syncRun == nil {
		t.Fatalf("no sync run recorded; runs=%+v", runs)
	}
	if !syncRun.Destination.Valid || syncRun.Destination.String != "scratch" {
		t.Fatalf("destination = %+v, want 'scratch'", syncRun.Destination)
	}
	if syncRun.Status != store.RunStatusSuccess {
		t.Fatalf("sync run status = %q, want success", syncRun.Status)
	}
}

// TestSyncIdempotentReruns confirms that re-running sync with no source
// changes produces a no-op rclone call (Transferred=0, Checked>0) and
// inserts a fresh runs row each time — sync calls are first-class events,
// not de-duplicated.
func TestSyncIdempotentReruns(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)

	if _, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{}); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	rep, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{})
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if rep.RcloneResult.Transferred != 0 {
		t.Fatalf("Transferred = %d, want 0 (idempotent)", rep.RcloneResult.Transferred)
	}
	if rep.RcloneResult.Checked < 1 {
		t.Fatalf("Checked = %d, want ≥ 1 (file verified)", rep.RcloneResult.Checked)
	}

	runs, _ := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	syncRuns := 0
	for _, r := range runs {
		if r.Kind == store.RunKindSync {
			syncRuns++
		}
	}
	if syncRuns != 2 {
		t.Fatalf("expected 2 sync runs after two invocations, got %d", syncRuns)
	}
}

// TestSyncOverwriteMovesToHistory: modify a file locally, re-sync, and
// verify the old content lives at <dest>/<volume>/.squirrel-history/run-N/.
// This is the destination-immutability guarantee.
func TestSyncOverwriteMovesToHistory(t *testing.T) {
	f := setupFixture(t)
	target := filepath.Join(f.vol.Path, "a.txt")
	if err := os.WriteFile(target, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)
	if _, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{}); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	if err := os.WriteFile(target, []byte("v2 different"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)
	rep, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{})
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if rep.RcloneResult.Transferred != 1 {
		t.Fatalf("Transferred = %d, want 1 (the modified file)", rep.RcloneResult.Transferred)
	}

	// New content at destination.
	got, _ := os.ReadFile(filepath.Join(f.dest.Root, f.vol.Name, "a.txt"))
	if string(got) != "v2 different" {
		t.Fatalf("dest a.txt = %q, want new content", got)
	}
	// Old content preserved under the volume's history dir for this run.
	histDir := filepath.Join(f.dest.Root, f.vol.Name, HistoryDirName)
	matches, _ := filepath.Glob(filepath.Join(histDir, "run-*", "a.txt"))
	if len(matches) == 0 {
		t.Fatalf("no history copy of a.txt under %s", histDir)
	}
	old, _ := os.ReadFile(matches[0])
	if string(old) != "v1" {
		t.Fatalf("history copy = %q, want 'v1'", old)
	}
}

// TestSyncDoesNotDeleteRemovedFiles: deleting a local file does not remove
// it from the destination. We never pass --delete-* to rclone.
func TestSyncDoesNotDeleteRemovedFiles(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.vol.Path, "b.txt"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)
	if _, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{}); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if err := os.Remove(filepath.Join(f.vol.Path, "b.txt")); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)
	if _, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{}); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dest.Root, f.vol.Name, "b.txt")); err != nil {
		t.Fatalf("destination b.txt was removed; want preserved (additive sync)")
	}
}

// TestSyncFiltersOutHistoryFromSource is the defensive case: even if a
// user manages to put a directory called .squirrel-history in their source
// volume, our filter excludes it from being copied. The destination's own
// .squirrel-history is also invisible to the comparison.
func TestSyncFiltersOutHistoryFromSource(t *testing.T) {
	f := setupFixture(t)
	if err := os.MkdirAll(filepath.Join(f.vol.Path, HistoryDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.vol.Path, HistoryDirName, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)
	if _, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dest.Root, f.vol.Name, HistoryDirName, "secret.txt")); err == nil {
		t.Fatalf("user-source .squirrel-history was copied; should be filtered")
	}
}

// TestSyncDryRunPath confirms --dry-run produces a Report (so the CLI
// can echo what would happen) but never inserts a runs row, never moves
// bytes, and never errors on the source-validation prerequisite.
func TestSyncDryRunPath(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)

	beforeRuns, _ := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	rep, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{DryRun: true})
	if err != nil {
		t.Fatalf("Sync --dry-run: %v", err)
	}
	if rep.RunID != 0 {
		t.Fatalf("dry-run produced RunID = %d, want 0", rep.RunID)
	}
	afterRuns, _ := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	if len(afterRuns) != len(beforeRuns) {
		t.Fatalf("dry-run inserted %d new runs rows, want 0", len(afterRuns)-len(beforeRuns))
	}
	// rclone --dry-run still reports what *would* be transferred via the
	// stats summary, so Transferred can be non-zero; we just verify no
	// physical bytes landed at the destination.
	if _, err := os.Stat(filepath.Join(f.dest.Root, f.vol.Name, "a.txt")); err == nil {
		t.Fatalf("dry-run wrote to destination; want no-op")
	}
	if rep.Verification.Verified() {
		t.Fatalf("dry-run must report an unverified result; got %+v", rep.Verification)
	}
}

// TestSyncHappyPathStampsVerification rides on the happy-path fixture
// to pin the typed durability report a default (BLAKE3) bucket sync
// produces.
func TestSyncHappyPathStampsVerification(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)

	rep, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !rep.Verification.Verified() || rep.Verification.Method != VerifyMethodBlake3 {
		t.Fatalf("Verification = %+v, want verified blake3", rep.Verification)
	}
	if rep.Verification.Files != 1 {
		t.Fatalf("Verification.Files = %d, want 1", rep.Verification.Files)
	}

	shallowRep, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{Shallow: true})
	if err != nil {
		t.Fatalf("shallow Sync: %v", err)
	}
	if shallowRep.Verification.Verified() || shallowRep.Verification.Method != VerifyMethodSizeMtime {
		t.Fatalf("shallow Verification = %+v, want unverified size+mtime", shallowRep.Verification)
	}
}

// TestSyncWarnsAboutHistoryDirInSource exercises the advisory path: a
// user with a literal .squirrel-history dir in their volume gets a
// warning surfaced via Report.Warnings, but the sync proceeds and the
// dir's contents are filtered (not uploaded).
func TestSyncWarnsAboutHistoryDirInSource(t *testing.T) {
	f := setupFixture(t)
	if err := os.MkdirAll(filepath.Join(f.vol.Path, HistoryDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.vol.Path, HistoryDirName, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.vol.Path, "ok.txt"), []byte("yep"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)

	rep, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(rep.Warnings) == 0 {
		t.Fatalf("expected a warning about %s in source; got none", HistoryDirName)
	}
	found := false
	for _, w := range rep.Warnings {
		if strings.Contains(w, HistoryDirName) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("warnings = %v; expected one mentioning %s", rep.Warnings, HistoryDirName)
	}
}

// TestRunPairRefusesWhenAnotherIsRunning is half of the issue #36
// acceptance: while a kind='sync' row for the same (volume,
// destination) is still in 'running', a fresh RunPair refuses with a
// clean error and writes no new run. Pre-inserting the running row
// via BeginRun stands in for an in-flight first invocation that
// hasn't reached FinishRun yet. The race-free side is covered by
// TestRunPairRefusesConcurrentInvocations below.
func TestRunPairRefusesWhenAnotherIsRunning(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)

	v, err := f.store.GetVolumeByName(context.Background(), f.vol.Name)
	if err != nil {
		t.Fatalf("GetVolumeByName: %v", err)
	}
	stuckID, err := f.store.BeginRun(context.Background(), store.RunKindSync, v.ID, f.dest.Name, false)
	if err != nil {
		t.Fatalf("seed running sync row: %v", err)
	}

	beforeRuns, _ := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	p := Pair{Volume: f.vol, Destination: f.dest}
	rep, err := RunPair(context.Background(), f.store, Tools{Rclone: f.rcl}, p, Options{})
	if err == nil {
		t.Fatalf("expected refusal while a run is in flight; got rep=%+v", rep)
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("error = %v, want one mentioning 'already running'", err)
	}
	if want := fmt.Sprintf("run=%d", stuckID); !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want substring %q", err, want)
	}
	afterRuns, _ := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	if len(afterRuns) != len(beforeRuns) {
		t.Fatalf("refused RunPair inserted %d new runs rows, want 0", len(afterRuns)-len(beforeRuns))
	}

	// Clearing the in-flight row unblocks a fresh invocation.
	if err := f.store.FinishRun(context.Background(), stuckID, store.RunStatusFailed, "test cleanup", 0); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if _, err := RunPair(context.Background(), f.store, Tools{Rclone: f.rcl}, p, Options{}); err != nil {
		t.Fatalf("RunPair after clearing stuck row: %v", err)
	}
}

// TestRunPairRefusesConcurrentInvocations is the other half of #36's
// acceptance: two RunPair calls launched in parallel against the same
// (volume, destination) must serialise — exactly one inserts a sync
// row and succeeds, the other refuses with the "already running"
// diagnostic. The atomic BEGIN IMMEDIATE in
// store.BeginSyncRunIfClear closes the check-then-act window that an
// app-level guard would leave open.
func TestRunPairRefusesConcurrentInvocations(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)

	const parallel = 6
	p := Pair{Volume: f.vol, Destination: f.dest}

	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		successes   int
		refusals    int
		otherErrors []error
	)
	start := make(chan struct{})
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := RunPair(context.Background(), f.store, Tools{Rclone: f.rcl}, p, Options{})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case strings.Contains(err.Error(), "already running"):
				refusals++
			default:
				otherErrors = append(otherErrors, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(otherErrors) > 0 {
		t.Fatalf("unexpected errors from concurrent RunPair: %v", otherErrors)
	}
	if successes != 1 || refusals != parallel-1 {
		t.Fatalf("got successes=%d refusals=%d, want 1 success and %d refusals",
			successes, refusals, parallel-1)
	}

	// Exactly one sync run row should exist in any terminal state.
	runs, err := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	syncRuns := 0
	for _, r := range runs {
		if r.Kind == store.RunKindSync {
			syncRuns++
			if r.Status == store.RunStatusRunning {
				t.Fatalf("sync run %d still in status=running after wg.Wait()", r.ID)
			}
		}
	}
	if syncRuns != 1 {
		t.Fatalf("sync runs recorded = %d, want exactly 1", syncRuns)
	}
}

// TestCheckMinVersionBranches covers the three branches of the
// version-floor gate without needing a stubbed rclone binary.
func TestCheckMinVersionBranches(t *testing.T) {
	v := func(maj, min, pat int) Version {
		return Version{Major: maj, Minor: min, Patch: pat,
			Raw: fmt.Sprintf("rclone v%d.%d.%d", maj, min, pat)}
	}
	cases := []struct {
		name     string
		version  Version
		shallow  bool
		wantErr  bool
		wantWarn bool
	}{
		{"at floor, !shallow", v(1, 66, 0), false, false, false},
		{"above floor, !shallow", v(1, 80, 0), false, false, false},
		{"below floor, !shallow → error", v(1, 65, 9), false, true, false},
		{"below floor, shallow → warn only", v(1, 65, 9), true, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf strings.Builder
			err := checkMinVersion(c.version, &buf, c.shallow)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if got := strings.Contains(buf.String(), "warning"); got != c.wantWarn {
				t.Fatalf("warning emitted = %v, want %v (out: %q)", got, c.wantWarn, buf.String())
			}
		})
	}
}

// TestRestoreRefusesOnVolumePathMismatch documents the parity with
// index.resolveNamedVolume: a DB row whose volumes.path no longer
// matches what config declares causes restore to refuse rather than
// silently writing the run row against the stale path.
func TestRestoreRefusesOnVolumePathMismatch(t *testing.T) {
	f := setupFixture(t)
	// Bypass index and CreateVolume manually with a path that differs
	// from f.vol.Path. (Both paths exist on disk so the failure mode
	// is unambiguously the mismatch check.)
	staleDir := t.TempDir()
	if _, err := f.store.CreateVolume(context.Background(), f.vol.Name, staleDir); err != nil {
		t.Fatalf("seed stale volume row: %v", err)
	}
	// Seed the marker at f.vol.Path so the marker gate passes; the
	// failure under test is the volume-row/config path mismatch, not
	// the marker check (which is exercised separately).
	if err := volmark.Write(f.vol.Path, volmark.Marker{Volume: f.vol.Name}); err != nil {
		t.Fatal(err)
	}
	_, err := Restore(context.Background(), f.store, f.rcl, f.vol, f.dest, RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "resolve the conflict") {
		t.Fatalf("expected path-mismatch error, got %v", err)
	}
}

func TestPairsForFiltering(t *testing.T) {
	cfgBody := `
[destinations.a]
type = "local"
root = "/a"

[destinations.b]
type = "local"
root = "/b"

[volumes.one]
path = "/v1"
sync_to = ["a", "b"]

[volumes.two]
path = "/v2"
sync_to = ["a"]
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cases := []struct {
		vol, dest string
		want      int
	}{
		{"", "", 3},     // all pairs: one→a, one→b, two→a
		{"one", "", 2},  // both pairs for 'one'
		{"two", "", 1},  // only 'two'→a
		{"", "a", 2},    // every pair targeting destination 'a'
		{"one", "b", 1}, // single specific pair
	}
	for _, c := range cases {
		got, err := PairsFor(cfg, c.vol, c.dest)
		if err != nil {
			t.Fatalf("PairsFor(%q,%q): %v", c.vol, c.dest, err)
		}
		if len(got) != c.want {
			t.Fatalf("PairsFor(%q,%q) returned %d pairs, want %d", c.vol, c.dest, len(got), c.want)
		}
	}

	if _, err := PairsFor(cfg, "ghost", ""); err == nil {
		t.Fatalf("expected unknown-volume error")
	}
	if _, err := PairsFor(cfg, "", "ghost"); err == nil {
		t.Fatalf("expected unknown-destination error")
	}
	if _, err := PairsFor(cfg, "two", "b"); err == nil {
		t.Fatalf("expected no-match error (volume 'two' doesn't sync to 'b')")
	}
}

func TestBuildRcloneArgsRefusesZeroRunIDOutsideDryRun(t *testing.T) {
	vol := &config.Volume{Name: "pics", Path: "/tmp/pics"}
	dest := &config.Destination{Name: "scratch", Type: "local", Root: "/tmp/dst"}

	// runID=0 outside dry-run is the bug shape #C4 guards against:
	// backupDirURI would otherwise collide every overwrite into the
	// run-dry-run/ placeholder bucket.
	if _, err := buildRcloneArgs(vol, dest, 0, Options{DryRun: false}); err == nil {
		t.Fatalf("expected refusal for runID=0 outside dry-run")
	}

	// runID=0 in dry-run is legitimate (the allocator returns 0 for
	// dry-run by design) and must succeed.
	if _, err := buildRcloneArgs(vol, dest, 0, Options{DryRun: true}); err != nil {
		t.Fatalf("dry-run with runID=0 should be allowed, got %v", err)
	}

	// A real runID must always succeed regardless of dry-run.
	if _, err := buildRcloneArgs(vol, dest, 17, Options{DryRun: false}); err != nil {
		t.Fatalf("non-zero runID outside dry-run should be allowed, got %v", err)
	}
}

// cryptFixtureDest returns a remote destination with a crypt overlay, the
// shape the crypt addressing/verification tests share; these tests stop
// at argument construction.
func cryptFixtureDest() *config.Destination {
	return &config.Destination{
		Name:  "offsite",
		Type:  "sftp",
		Root:  "/data",
		Crypt: &config.Crypt{Password: "obscured-pw"},
	}
}

// TestBuildRcloneArgsCryptAddressing pins the two crypt behaviours of the
// sync args builder: transfers and the backup-dir address the overlay
// remote (whose remote line carries the root, so paths are
// volume-relative), and the BLAKE3 flags are dropped because crypt
// remotes expose no content hashes.
func TestBuildRcloneArgsCryptAddressing(t *testing.T) {
	vol := &config.Volume{Name: "pics", Path: "/tmp/pics"}

	args, err := buildRcloneArgs(vol, cryptFixtureDest(), 7, Options{})
	if err != nil {
		t.Fatalf("buildRcloneArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if got := args[len(args)-1]; got != "offsite-crypt:pics/" {
		t.Fatalf("dst arg = %q, want offsite-crypt:pics/", got)
	}
	if !strings.Contains(joined, "--backup-dir offsite-crypt:pics/"+HistoryDirName+"/run-7") {
		t.Fatalf("backup-dir not addressed through the crypt remote: %s", joined)
	}
	if strings.Contains(joined, "--checksum") || strings.Contains(joined, "blake3") {
		t.Fatalf("BLAKE3 flags passed to a crypt destination: %s", joined)
	}

	plain := cryptFixtureDest()
	plain.Crypt = nil
	plainArgs, err := buildRcloneArgs(vol, plain, 7, Options{})
	if err != nil {
		t.Fatalf("buildRcloneArgs (plain): %v", err)
	}
	plainJoined := strings.Join(plainArgs, " ")
	if got := plainArgs[len(plainArgs)-1]; got != "offsite:/data/pics/" {
		t.Fatalf("plain dst arg = %q, want offsite:/data/pics/", got)
	}
	if !strings.Contains(plainJoined, "--checksum --hash blake3") {
		t.Fatalf("plain destination lost its BLAKE3 flags: %s", plainJoined)
	}
}

// TestBuildRestoreArgsCryptAddressing mirrors the sync case for the pull
// direction: the source is the crypt remote and the BLAKE3 flags stay off.
func TestBuildRestoreArgsCryptAddressing(t *testing.T) {
	vol := &config.Volume{Name: "pics", Path: "/tmp/pics"}
	args := buildRestoreArgs(vol, cryptFixtureDest(), 3, RestoreOptions{ToPath: "/tmp/scratch"})
	joined := strings.Join(args, " ")
	if got := args[len(args)-2]; got != "offsite-crypt:pics/" {
		t.Fatalf("src arg = %q, want offsite-crypt:pics/", got)
	}
	if strings.Contains(joined, "--checksum") || strings.Contains(joined, "blake3") {
		t.Fatalf("BLAKE3 flags passed for a crypt source: %s", joined)
	}
}

// TestIndexDirURICrypt: the snapshot ride-along lands inside the encrypted
// tree, addressed through the same overlay as the data transfer.
func TestIndexDirURICrypt(t *testing.T) {
	if got := indexDirURI(cryptFixtureDest(), "pics"); got != "offsite-crypt:pics/"+IndexDirName {
		t.Fatalf("indexDirURI = %q, want offsite-crypt:pics/%s", got, IndexDirName)
	}
}

// TestEffectiveShallowCrypt pins that a crypt destination downgrades a
// non-shallow request (and that the runs row will say so), while a plain
// destination passes the flag through.
func TestEffectiveShallowCrypt(t *testing.T) {
	crypt := cryptFixtureDest()
	plain := cryptFixtureDest()
	plain.Crypt = nil
	cases := []struct {
		dest    *config.Destination
		shallow bool
		want    bool
	}{
		{crypt, false, true},
		{crypt, true, true},
		{plain, false, false},
		{plain, true, true},
	}
	for _, c := range cases {
		if got := EffectiveShallow(c.dest, c.shallow); got != c.want {
			t.Errorf("EffectiveShallow(crypt=%v, shallow=%v) = %v, want %v",
				c.dest.Crypt != nil, c.shallow, got, c.want)
		}
	}
}

// TestShallowForPairs pins the version-preflight scope: only an
// invocation with at least one blake3-verified target (a plain bucket
// or a peer node) requires the full rclone floor.
func TestShallowForPairs(t *testing.T) {
	crypt := cryptFixtureDest()
	plain := cryptFixtureDest()
	plain.Crypt = nil
	node := Pair{Node: &config.Node{Name: "peer"}}
	kopia := Pair{Destination: &config.Destination{Name: "mirror", Type: "kopia", Root: "/tmp/repo"}}
	contentAddressed := Pair{Destination: &config.Destination{Name: "archive", Type: "sftp", Root: "/data", Layout: config.LayoutContentAddressed}}
	cases := []struct {
		name    string
		pairs   []Pair
		shallow bool
		want    bool
	}{
		{"user shallow wins", []Pair{{Destination: plain}}, true, true},
		{"all crypt", []Pair{{Destination: crypt}, {Destination: crypt}}, false, true},
		{"mixed crypt and plain", []Pair{{Destination: crypt}, {Destination: plain}}, false, false},
		{"node target verifies", []Pair{{Destination: crypt}, node}, false, false},
		{"kopia pair puts no constraint on rclone", []Pair{kopia, {Destination: crypt}}, false, true},
		{"kopia beside plain still verifies", []Pair{kopia, {Destination: plain}}, false, false},
		{"content-addressed pair puts no constraint on rclone", []Pair{contentAddressed, {Destination: crypt}}, false, true},
		{"content-addressed beside plain still verifies", []Pair{contentAddressed, {Destination: plain}}, false, false},
		{"no pairs", nil, false, true},
	}
	for _, c := range cases {
		if got := ShallowForPairs(c.pairs, c.shallow); got != c.want {
			t.Errorf("%s: ShallowForPairs = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestCryptVerificationWarning: the advisory fires exactly when the
// fallback is implicit — a crypt destination without --shallow. An
// explicit --shallow run already gets the CLI's shallow warning, and a
// plain destination has nothing to warn about.
func TestCryptVerificationWarning(t *testing.T) {
	crypt := cryptFixtureDest()
	w := cryptVerificationWarning(crypt, false)
	if !strings.Contains(w, "size+mtime") || !strings.Contains(w, crypt.Name) {
		t.Fatalf("warning = %q, want one naming the destination and the size+mtime fallback", w)
	}
	if w := cryptVerificationWarning(crypt, true); w != "" {
		t.Fatalf("warning = %q for an explicit --shallow run, want empty", w)
	}
	plain := cryptFixtureDest()
	plain.Crypt = nil
	if w := cryptVerificationWarning(plain, false); w != "" {
		t.Fatalf("warning = %q for a plain destination, want empty", w)
	}
}

// TestSyncRefusesUninitialisedDestination removes the bootstrap
// marker that setupFixture seeded and confirms Sync refuses to run
// without --init. This is the threat model: a typo in dest.Root
// points us at an unrelated directory and we have no way to know
// that without the marker.
func TestSyncRefusesUninitialisedDestination(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)
	// Remove the seeded marker — simulate "this directory was not
	// claimed by squirrel."
	if err := os.Remove(filepath.Join(f.dest.Root, f.vol.Name, volmark.MarkerName)); err != nil {
		t.Fatalf("remove seeded marker: %v", err)
	}

	rep, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{})
	if err == nil || !strings.Contains(err.Error(), "--init") {
		t.Fatalf("expected --init hint, got %v", err)
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("marker refusal should wrap ErrRefused, got %v", err)
	}
	// The refusal must mint a visible terminal run row, not vanish into
	// the returned error with run_id=0 (#157, F26).
	if rep.RunID == 0 || rep.Status != store.RunStatusRefused {
		t.Fatalf("rep = %+v, want a refused run row", rep)
	}
	run, gerr := f.store.GetRun(context.Background(), rep.RunID)
	if gerr != nil {
		t.Fatalf("GetRun(%d): %v", rep.RunID, gerr)
	}
	if run.Kind != store.RunKindSync || run.Status != store.RunStatusRefused ||
		!run.Error.Valid || !strings.Contains(run.Error.String, "--init") {
		t.Fatalf("run = %+v, want a refused sync run carrying the refusal message", run)
	}
}

// TestSyncInitWritesMarker confirms that passing Init: true creates
// the destination per-volume directory + marker and proceeds with
// the sync.
func TestSyncInitWritesMarker(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)
	// Tear down the entire per-volume destination subtree (incl.
	// marker) so Init must (re-)create both.
	if err := os.RemoveAll(filepath.Join(f.dest.Root, f.vol.Name)); err != nil {
		t.Fatalf("clear dst volume: %v", err)
	}

	rep, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{Init: true})
	if err != nil {
		t.Fatalf("Sync with Init: %v", err)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success", rep.Status)
	}
	m, err := volmark.Read(filepath.Join(f.dest.Root, f.vol.Name))
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if m.Volume != f.vol.Name {
		t.Fatalf("marker volume = %q, want %q", m.Volume, f.vol.Name)
	}
}

// TestSyncRefusesMismatchedDestinationMarker covers the "different
// volume's tree" case: a marker exists but names something else.
// Init is irrelevant here — we must always refuse rather than
// overwrite another volume's identity.
func TestSyncRefusesMismatchedDestinationMarker(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)
	if err := volmark.Write(filepath.Join(f.dest.Root, f.vol.Name), volmark.Marker{Volume: "video"}); err != nil {
		t.Fatalf("seed wrong-volume marker: %v", err)
	}

	for _, init := range []bool{false, true} {
		_, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{Init: init})
		if err == nil || !strings.Contains(err.Error(), `names "video"`) {
			t.Fatalf("Init=%v: expected mismatch error, got %v", init, err)
		}
	}
}

// TestRestoreRefusesMissingLocalMarker exercises the source-side
// guard: restore writes into the local vol.Path and must refuse if
// the marker doesn't name this volume.
func TestRestoreRefusesMissingLocalMarker(t *testing.T) {
	f := setupFixture(t)
	if err := os.Remove(filepath.Join(f.vol.Path, volmark.MarkerName)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear source marker: %v", err)
	}
	_, err := Restore(context.Background(), f.store, f.rcl, f.vol, f.dest, RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "no .squirrel-volume marker") {
		t.Fatalf("expected missing-marker refusal, got %v", err)
	}
}

// TestRestoreToPathSkipsMarkerCheck confirms the scratch-target
// override bypasses the marker requirement.
func TestRestoreToPathSkipsMarkerCheck(t *testing.T) {
	f := setupFixture(t)
	if err := os.Remove(filepath.Join(f.vol.Path, volmark.MarkerName)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear source marker: %v", err)
	}
	scratch := t.TempDir()
	rep, err := Restore(context.Background(), f.store, f.rcl, f.vol, f.dest, RestoreOptions{ToPath: scratch})
	if err != nil {
		t.Fatalf("Restore --to scratch: %v (rep=%+v)", err, rep)
	}
}

// TestRestoreRefusesPopulatedVolumeWithoutInPlace covers issue #61:
// a default-target restore against a vol.Path that contains user
// content must refuse unless --in-place is set. The marker alone
// doesn't count as content, but any other file does.
func TestRestoreRefusesPopulatedVolumeWithoutInPlace(t *testing.T) {
	f := setupFixture(t)
	// Bootstrap the local volume: write the marker and one user file.
	if err := volmark.Write(f.vol.Path, volmark.Marker{Volume: f.vol.Name}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.vol.Path, "local-edit.txt"), []byte("user wrote this"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Restore(context.Background(), f.store, f.rcl, f.vol, f.dest, RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "--in-place") {
		t.Fatalf("expected --in-place refusal, got %v", err)
	}
}

// TestRestoreInPlacePreservesLocalEdits asserts the --backup-dir
// contract end-to-end: an --in-place restore with a hash-mismatched
// local file moves the prior bytes to
// .squirrel-restore-history/run-<id>/<path>/ and lands the
// destination's bytes at the live path.
func TestRestoreInPlacePreservesLocalEdits(t *testing.T) {
	f := setupFixture(t)
	// Seed: a file synced upstream, then a local edit that diverges
	// from the destination.
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("upstream-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)
	if _, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{}); err != nil {
		t.Fatalf("seed Sync: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("local-edit-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := Restore(context.Background(), f.store, f.rcl, f.vol, f.dest, RestoreOptions{InPlace: true})
	if err != nil {
		t.Fatalf("Restore --in-place: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success", rep.Status)
	}

	// Live path holds the destination's bytes (upstream-bytes).
	got, _ := os.ReadFile(filepath.Join(f.vol.Path, "a.txt"))
	if string(got) != "upstream-bytes" {
		t.Fatalf("a.txt = %q, want upstream-bytes", got)
	}
	// Prior local bytes preserved under run-<id>/.
	histRoot := filepath.Join(f.vol.Path, RestoreHistoryDirName)
	matches, _ := filepath.Glob(filepath.Join(histRoot, "run-*", "a.txt"))
	if len(matches) == 0 {
		t.Fatalf("no .squirrel-restore-history/run-*/a.txt under %s", histRoot)
	}
	preserved, _ := os.ReadFile(matches[0])
	if string(preserved) != "local-edit-bytes" {
		t.Fatalf("restore-history copy = %q, want local-edit-bytes", preserved)
	}
}

// TestRestoreToPathSkipsInPlaceCheck confirms --to lets the operator
// restore into any scratch directory without needing --in-place,
// even when that directory is empty or populated.
func TestRestoreToPathSkipsInPlaceCheck(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("upstream-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)
	if _, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{}); err != nil {
		t.Fatalf("seed Sync: %v", err)
	}

	scratch := t.TempDir()
	// Make scratch non-empty to confirm --to bypasses the gate.
	if err := os.WriteFile(filepath.Join(scratch, "preexisting.txt"), []byte("scratch had this"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := Restore(context.Background(), f.store, f.rcl, f.vol, f.dest, RestoreOptions{ToPath: scratch})
	if err != nil {
		t.Fatalf("Restore --to populated scratch: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success", rep.Status)
	}
}

// TestRestoreToPathEqualVolPathTreatedAsInPlace confirms that passing
// --to <vol.Path> goes through the same safety gates as the default
// in-place restore (marker check + non-empty refusal). Without this
// equivalence, `--to <vol.Path>` would silently bypass the very
// protections #61 is meant to enforce.
func TestRestoreToPathEqualVolPathTreatedAsInPlace(t *testing.T) {
	f := setupFixture(t)
	if err := volmark.Write(f.vol.Path, volmark.Marker{Volume: f.vol.Name}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.vol.Path, "local-edit.txt"), []byte("user wrote this"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Restore(context.Background(), f.store, f.rcl, f.vol, f.dest, RestoreOptions{ToPath: f.vol.Path})
	if err == nil || !strings.Contains(err.Error(), "--in-place") {
		t.Fatalf("expected --in-place refusal when --to equals vol.Path, got %v", err)
	}
}

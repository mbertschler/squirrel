package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
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
		{"", "", 3},       // all pairs: one→a, one→b, two→a
		{"one", "", 2},    // both pairs for 'one'
		{"two", "", 1},    // only 'two'→a
		{"", "a", 2},      // every pair targeting destination 'a'
		{"one", "b", 1},   // single specific pair
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

package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
)

// snapshotterFor builds a Snapshotter wired to the fixture's store and
// rclone wrapper, with the local tier pointed at a fresh temp dir.
func (f *syncFixture) snapshotterFor(t *testing.T, cfg SnapshotConfig) *Snapshotter {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	return NewSnapshotter(f.store, f.rcl, cfg)
}

func globOne(t *testing.T, pattern string) string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	if len(matches) != 1 {
		t.Fatalf("glob %s = %d matches, want exactly 1: %v", pattern, len(matches), matches)
	}
	return matches[0]
}

// TestSyncCloudRideAlong is the headline acceptance: after a successful
// destination sync a snapshot lands under
// <dest>/<volume>/.squirrel-index/, is the *global* index.db (carries
// rows for a volume that was never synced to this destination, per
// decision #1), opens cleanly, and a local-tier copy exists alongside.
// The filename carries the producing run id.
func TestSyncCloudRideAlong(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)

	// A second volume indexed into the *same* store but never synced here.
	// Its rows must still appear in the ride-along snapshot, proving the
	// payload is the whole catalog rather than a per-volume slice.
	otherPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(otherPath, "other.txt"), []byte("o"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Index(context.Background(), f.store, otherPath, index.Options{Name: "other"}); err != nil {
		t.Fatalf("index other volume: %v", err)
	}

	localDir := t.TempDir()
	sn := f.snapshotterFor(t, SnapshotConfig{Dir: localDir, Keep: 7, Cloud: true, CloudKeep: 7})
	rep, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{Snapshot: sn})
	if err != nil {
		t.Fatalf("Sync: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success", rep.Status)
	}
	if rep.SnapshotErr != nil {
		t.Fatalf("SnapshotErr = %v, want nil", rep.SnapshotErr)
	}

	// Cloud snapshot present under the per-volume .squirrel-index dir.
	cloudSnap := globOne(t, filepath.Join(f.dest.Root, f.vol.Name, IndexDirName, "index-*-run-*.db"))
	if want := fmt.Sprintf("run-%d.db", rep.RunID); !strings.HasSuffix(cloudSnap, want) {
		t.Fatalf("cloud snapshot %s does not end with %q", cloudSnap, want)
	}
	// Local tier copy present with the same name.
	localSnap := globOne(t, filepath.Join(localDir, "index-*-run-*.db"))
	if filepath.Base(localSnap) != filepath.Base(cloudSnap) {
		t.Fatalf("local %s and cloud %s have different names", filepath.Base(localSnap), filepath.Base(cloudSnap))
	}

	// Opens cleanly and is the global catalog: the never-synced "other"
	// volume's rows are present in the destination-side snapshot.
	snap, err := store.Open(cloudSnap)
	if err != nil {
		t.Fatalf("open cloud snapshot: %v", err)
	}
	defer snap.Close()
	rows, err := snap.IntegrityCheck(context.Background())
	if err != nil {
		t.Fatalf("snapshot integrity check: %v", err)
	}
	if !store.IsIntegrityClean(rows) {
		t.Fatalf("snapshot integrity = %v, want ok", rows)
	}
	other, err := snap.GetVolumeByName(context.Background(), "other")
	if err != nil {
		t.Fatalf("snapshot missing 'other' volume (not a global catalog?): %v", err)
	}
	paths, err := snap.ListPresentPathsUnder(context.Background(), other.ID)
	if err != nil {
		t.Fatalf("list other paths: %v", err)
	}
	if _, ok := paths["other.txt"]; !ok {
		t.Fatalf("snapshot 'other' volume missing other.txt; paths=%v", paths)
	}
}

// TestSyncCloudDisabledKeepsLocalSnapshot: with Cloud=false a local
// snapshot is still taken, but nothing is uploaded to the destination.
func TestSyncCloudDisabledKeepsLocalSnapshot(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)

	localDir := t.TempDir()
	sn := f.snapshotterFor(t, SnapshotConfig{Dir: localDir, Keep: 7, Cloud: false, CloudKeep: 7})
	rep, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{Snapshot: sn})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if rep.SnapshotErr != nil {
		t.Fatalf("SnapshotErr = %v, want nil", rep.SnapshotErr)
	}
	if _, err := os.Stat(filepath.Join(localDir)); err != nil {
		t.Fatalf("local backups dir missing: %v", err)
	}
	globOne(t, filepath.Join(localDir, "index-*-run-*.db"))
	if _, err := os.Stat(filepath.Join(f.dest.Root, f.vol.Name, IndexDirName)); !os.IsNotExist(err) {
		t.Fatalf("cloud .squirrel-index exists with cloud=false (err=%v)", err)
	}
}

// TestSyncNoSnapshotterIsNoOp: a nil Options.Snapshot (feature disabled,
// e.g. [backups] enabled=false) takes no snapshot at all.
func TestSyncNoSnapshotterIsNoOp(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)

	rep, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if rep.SnapshotErr != nil {
		t.Fatalf("SnapshotErr = %v, want nil", rep.SnapshotErr)
	}
	if _, err := os.Stat(filepath.Join(f.dest.Root, f.vol.Name, IndexDirName)); !os.IsNotExist(err) {
		t.Fatalf("snapshot taken with no Snapshotter (err=%v)", err)
	}
}

// TestSnapshotErrorDoesNotFailSync injects a backup failure (the local
// snapshot dir's parent is a regular file, so MkdirAll fails) and asserts
// the sync run's status is unchanged while the failure surfaces on
// SnapshotErr. Defense-in-depth must never flip a good sync to failed.
func TestSnapshotErrorDoesNotFailSync(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)

	// Make the snapshot dir un-creatable: its parent is a file.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sn := NewSnapshotter(f.store, f.rcl, SnapshotConfig{Dir: filepath.Join(blocker, "backups"), Keep: 7, Cloud: true, CloudKeep: 7})

	rep, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{Snapshot: sn})
	if err != nil {
		t.Fatalf("Sync returned error for a snapshot-only failure: %v", err)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success (snapshot failure must not flip status)", rep.Status)
	}
	if rep.SnapshotErr == nil {
		t.Fatalf("SnapshotErr = nil, want the injected backup failure")
	}
	// The runs row itself stays success.
	run, err := f.store.GetRun(context.Background(), rep.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunStatusSuccess {
		t.Fatalf("run row status = %q, want success", run.Status)
	}
}

// TestCloudRotationBoundsDir runs several syncs and asserts the
// destination .squirrel-index/ dir is bounded to cloud_keep snapshots.
// CloudKeep=2 with three syncs must leave exactly two (the newest).
func TestCloudRotationBoundsDir(t *testing.T) {
	f := setupFixture(t)
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)

	indexDir := filepath.Join(f.dest.Root, f.vol.Name, IndexDirName)
	var names []string
	for i := 0; i < 3; i++ {
		// A fresh Snapshotter per invocation: that is the real shape (one
		// per `squirrel sync`), and each takes its own single snapshot.
		sn := f.snapshotterFor(t, SnapshotConfig{Dir: t.TempDir(), Keep: 7, Cloud: true, CloudKeep: 2})
		rep, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{Snapshot: sn})
		if err != nil {
			t.Fatalf("Sync %d: %v", i, err)
		}
		if rep.SnapshotErr != nil {
			t.Fatalf("Sync %d SnapshotErr: %v", i, rep.SnapshotErr)
		}
		names = append(names, filepath.Base(globOneNewest(t, indexDir)))
	}
	left, err := filepath.Glob(filepath.Join(indexDir, "index-*-run-*.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 {
		t.Fatalf("cloud dir has %d snapshots, want 2 (cloud_keep): %v", len(left), left)
	}
	// The two survivors are the two newest by name (lexically sortable).
	if !containsBase(left, names[1]) || !containsBase(left, names[2]) {
		t.Fatalf("survivors %v are not the two newest %v", left, names[1:])
	}
}

// globOneNewest returns the lexically-greatest index snapshot in dir.
func globOneNewest(t *testing.T, dir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "index-*-run-*.db"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("glob newest in %s: %d matches, err=%v", dir, len(matches), err)
	}
	newest := matches[0]
	for _, m := range matches[1:] {
		if m > newest {
			newest = m
		}
	}
	return newest
}

func containsBase(paths []string, base string) bool {
	for _, p := range paths {
		if filepath.Base(p) == base {
			return true
		}
	}
	return false
}

// TestLocalRotationBoundsDir exercises rotateSnapshots directly: writing
// more than keep index-* files and rotating leaves the newest keep.
func TestLocalRotationBoundsDir(t *testing.T) {
	dir := t.TempDir()
	// Distinct, lexically-ordered names; modtime order matches creation.
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("index-2026010%dT000000.000Z-run-%d.db", i, i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// An unrelated file must be left untouched.
	if err := os.WriteFile(filepath.Join(dir, "keep-me.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := rotateSnapshots(dir, 2)
	if err != nil {
		t.Fatalf("rotateSnapshots: %v", err)
	}
	if len(removed) != 3 {
		t.Fatalf("removed %d, want 3: %v", len(removed), removed)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "index-*.db"))
	if len(left) != 2 {
		t.Fatalf("index files left = %d, want 2: %v", len(left), left)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep-me.txt")); err != nil {
		t.Fatalf("rotation removed an unrelated file: %v", err)
	}
}

// TestRotationExemptsPreMigrationSnapshots is the #112 guard: routine
// snapshot-on-sync rotation must never delete a pre-migration snapshot,
// even when far more than keep sync-time rotations run, because it is a
// buggy migration's only rollback surface. index-* snapshots still rotate.
func TestRotationExemptsPreMigrationSnapshots(t *testing.T) {
	dir := t.TempDir()
	preMig := "pre-migration-v5-to-v6-20260101T000000.000Z.db"
	if err := os.WriteFile(filepath.Join(dir, preMig), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Many more index-* snapshots than keep, written after the pre-migration
	// one so by modtime they would sort newer — the pre-migration snapshot
	// must survive on prefix, not on age.
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("index-2026020%dT000000.000Z-run-%d.db", i, i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := rotateSnapshots(dir, 2)
	if err != nil {
		t.Fatalf("rotateSnapshots: %v", err)
	}
	for _, r := range removed {
		if strings.HasPrefix(filepath.Base(r), "pre-migration-") {
			t.Fatalf("rotation removed a pre-migration snapshot: %s", r)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, preMig)); err != nil {
		t.Fatalf("pre-migration snapshot was deleted by sync-time rotation: %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "index-*.db"))
	if len(left) != 2 {
		t.Fatalf("index-* files left = %d, want 2 (keep): %v", len(left), left)
	}
}

// TestSyncFiltersOutIndexDirFromSource is the reserved-path guard for
// sync: a .squirrel-index dir that incidentally exists in the source
// volume must not be uploaded.
func TestSyncFiltersOutIndexDirFromSource(t *testing.T) {
	f := setupFixture(t)
	if err := os.MkdirAll(filepath.Join(f.vol.Path, IndexDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.vol.Path, IndexDirName, "stale.db"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.vol.Path, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runIndex(t)
	// No snapshotter here: we are asserting the *data sync* filter, not the
	// ride-along, so any .squirrel-index at the destination would be from
	// the source tree leaking through.
	if _, err := Sync(context.Background(), f.store, f.rcl, f.vol, f.dest, Options{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dest.Root, f.vol.Name, IndexDirName, "stale.db")); err == nil {
		t.Fatalf("source .squirrel-index leaked to destination; should be filtered")
	}
}

// TestReservedSyncPathCoversIndexDir documents that the peer-sync
// reserved-path predicates exclude .squirrel-index, so a node whose index
// carries rows there never re-publishes them.
func TestReservedSyncPathCoversIndexDir(t *testing.T) {
	if !isReservedSyncPath(IndexDirName + "/index-x-run-1.db") {
		t.Fatalf("isReservedSyncPath does not cover %s/", IndexDirName)
	}
	if !isReservedFolderPath(IndexDirName) {
		t.Fatalf("isReservedFolderPath does not cover bare %s", IndexDirName)
	}
}

// TestBuildArgsFilterIndexDir asserts both the sync and restore arg
// builders carry the .squirrel-index filter.
func TestBuildArgsFilterIndexDir(t *testing.T) {
	vol := &config.Volume{Name: "pics", Path: "/tmp/pics"}
	dest := &config.Destination{Name: "scratch", Type: "local", Root: "/tmp/dst"}

	syncArgs, err := buildRcloneArgs(vol, dest, 1, Options{})
	if err != nil {
		t.Fatalf("buildRcloneArgs: %v", err)
	}
	if !argsHaveFilter(syncArgs, "- /"+IndexDirName+"/**") {
		t.Fatalf("sync args missing .squirrel-index filter: %v", syncArgs)
	}
	restoreArgs := buildRestoreArgs(vol, dest, 1, RestoreOptions{})
	if !argsHaveFilter(restoreArgs, "- /"+IndexDirName+"/**") {
		t.Fatalf("restore args missing .squirrel-index filter: %v", restoreArgs)
	}
}

func argsHaveFilter(args []string, want string) bool {
	for i, a := range args {
		if a == "--filter" && i+1 < len(args) && args[i+1] == want {
			return true
		}
	}
	return false
}

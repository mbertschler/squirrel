package offload

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/store"
)

// skipIfRoot mirrors the index package's guard: uid 0 bypasses DAC
// permission checks, so chmod-based failure simulation silently stops
// failing.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("test relies on permission denial; root bypasses DAC checks")
	}
}

// restoreMtime resets a file's timestamps to the indexed mtime so a
// test can change bytes while keeping the (size, mtime) shortcut
// inputs identical.
func restoreMtime(t *testing.T, path string, mtimeNs int64) {
	t.Helper()
	ts := time.Unix(0, mtimeNs)
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// TestOffloadDriftSkips: any disagreement between the on-disk file and
// the indexed row — content with matching size+mtime, size, or mtime —
// refuses the deletion, leaving disk and row untouched.
func TestOffloadDriftSkips(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "content.txt"), "alpha")
	writeFile(t, filepath.Join(root, "size.txt"), "bravo")
	writeFile(t, filepath.Join(root, "mtime.txt"), "carol")
	s := setupStore(t)
	ctx := context.Background()
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	seedVector(t, s, v.ID, "t1", selfNode(t, s).ID, idx.RunID)

	contentRow := rowAt(t, s, v.ID, "content.txt")
	writeFile(t, filepath.Join(root, "content.txt"), "ALPHA")
	restoreMtime(t, filepath.Join(root, "content.txt"), contentRow.MtimeNs)

	writeFile(t, filepath.Join(root, "size.txt"), "bravo plus more")
	sizeRow := rowAt(t, s, v.ID, "size.txt")
	restoreMtime(t, filepath.Join(root, "size.txt"), sizeRow.MtimeNs)

	restoreMtime(t, filepath.Join(root, "mtime.txt"), rowAt(t, s, v.ID, "mtime.txt").MtimeNs+1)

	rep, err := Offload(ctx, s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if rep.Drift != 3 || rep.Offloaded != 0 || rep.Errors != 0 {
		t.Fatalf("report = %+v, want 3 drift skips", rep)
	}
	for path, wantReason := range map[string]string{
		"content.txt": "content hash changed",
		"size.txt":    "size changed",
		"mtime.txt":   "mtime changed",
	} {
		res := oneResult(t, rep, path, OutcomeDrift)
		if !strings.Contains(res.Reasons[0], wantReason) {
			t.Fatalf("%s reason = %v, want %q", path, res.Reasons, wantReason)
		}
		mustExist(t, filepath.Join(root, path))
		if row := rowAt(t, s, v.ID, path); row.Status != store.StatusPresent {
			t.Fatalf("%s status = %q, want present", path, row.Status)
		}
	}
}

// TestOffloadMissingOnDiskSkips: a file already gone from disk (with
// the row still 'present') is drift, never an offload.
func TestOffloadMissingOnDiskSkips(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	seedVector(t, s, v.ID, "t1", selfNode(t, s).ID, idx.RunID)
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}

	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"a.txt"}, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	res := oneResult(t, rep, "a.txt", OutcomeDrift)
	if !strings.Contains(res.Reasons[0], "missing on disk") {
		t.Fatalf("reason = %v, want missing-on-disk drift", res.Reasons)
	}
	if row := rowAt(t, s, v.ID, "a.txt"); row.Status != store.StatusPresent {
		t.Fatalf("status = %q, want present (only the indexer records absence)", row.Status)
	}
}

// TestOffloadSymlinkFileRefused: a symlink where the index recorded a
// regular file is refused and its target survives.
func TestOffloadSymlinkFileRefused(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	writeFile(t, filepath.Join(root, "target.txt"), "target bytes")
	s := setupStore(t)
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	seedVector(t, s, v.ID, "t1", selfNode(t, s).ID, idx.RunID)

	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "target.txt"), filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"a.txt"}, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	res := oneResult(t, rep, "a.txt", OutcomeDrift)
	if !strings.Contains(res.Reasons[0], "symlink") {
		t.Fatalf("reason = %v, want symlink refusal", res.Reasons)
	}
	mustExist(t, filepath.Join(root, "a.txt"))
	mustExist(t, filepath.Join(root, "target.txt"))
}

// TestOffloadSymlinkParentRefused: a directory component that became a
// symlink since indexing refuses the deletion even though the link
// resolves to the recorded bytes.
func TestOffloadSymlinkParentRefused(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dir", "c.txt"), "charlie")
	s := setupStore(t)
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	seedVector(t, s, v.ID, "t1", selfNode(t, s).ID, idx.RunID)

	if err := os.Rename(filepath.Join(root, "dir"), filepath.Join(root, "dir2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "dir2"), filepath.Join(root, "dir")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"dir/c.txt"}, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	res := oneResult(t, rep, "dir/c.txt", OutcomeDrift)
	if !strings.Contains(res.Reasons[0], "parent dir is a symlink") {
		t.Fatalf("reason = %v, want parent-symlink refusal", res.Reasons)
	}
	mustExist(t, filepath.Join(root, "dir2", "c.txt"))
}

// TestOffloadPartialRun: a per-file unlink failure is reported and
// counted, the other files still offload, and the runs row lands in
// 'partial' with file_count = the files actually offloaded.
func TestOffloadPartialRun(t *testing.T) {
	skipIfRoot(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	writeFile(t, filepath.Join(root, "ro", "b.txt"), "bravo")
	s := setupStore(t)
	ctx := context.Background()
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	seedVector(t, s, v.ID, "t1", selfNode(t, s).ID, idx.RunID)

	roDir := filepath.Join(root, "ro")
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	rep, err := Offload(ctx, s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if rep.Offloaded != 1 || rep.Errors != 1 {
		t.Fatalf("report = %+v, want 1 offloaded + 1 error", rep)
	}
	oneResult(t, rep, "a.txt", OutcomeOffloaded)
	res := oneResult(t, rep, "ro/b.txt", OutcomeError)
	if !strings.Contains(res.Reasons[0], "remove") {
		t.Fatalf("reason = %v, want remove failure", res.Reasons)
	}
	mustBeGone(t, filepath.Join(root, "a.txt"))
	mustExist(t, filepath.Join(root, "ro", "b.txt"))
	if row := rowAt(t, s, v.ID, "ro/b.txt"); row.Status != store.StatusPresent {
		t.Fatalf("ro/b.txt status = %q, want present", row.Status)
	}

	run, err := s.GetRun(ctx, rep.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunStatusPartial || run.FileCount != 1 {
		t.Fatalf("run = %+v, want status=partial file_count=1", run)
	}
}

package index

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/runevents"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/volmark"
)

func blake3Of(t *testing.T, content string) []byte {
	t.Helper()
	sum := blake3.Sum256([]byte(content))
	return sum[:]
}

func setupStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// skipIfRoot bails out of tests that simulate read errors with chmod 0o000.
// The kernel bypasses DAC permission checks for uid=0, so the chmod becomes
// a no-op and the assertion that errors surfaced would fail spuriously when
// the suite is run as root (containers, some sandboxes). CI runs as a
// non-root user so coverage is preserved there.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("test relies on chmod 0o000 denying access; root bypasses DAC checks")
	}
}

// volumeFor resolves the volume for an absolute path, failing the test if
// the volume hasn't been created by the indexer yet.
func volumeFor(t *testing.T, s *store.Store, absPath string) store.Volume {
	t.Helper()
	v, err := s.GetVolumeByPath(context.Background(), absPath)
	if err != nil {
		t.Fatalf("GetVolumeByPath(%q): %v", absPath, err)
	}
	return v
}

func TestFullIndex(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "hello")
	writeFile(t, filepath.Join(root, "sub", "b.txt"), "world")
	writeFile(t, filepath.Join(root, "sub", "c.txt"), "world") // duplicate of b

	s := setupStore(t)
	ctx := context.Background()

	rep, err := Index(ctx, s, root, Options{})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if rep.Added != 3 || rep.Modified != 0 || rep.Errors != 0 {
		t.Fatalf("unexpected first-run report: %+v", rep)
	}

	absRoot, _ := filepath.Abs(root)
	vol := volumeFor(t, s, absRoot)

	// All three rows present.
	a, err := s.GetByPath(ctx, vol.ID, "a.txt")
	if err != nil {
		t.Fatalf("GetByPath a: %v", err)
	}
	if a.Status != store.StatusPresent {
		t.Fatalf("a.Status = %s", a.Status)
	}
	if a.VolumeID != vol.ID {
		t.Fatalf("a.VolumeID = %d, want %d", a.VolumeID, vol.ID)
	}
	if a.Path != "a.txt" {
		t.Fatalf("a.Path = %q, want %q", a.Path, "a.txt")
	}
	if len(a.Blake3) != 32 {
		t.Fatalf("a.Blake3 len = %d, want 32", len(a.Blake3))
	}

	// b and c are duplicates.
	dups, err := s.ListDuplicates(ctx)
	if err != nil {
		t.Fatalf("ListDuplicates: %v", err)
	}
	if len(dups) != 2 {
		t.Fatalf("dups = %d, want 2", len(dups))
	}
	if !bytes.Equal(dups[0].File.Blake3, dups[1].File.Blake3) {
		t.Fatalf("dup blake3 digests differ: %+v", dups)
	}
}

func TestReindexNoOpUnchanged(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "hello")
	writeFile(t, filepath.Join(root, "b.txt"), "world")

	s := setupStore(t)
	ctx := context.Background()

	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("first Index: %v", err)
	}
	rep, err := Index(ctx, s, root, Options{Shallow: true})
	if err != nil {
		t.Fatalf("second Index: %v", err)
	}
	if rep.Unchanged != 2 || rep.Added != 0 || rep.Modified != 0 {
		t.Fatalf("expected no-op (shallow), got %+v", rep)
	}
}

// TestProgressCallbackCarriesBytesAndPath verifies the index collector emits
// in-flight progress events that carry the running file count, a non-zero
// byte total, and the current path — the signal the CLI renders live. The
// callback is invoked from the collector goroutine, so guard the slice.
func TestProgressCallbackCarriesBytesAndPath(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "hello")
	writeFile(t, filepath.Join(root, "sub", "b.txt"), "world!!")

	s := setupStore(t)
	ctx := context.Background()

	var mu sync.Mutex
	var events []runevents.Progress
	rep, err := Index(ctx, s, root, Options{
		Progress: func(p runevents.Progress) {
			mu.Lock()
			events = append(events, p)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if rep.Added != 2 {
		t.Fatalf("expected 2 added, got %+v", rep)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 {
		t.Fatal("no progress events emitted")
	}
	last := events[len(events)-1]
	if last.Stage != runevents.StageHashing {
		t.Errorf("stage = %q, want %q", last.Stage, runevents.StageHashing)
	}
	if last.Done != 2 {
		t.Errorf("final Done = %d, want 2", last.Done)
	}
	if last.BytesDone != int64(len("hello")+len("world!!")) {
		t.Errorf("final BytesDone = %d, want %d", last.BytesDone, len("hello")+len("world!!"))
	}
	if last.Message == "" {
		t.Error("progress event carried no current path")
	}
}

// TestNoProgressCallbackIsQuiet confirms a nil Progress option (the agent and
// scripted path) neither errors nor changes the outcome — the throttle no-ops.
func TestNoProgressCallbackIsQuiet(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "hello")

	s := setupStore(t)
	rep, err := Index(context.Background(), s, root, Options{})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if rep.Added != 1 {
		t.Fatalf("expected 1 added, got %+v", rep)
	}
}

func TestIncrementalAddModifyDelete(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "b.txt")
	writeFile(t, a, "hello")
	writeFile(t, b, "world")

	s := setupStore(t)
	ctx := context.Background()
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("first Index: %v", err)
	}

	// Modify a, delete b, add c.
	// Sleep a bit so mtime changes are detectable; not strictly needed since
	// content changes are detected by hash regardless.
	time.Sleep(10 * time.Millisecond)
	writeFile(t, a, "goodbye")
	if err := os.Remove(b); err != nil {
		t.Fatalf("remove b: %v", err)
	}
	c := filepath.Join(root, "c.txt")
	writeFile(t, c, "new file")

	rep, err := Index(ctx, s, root, Options{})
	if err != nil {
		t.Fatalf("second Index: %v", err)
	}
	if rep.Added != 1 {
		t.Fatalf("Added = %d, want 1", rep.Added)
	}
	if rep.Modified != 1 {
		t.Fatalf("Modified = %d, want 1", rep.Modified)
	}
	if rep.Missing != 1 {
		t.Fatalf("Missing = %d, want 1", rep.Missing)
	}

	absRoot, _ := filepath.Abs(root)
	vol := volumeFor(t, s, absRoot)

	// b should be present in DB but flagged missing.
	row, err := s.GetByPath(ctx, vol.ID, "b.txt")
	if err != nil {
		t.Fatalf("GetByPath b: %v", err)
	}
	if row.Status != store.StatusMissing {
		t.Fatalf("b.Status = %s, want missing", row.Status)
	}

	missing, err := s.ListMissing(ctx)
	if err != nil {
		t.Fatalf("ListMissing: %v", err)
	}
	if len(missing) != 1 || missing[0].File.Path != "b.txt" || missing[0].Volume.Path != absRoot {
		t.Fatalf("ListMissing = %+v", missing)
	}
}

func TestDryRunDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "hello")

	s := setupStore(t)
	ctx := context.Background()

	rep, err := Index(ctx, s, root, Options{DryRun: true})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if rep.Added != 1 {
		t.Fatalf("Added = %d, want 1", rep.Added)
	}

	absRoot, _ := filepath.Abs(root)
	// DB should be empty — no volume registered, no files written.
	if _, err := s.GetVolumeByPath(ctx, absRoot); !store.IsNotFound(err) {
		t.Fatalf("dry-run created a volume row: %v", err)
	}
}

func TestQueryByHash(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "x"), "same")
	writeFile(t, filepath.Join(root, "y"), "same")
	writeFile(t, filepath.Join(root, "z"), "different")

	s := setupStore(t)
	ctx := context.Background()
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	absRoot, _ := filepath.Abs(root)
	vol := volumeFor(t, s, absRoot)
	x, err := s.GetByPath(ctx, vol.ID, "x")
	if err != nil {
		t.Fatalf("GetByPath x: %v", err)
	}
	rows, err := s.GetByBlake3(ctx, x.Blake3)
	if err != nil {
		t.Fatalf("GetByBlake3: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("GetByBlake3 returned %d rows, want 2", len(rows))
	}
}

func TestSymlinksSkipped(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real.txt")
	writeFile(t, target, "hello")
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	s := setupStore(t)
	ctx := context.Background()
	rep, err := Index(ctx, s, root, Options{})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if rep.Added != 1 {
		t.Fatalf("Added = %d, want 1 (symlink should be skipped)", rep.Added)
	}
}

func TestFullReindexReportsUnchanged(t *testing.T) {
	// Without --shallow we re-hash every file but the result should still
	// classify as Unchanged when the content matches the stored digest.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "hello")
	writeFile(t, filepath.Join(root, "b.txt"), "world")

	s := setupStore(t)
	ctx := context.Background()
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("first Index: %v", err)
	}
	rep, err := Index(ctx, s, root, Options{Shallow: false})
	if err != nil {
		t.Fatalf("second Index: %v", err)
	}
	if rep.Unchanged != 2 || rep.Added != 0 || rep.Modified != 0 {
		t.Fatalf("full re-index report = %+v, want Unchanged=2", rep)
	}
}

func TestMissingFileRestored(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	writeFile(t, a, "hello")

	s := setupStore(t)
	ctx := context.Background()
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("first Index: %v", err)
	}

	// Delete the file; should flip to missing.
	if err := os.Remove(a); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("second Index: %v", err)
	}
	absRoot, _ := filepath.Abs(root)
	vol := volumeFor(t, s, absRoot)
	row, err := s.GetByPath(ctx, vol.ID, "a.txt")
	if err != nil {
		t.Fatalf("GetByPath after delete: %v", err)
	}
	if row.Status != store.StatusMissing {
		t.Fatalf("status after delete = %s, want missing", row.Status)
	}

	// Restore with the same content; status should flip back to present.
	writeFile(t, a, "hello")
	rep, err := Index(ctx, s, root, Options{})
	if err != nil {
		t.Fatalf("third Index: %v", err)
	}
	// Same digest + previously-missing row hits the Modified branch in
	// processFile because existing.Status != Present.
	if rep.Modified != 1 {
		t.Fatalf("restore report = %+v, want Modified=1", rep)
	}
	row, err = s.GetByPath(ctx, vol.ID, "a.txt")
	if err != nil {
		t.Fatalf("GetByPath after restore: %v", err)
	}
	if row.Status != store.StatusPresent {
		t.Fatalf("status after restore = %s, want present", row.Status)
	}
}

func TestEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	s := setupStore(t)
	ctx := context.Background()

	rep, err := Index(ctx, s, root, Options{})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if !isZeroReport(rep) {
		t.Fatalf("report = %+v, want zero", rep)
	}

	// Re-indexing should also be a clean no-op.
	rep, err = Index(ctx, s, root, Options{})
	if err != nil {
		t.Fatalf("re-Index: %v", err)
	}
	if !isZeroReport(rep) {
		t.Fatalf("re-index report = %+v, want zero", rep)
	}
}

func isZeroReport(r Report) bool {
	return r.Added == 0 && r.Modified == 0 && r.Unchanged == 0 && r.Missing == 0 && r.Errors == 0 && len(r.ErrorList) == 0
}

func TestReportSurfacesPerFileErrors(t *testing.T) {
	skipIfRoot(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "ok.txt"), "hello")
	unreadable := filepath.Join(root, "denied.txt")
	writeFile(t, unreadable, "secret")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Skipf("chmod 000 not supported: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	s := setupStore(t)
	rep, err := Index(context.Background(), s, root, Options{})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if rep.Errors != 1 || len(rep.ErrorList) != 1 {
		t.Fatalf("expected exactly one error in Report, got %+v", rep)
	}
	if !strings.Contains(rep.ErrorList[0].Error(), "denied.txt") {
		t.Fatalf("error %q does not reference the unreadable file", rep.ErrorList[0])
	}
	if rep.Added != 1 {
		t.Fatalf("Added = %d, want 1 (the readable file)", rep.Added)
	}
}

func TestWorkerCountDoesNotAffectResult(t *testing.T) {
	// Index the same tree under Workers=1 and Workers=8; the resulting row set
	// (paths + digests) must match. This is the canary for races in the
	// walker/worker/DB-writer plumbing.
	build := func() string {
		root := t.TempDir()
		// Mix of file sizes and a nested directory to exercise both small
		// and larger hashing paths.
		writeFile(t, filepath.Join(root, "a.txt"), "alpha")
		writeFile(t, filepath.Join(root, "b.txt"), "beta")
		writeFile(t, filepath.Join(root, "sub", "c.txt"), strings.Repeat("c", 4096))
		writeFile(t, filepath.Join(root, "sub", "d.txt"), strings.Repeat("d", 200_000))
		writeFile(t, filepath.Join(root, "sub", "nested", "e.txt"), "eps")
		return root
	}

	collect := func(workers int) map[string]string {
		t.Helper()
		root := build()
		s := setupStore(t)
		ctx := context.Background()
		rep, err := Index(ctx, s, root, Options{Workers: workers})
		if err != nil {
			t.Fatalf("Index(workers=%d): %v", workers, err)
		}
		if rep.Added != 5 {
			t.Fatalf("workers=%d: Added=%d, want 5", workers, rep.Added)
		}
		absRoot, _ := filepath.Abs(root)
		vol := volumeFor(t, s, absRoot)
		paths, err := s.ListPresentPathsUnder(ctx, vol.ID)
		if err != nil {
			t.Fatalf("ListPresentPathsUnder: %v", err)
		}
		out := make(map[string]string, len(paths))
		for p := range paths {
			row, err := s.GetByPath(ctx, vol.ID, p)
			if err != nil {
				t.Fatalf("GetByPath %s: %v", p, err)
			}
			out[p] = fmt.Sprintf("%x", row.Blake3)
		}
		return out
	}

	single := collect(1)
	many := collect(8)

	if len(single) != len(many) {
		t.Fatalf("row count differs: workers=1 has %d, workers=8 has %d", len(single), len(many))
	}
	for path, digest := range single {
		if many[path] != digest {
			t.Fatalf("digest differs at %q: workers=1 %s, workers=8 %s", path, digest, many[path])
		}
	}
}

func TestIndexRecordsRuns(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "hello")

	s := setupStore(t)
	ctx := context.Background()
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("first Index: %v", err)
	}
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("second Index: %v", err)
	}

	absRoot, _ := filepath.Abs(root)
	vol := volumeFor(t, s, absRoot)
	runs, err := s.ListRuns(ctx, store.ListRunsOpts{VolumeID: &vol.ID})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d run rows, want 2: %+v", len(runs), runs)
	}
	for i, r := range runs {
		if r.Kind != store.RunKindIndex {
			t.Fatalf("run %d kind = %q, want %q", i, r.Kind, store.RunKindIndex)
		}
		if r.Status != store.RunStatusSuccess {
			t.Fatalf("run %d status = %q, want %q", i, r.Status, store.RunStatusSuccess)
		}
		if !r.EndedAtNs.Valid {
			t.Fatalf("run %d ended_at_ns NULL, want set", i)
		}
		if r.FileCount != 1 {
			t.Fatalf("run %d file_count = %d, want 1", i, r.FileCount)
		}
	}

	// The file row's last_seen_run_id must advance to the second run while
	// first_seen_run_id stays at the first.
	row, err := s.GetByPath(ctx, vol.ID, "a.txt")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if row.FirstSeenRunID != runs[0].ID {
		t.Fatalf("FirstSeenRunID = %d, want %d", row.FirstSeenRunID, runs[0].ID)
	}
	if row.LastSeenRunID != runs[1].ID {
		t.Fatalf("LastSeenRunID = %d, want %d (second run)", row.LastSeenRunID, runs[1].ID)
	}
}

// TestIndexRecordsChangedCount pins the v28 column for index runs (#182):
// the first walk added a file and the second changed nothing, so the two
// rows carry the same file_count but different changed counts — and only
// the second folds out of `runs --changes` and the TUI's fold. A third walk
// after an edit and a delete counts both as changes.
func TestIndexRecordsChangedCount(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "hello")
	writeFile(t, filepath.Join(root, "b.txt"), "world")

	s := setupStore(t)
	ctx := context.Background()
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("first Index: %v", err)
	}
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("second Index: %v", err)
	}
	writeFile(t, filepath.Join(root, "a.txt"), "hello again")
	if err := os.Remove(filepath.Join(root, "b.txt")); err != nil {
		t.Fatalf("remove b.txt: %v", err)
	}
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("third Index: %v", err)
	}

	absRoot, _ := filepath.Abs(root)
	vol := volumeFor(t, s, absRoot)
	runs, err := s.ListRuns(ctx, store.ListRunsOpts{VolumeID: &vol.ID})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("got %d run rows, want 3: %+v", len(runs), runs)
	}
	// added a.txt + b.txt, nothing, then a.txt modified + b.txt missing.
	wantChanged := []int64{2, 0, 2}
	for i, r := range runs {
		if !r.ChangedCount.Valid {
			t.Fatalf("run %d changed_count is NULL, want %d", i, wantChanged[i])
		}
		if r.ChangedCount.Int64 != wantChanged[i] {
			t.Errorf("run %d changed_count = %d, want %d", i, r.ChangedCount.Int64, wantChanged[i])
		}
	}
	if runs[0].NoOp() || !runs[1].NoOp() || runs[2].NoOp() {
		t.Errorf("NoOp() = %v, want [false true false]",
			[]bool{runs[0].NoOp(), runs[1].NoOp(), runs[2].NoOp()})
	}
	// The all-unchanged walk still considered every file — the point of
	// the new column is that file_count alone couldn't say it was a no-op.
	if runs[1].FileCount != 2 {
		t.Errorf("no-op run file_count = %d, want 2 (files considered)", runs[1].FileCount)
	}
}

// TestIndexRecordsShallowFlag pins that the runs row carries the
// Options.Shallow value the call was made with: shallow=true for the
// fast (size, mtime) path and shallow=false for the rehash-everything
// path. The flag is what the UIs render to tell users whether the
// run's "unchanged" counts were verified by hash or trusted by stat.
func TestIndexRecordsShallowFlag(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "hello")

	s := setupStore(t)
	ctx := context.Background()
	rep1, err := Index(ctx, s, root, Options{Shallow: false})
	if err != nil {
		t.Fatalf("first Index: %v", err)
	}
	rep2, err := Index(ctx, s, root, Options{Shallow: true})
	if err != nil {
		t.Fatalf("second Index: %v", err)
	}
	first, err := s.GetRun(ctx, rep1.RunID)
	if err != nil {
		t.Fatalf("GetRun first: %v", err)
	}
	if !first.Shallow.Valid || first.Shallow.Bool {
		t.Fatalf("first run shallow = %+v, want valid=true bool=false", first.Shallow)
	}
	second, err := s.GetRun(ctx, rep2.RunID)
	if err != nil {
		t.Fatalf("GetRun second: %v", err)
	}
	if !second.Shallow.Valid || !second.Shallow.Bool {
		t.Fatalf("second run shallow = %+v, want valid=true bool=true", second.Shallow)
	}
}

func TestIndexPartialRunOnPerFileError(t *testing.T) {
	skipIfRoot(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "ok.txt"), "hello")
	unreadable := filepath.Join(root, "denied.txt")
	writeFile(t, unreadable, "secret")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Skipf("chmod 000 not supported: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	s := setupStore(t)
	ctx := context.Background()
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	absRoot, _ := filepath.Abs(root)
	vol := volumeFor(t, s, absRoot)
	runs, err := s.ListRuns(ctx, store.ListRunsOpts{VolumeID: &vol.ID})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0].Status != store.RunStatusPartial {
		t.Fatalf("status = %q, want partial (per-file error during a completed walk)", runs[0].Status)
	}
}

// TestPartialRunDoesNotMarkErroredFilesMissing guards a subtle correctness
// rule: a file that errored during hashing did not have its last_seen_run_id
// bumped this run, so a naive MarkMissing call would flip it to 'missing'
// even though the file still exists on disk. finalizeMissing must skip the
// flip when report.Errors > 0.
func TestPartialRunDoesNotMarkErroredFilesMissing(t *testing.T) {
	skipIfRoot(t)
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "b.txt")
	writeFile(t, a, "alpha")
	writeFile(t, b, "beta")

	s := setupStore(t)
	ctx := context.Background()
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("first Index: %v", err)
	}

	// Make b unreadable so the second run errors on it. b is still on disk,
	// so it must NOT be flagged as missing.
	if err := os.Chmod(b, 0o000); err != nil {
		t.Skipf("chmod 000 not supported: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(b, 0o644) })

	rep, err := Index(ctx, s, root, Options{})
	if err != nil {
		t.Fatalf("second Index: %v", err)
	}
	if rep.Errors == 0 {
		t.Fatalf("expected per-file error on unreadable b, got %+v", rep)
	}
	if rep.Missing != 0 {
		t.Fatalf("Missing=%d on partial run, want 0 (skipped for safety)", rep.Missing)
	}
	absRoot, _ := filepath.Abs(root)
	vol := volumeFor(t, s, absRoot)
	bRow, err := s.GetByPath(ctx, vol.ID, "b.txt")
	if err != nil {
		t.Fatalf("GetByPath b: %v", err)
	}
	if bRow.Status != store.StatusPresent {
		t.Fatalf("b.Status = %s, want present (file still on disk, was just unreadable)", bRow.Status)
	}
}

// TestReindexModifiedFilePreservesOldHash is the end-to-end version of the
// append-only guarantee: write content A, index, change to content B,
// re-index. The original hash A must still be queryable from the index as a
// superseded row, not silently dropped.
func TestReindexModifiedFilePreservesOldHash(t *testing.T) {
	root := t.TempDir()
	doc := filepath.Join(root, "doc.txt")
	writeFile(t, doc, "version A")

	s := setupStore(t)
	ctx := context.Background()
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("first Index: %v", err)
	}
	absRoot, _ := filepath.Abs(root)
	vol := volumeFor(t, s, absRoot)
	first, err := s.GetByPath(ctx, vol.ID, "doc.txt")
	if err != nil {
		t.Fatalf("GetByPath after first: %v", err)
	}
	hashA := append([]byte(nil), first.Blake3...)

	writeFile(t, doc, "version B is different")
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("second Index: %v", err)
	}

	history, err := s.ListHistoryByPath(ctx, vol.ID, "doc.txt")
	if err != nil {
		t.Fatalf("ListHistoryByPath: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history = %d rows, want 2 (A superseded + B present)", len(history))
	}
	if !bytes.Equal(history[0].Blake3, hashA) || history[0].Status != store.StatusSuperseded {
		t.Fatalf("history[0] = %+v, want hashA superseded", history[0])
	}
	if history[1].Status != store.StatusPresent {
		t.Fatalf("history[1].Status = %s, want present", history[1].Status)
	}
	if bytes.Equal(history[1].Blake3, hashA) {
		t.Fatalf("history[1].Blake3 still matches hashA — old hash was overwritten in place")
	}

	// Looking up by the original hash must still find the path (now via the
	// superseded row).
	matches, err := s.GetByBlake3(ctx, hashA)
	if err != nil {
		t.Fatalf("GetByBlake3: %v", err)
	}
	if len(matches) != 1 || matches[0].File.Status != store.StatusSuperseded {
		t.Fatalf("GetByBlake3(hashA) = %+v, want one superseded row", matches)
	}
}

// TestHashFileSizePairsWithDigest pins the stat-after-hash contract: the
// size hashFile returns is the count of bytes that produced the digest, read
// from the open handle's fstat rather than a path stat that could observe a
// different inode state. A regression to a pre-hash path stat would let the
// digest and the size describe different bytes; here they must agree.
func TestHashFileSizePairsWithDigest(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.txt")
	content := "the quick brown fox"
	writeFile(t, path, content)

	got, err := hashFile(path, make([]byte, hashReadBufferSize))
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	if got.sizeBytes != int64(len(content)) {
		t.Fatalf("sizeBytes = %d, want %d (size of the hashed bytes)", got.sizeBytes, len(content))
	}
	if !bytes.Equal(got.digest, blake3Of(t, content)) {
		t.Fatalf("digest does not match BLAKE3 of the %d hashed bytes", len(content))
	}
}

// TestReindexAfterAppendSupersedesToConsistentRow exercises the residual the
// stat-after-hash fix documents: a file that grows between index runs cannot
// leave a row whose size disagrees with its hash. The grown file mints a new
// (digest, size) pair that supersedes the prior row, and each live row's size
// matches the content its hash covers.
func TestReindexAfterAppendSupersedesToConsistentRow(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.txt")
	writeFile(t, path, "abc")

	s := setupStore(t)
	ctx := context.Background()
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("first Index: %v", err)
	}
	absRoot, _ := filepath.Abs(root)
	vol := volumeFor(t, s, absRoot)
	first, err := s.GetByPath(ctx, vol.ID, "doc.txt")
	if err != nil {
		t.Fatalf("GetByPath after first: %v", err)
	}
	if first.SizeBytes != 3 || !bytes.Equal(first.Blake3, blake3Of(t, "abc")) {
		t.Fatalf("first row = (size=%d), want size=3 matching BLAKE3(abc)", first.SizeBytes)
	}

	writeFile(t, path, "abcdef")
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("second Index: %v", err)
	}

	history, err := s.ListHistoryByPath(ctx, vol.ID, "doc.txt")
	if err != nil {
		t.Fatalf("ListHistoryByPath: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history = %d rows, want 2", len(history))
	}
	for _, row := range history {
		want := blake3Of(t, "abc")
		wantSize := int64(3)
		if row.Status == store.StatusPresent {
			want, wantSize = blake3Of(t, "abcdef"), 6
		}
		if row.SizeBytes != wantSize || !bytes.Equal(row.Blake3, want) {
			t.Fatalf("row (status=%s) size=%d does not match its hash", row.Status, row.SizeBytes)
		}
	}
}

func TestDryRunDoesNotRecordRun(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "hello")

	s := setupStore(t)
	ctx := context.Background()
	if _, err := Index(ctx, s, root, Options{DryRun: true}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	// Dry-run skips volume creation, so any runs we recorded would necessarily
	// be cross-volume / orphan rows. The cleanest check: look up the absent
	// volume; if it really doesn't exist, the runs table has nothing pointing
	// at it. A stronger assertion is that ListRuns(nil filter) returns nothing.
	absRoot, _ := filepath.Abs(root)
	if _, err := s.GetVolumeByPath(ctx, absRoot); !store.IsNotFound(err) {
		t.Fatalf("dry-run created a volume row: %v", err)
	}
	runs, err := s.ListRuns(ctx, store.ListRunsOpts{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected no runs after dry-run, got %+v", runs)
	}
}

func TestRunStatus(t *testing.T) {
	boom := fmt.Errorf("boom")
	cases := []struct {
		name      string
		fatalErr  error
		errors    int
		wantState string
		wantMsg   string
	}{
		{"clean", nil, 0, store.RunStatusSuccess, ""},
		{"per-file errors only", nil, 2, store.RunStatusPartial, ""},
		{"fatal alone", boom, 0, store.RunStatusFailed, "boom"},
		{"fatal trumps per-file", boom, 3, store.RunStatusFailed, "boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, msg := runStatus(&Report{Errors: c.errors}, c.fatalErr)
			if got != c.wantState {
				t.Fatalf("status = %q, want %q", got, c.wantState)
			}
			if msg != c.wantMsg {
				t.Fatalf("msg = %q, want %q", msg, c.wantMsg)
			}
		})
	}
}

// TestIndexWalkErrorReachesRun verifies that a directory we can't descend
// into (chmod 0o000) reaches sendErr via filepath.WalkDir's error callback
// and that the resulting run still finishes as 'partial' (the walk completed
// for the parts we could see).
func TestIndexWalkErrorReachesRun(t *testing.T) {
	skipIfRoot(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "visible.txt"), "hello")
	hidden := filepath.Join(root, "no-entry")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatalf("mkdir hidden: %v", err)
	}
	writeFile(t, filepath.Join(hidden, "inside.txt"), "secret")
	if err := os.Chmod(hidden, 0o000); err != nil {
		t.Skipf("chmod 000 on dir not supported: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(hidden, 0o755) })

	s := setupStore(t)
	ctx := context.Background()
	rep, err := Index(ctx, s, root, Options{})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if rep.Errors == 0 {
		t.Fatalf("expected at least one per-entry error from blocked dir, got %+v", rep)
	}
	absRoot, _ := filepath.Abs(root)
	vol := volumeFor(t, s, absRoot)
	runs, err := s.ListRuns(ctx, store.ListRunsOpts{VolumeID: &vol.ID})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Status != store.RunStatusPartial {
		t.Fatalf("got runs %+v, want one partial run", runs)
	}
}

func TestDryRunReportsMissingCount(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "b.txt")
	writeFile(t, a, "hello")
	writeFile(t, b, "world")

	s := setupStore(t)
	ctx := context.Background()
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("first Index: %v", err)
	}

	// Remove one file then dry-run; the report should show Missing=1 without
	// actually flipping the DB row.
	if err := os.Remove(b); err != nil {
		t.Fatalf("remove: %v", err)
	}
	rep, err := Index(ctx, s, root, Options{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run Index: %v", err)
	}
	if rep.Missing != 1 {
		t.Fatalf("dry-run Missing = %d, want 1", rep.Missing)
	}

	absRoot, _ := filepath.Abs(root)
	vol := volumeFor(t, s, absRoot)
	row, err := s.GetByPath(ctx, vol.ID, "b.txt")
	if err != nil {
		t.Fatalf("GetByPath b after dry-run: %v", err)
	}
	if row.Status != store.StatusPresent {
		t.Fatalf("dry-run mutated DB: status = %s, want present (untouched)", row.Status)
	}
}

// TestConcurrentIndexCallsCannotOverlap covers the TOCTOU race the new
// BeginIndexRunIfClear gate closes. Asserting "exactly one of N
// parallel Index calls succeeds" is timing-dependent (a fast walk can
// finish before the next goroutine reaches the gate), so we instead
// park a running index row externally and confirm that *every*
// concurrent Index call is refused with the sentinel pointing at the
// parked blocker. The store-level race coverage lives in
// TestBeginIndexRunIfClearAtomic.
func TestConcurrentIndexCallsCannotOverlap(t *testing.T) {
	s := setupStore(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	writeFile(t, filepath.Join(root, "b.txt"), "beta")

	// Materialise the volume row via a real Index pass so the parked
	// row can reference a valid volume_id.
	if _, err := Index(context.Background(), s, root, Options{Name: "v", Workers: 2}); err != nil {
		t.Fatalf("seed Index: %v", err)
	}
	v := volumeFor(t, s, root)

	parkedID, blocker, err := s.BeginIndexRunIfClear(context.Background(), store.RunKindIndex, v.ID, false)
	if err != nil {
		t.Fatalf("park running row: %v", err)
	}
	if blocker != nil {
		t.Fatalf("park gate unexpectedly blocked: blocker=%+v", blocker)
	}
	defer func() {
		_ = s.FinishRun(context.Background(), parkedID, store.RunStatusSuccess, "", 0)
	}()

	const parallel = 4
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		blocked  int
		other    []error
		surprise int
	)
	start := make(chan struct{})
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := Index(context.Background(), s, root, Options{Name: "v", Workers: 2})
			mu.Lock()
			defer mu.Unlock()
			var inFlight *ErrAlreadyRunning
			switch {
			case err == nil:
				surprise++
			case errors.As(err, &inFlight):
				if inFlight.Blocker.ID != parkedID {
					other = append(other, fmt.Errorf("blocker id = %d, want %d", inFlight.Blocker.ID, parkedID))
					return
				}
				blocked++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if surprise > 0 {
		t.Fatalf("%d concurrent Index() calls succeeded while a row was parked", surprise)
	}
	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if blocked != parallel {
		t.Fatalf("blocked = %d, want %d", blocked, parallel)
	}

	// Releasing the parked row reopens the slot.
	if err := s.FinishRun(context.Background(), parkedID, store.RunStatusSuccess, "", 0); err != nil {
		t.Fatalf("FinishRun parked: %v", err)
	}
	if _, err := Index(context.Background(), s, root, Options{Name: "v", Workers: 2}); err != nil {
		t.Fatalf("post-finish Index: %v", err)
	}
}

// TestIndexBlocksConcurrentAudit confirms that an in-flight index run
// also refuses a fresh audit-kind invocation (and vice versa) on the
// same volume. Both walk the volume and call MarkMissing under their
// own run-id, so letting them overlap is exactly the corruption mode
// the gate guards against.
func TestIndexBlocksConcurrentAudit(t *testing.T) {
	s := setupStore(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")

	// Materialise the volume row via a real Index pass so we can park a
	// running row on it directly afterwards.
	if _, err := Index(context.Background(), s, root, Options{Name: "v", Workers: 2}); err != nil {
		t.Fatalf("seed Index: %v", err)
	}
	v := volumeFor(t, s, root)

	// Manually park a running index row to simulate "another index is
	// still in flight" without spinning up a second goroutine that
	// races on completion timing.
	idxID, blocker, err := s.BeginIndexRunIfClear(context.Background(), store.RunKindIndex, v.ID, false)
	if err != nil {
		t.Fatalf("seed running index row: %v", err)
	}
	if blocker != nil {
		t.Fatalf("seed gate unexpectedly blocked: blocker=%+v", blocker)
	}
	defer func() {
		_ = s.FinishRun(context.Background(), idxID, store.RunStatusSuccess, "", 0)
	}()

	_, err = Index(context.Background(), s, root, Options{Name: "v", Workers: 2, Kind: store.RunKindAudit})
	if err == nil {
		t.Fatalf("audit Index unexpectedly succeeded with an in-flight index")
	}
	var sentinel *ErrAlreadyRunning
	if !errors.As(err, &sentinel) {
		t.Fatalf("err = %v (%T), want *ErrAlreadyRunning", err, err)
	}
	if sentinel.Blocker.ID != idxID {
		t.Fatalf("blocker id = %d, want %d (the seeded index)", sentinel.Blocker.ID, idxID)
	}
	if sentinel.Kind != store.RunKindAudit {
		t.Fatalf("err.Kind = %q, want %q (the kind that was refused)", sentinel.Kind, store.RunKindAudit)
	}
}

// TestIndexWritesVolumeMarker confirms the first non-dry-run Index
// against a named volume drops a .squirrel-volume marker at the
// volume root, and that a subsequent run with the same name leaves
// it intact (the marker is metadata, not a per-run write target).
func TestIndexWritesVolumeMarker(t *testing.T) {
	s := setupStore(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")

	if _, err := Index(context.Background(), s, root, Options{Name: "pics", Workers: 2}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	m, err := volmark.Read(root)
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if m.Volume != "pics" {
		t.Fatalf("marker volume = %q, want %q", m.Volume, "pics")
	}
	if m.CreatedAt == "" {
		t.Fatalf("marker created_at empty; want timestamp")
	}

	// Second run leaves the marker untouched. Read again and confirm
	// timestamps match — a rewrite would bump CreatedAt.
	if _, err := Index(context.Background(), s, root, Options{Name: "pics", Workers: 2}); err != nil {
		t.Fatalf("second Index: %v", err)
	}
	m2, err := volmark.Read(root)
	if err != nil {
		t.Fatalf("marker missing after second run: %v", err)
	}
	if m2.CreatedAt != m.CreatedAt {
		t.Fatalf("marker rewritten by second run: %q → %q", m.CreatedAt, m2.CreatedAt)
	}

	// The marker file is not in the index (walker filters it).
	v, _ := s.GetVolumeByPath(context.Background(), root)
	if _, err := s.GetByPath(context.Background(), v.ID, volmark.MarkerName); err == nil {
		t.Fatalf("walker indexed %s; want skipped", volmark.MarkerName)
	}
}

// TestIndexRefusesMismatchedVolumeMarker covers the "wrong volume"
// case: a marker at the root names a different volume than the one
// the indexer was asked to populate. The fix prevents accidentally
// repurposing one volume's tree as a new one — that would invent
// two volume identities for the same byte tree.
func TestIndexRefusesMismatchedVolumeMarker(t *testing.T) {
	s := setupStore(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	if err := volmark.Write(root, volmark.Marker{Volume: "video"}); err != nil {
		t.Fatalf("seed wrong-volume marker: %v", err)
	}

	_, err := Index(context.Background(), s, root, Options{Name: "pics", Workers: 2})
	if err == nil {
		t.Fatalf("Index against mismatched marker should refuse")
	}
	var mismatch *volmark.ErrMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("err type = %T (%v), want *volmark.ErrMismatch", err, err)
	}
}

// markOffloaded flips the live row at relPath to status='offloaded'
// via an Upsert carrying the same content — the status transition the
// future offload command records once durability is proven.
func markOffloaded(t *testing.T, s *store.Store, volumeID int64, relPath string) {
	t.Helper()
	ctx := context.Background()
	row, err := s.GetByPath(ctx, volumeID, relPath)
	if err != nil {
		t.Fatalf("GetByPath %s: %v", relPath, err)
	}
	row.Status = store.StatusOffloaded
	if err := s.Upsert(ctx, row, nil); err != nil {
		t.Fatalf("Upsert offloaded %s: %v", relPath, err)
	}
}

// TestIndexLeavesOffloadedRowsAlone: an offloaded row's on-disk absence
// is intentional, so a re-index neither flips it to missing nor counts
// it in the report's Missing tally.
func TestIndexLeavesOffloadedRowsAlone(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "keep.txt"), "kept")
	writeFile(t, filepath.Join(root, "cold.txt"), "rarely needed")

	s := setupStore(t)
	ctx := context.Background()
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("first Index: %v", err)
	}
	absRoot, _ := filepath.Abs(root)
	vol := volumeFor(t, s, absRoot)

	markOffloaded(t, s, vol.ID, "cold.txt")
	if err := os.Remove(filepath.Join(root, "cold.txt")); err != nil {
		t.Fatal(err)
	}

	rep, err := Index(ctx, s, root, Options{})
	if err != nil {
		t.Fatalf("re-Index: %v", err)
	}
	if rep.Missing != 0 {
		t.Fatalf("report.Missing = %d, want 0 (offloaded absence is expected)", rep.Missing)
	}
	row, err := s.GetByPath(ctx, vol.ID, "cold.txt")
	if err != nil {
		t.Fatalf("GetByPath cold.txt: %v", err)
	}
	if row.Status != store.StatusOffloaded {
		t.Fatalf("cold.txt status = %q after re-index, want offloaded", row.Status)
	}
}

// TestIndexFlipsOffloadedBackToPresent: when the file reappears on disk
// with its recorded content (a restore or manual copy-back), the next
// index run flips the row back to present, preserving first_seen.
func TestIndexFlipsOffloadedBackToPresent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "cold.txt"), "rarely needed")

	s := setupStore(t)
	ctx := context.Background()
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("first Index: %v", err)
	}
	absRoot, _ := filepath.Abs(root)
	vol := volumeFor(t, s, absRoot)
	before, err := s.GetByPath(ctx, vol.ID, "cold.txt")
	if err != nil {
		t.Fatalf("GetByPath before: %v", err)
	}

	markOffloaded(t, s, vol.ID, "cold.txt")
	if err := os.Remove(filepath.Join(root, "cold.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := Index(ctx, s, root, Options{}); err != nil {
		t.Fatalf("Index while offloaded: %v", err)
	}

	writeFile(t, filepath.Join(root, "cold.txt"), "rarely needed")
	rep, err := Index(ctx, s, root, Options{})
	if err != nil {
		t.Fatalf("Index after reappearance: %v", err)
	}
	if rep.Errors != 0 {
		t.Fatalf("report errors = %+v", rep.ErrorList)
	}

	after, err := s.GetByPath(ctx, vol.ID, "cold.txt")
	if err != nil {
		t.Fatalf("GetByPath after: %v", err)
	}
	if after.Status != store.StatusPresent {
		t.Fatalf("cold.txt status = %q after reappearance, want present", after.Status)
	}
	if !bytes.Equal(after.Blake3, before.Blake3) {
		t.Fatalf("cold.txt content changed across offload round trip")
	}
	if after.FirstSeenRunID != before.FirstSeenRunID {
		t.Fatalf("first_seen_run_id = %d, want %d (reappearance must not rewrite it)",
			after.FirstSeenRunID, before.FirstSeenRunID)
	}
}

// TestHashStatPinnedToHashedBytes simulates a file that grows between the
// walker's stat and the worker's hash: process must record the size read
// from the open handle after hashing (matching the hashed content), not
// the stale walk size. Binding the new digest to the old size would mint a
// contents row whose size_bytes can never match the honest content again.
func TestHashStatPinnedToHashedBytes(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "growing.txt")
	content := "the full on-disk content after the append"
	writeFile(t, abs, content)

	s := setupStore(t)
	ctx := context.Background()
	idx, err := newIndexer(ctx, s, root, Options{Name: "vol"})
	if err != nil {
		t.Fatalf("newIndexer: %v", err)
	}

	stale := workItem{
		absPath:   abs,
		relPath:   "growing.txt",
		sizeBytes: 3, // what the walker stat saw before the append
		mtimeNs:   1,
	}
	res := idx.process(stale, make([]byte, hashReadBufferSize))
	if res.err != nil {
		t.Fatalf("process: %v", res.err)
	}
	if res.row.SizeBytes != int64(len(content)) {
		t.Fatalf("row size = %d, want %d (the hashed bytes, not the stale walk size %d)",
			res.row.SizeBytes, len(content), stale.sizeBytes)
	}
	want := blake3Of(t, content)
	if !bytes.Equal(res.row.Blake3, want) {
		t.Fatalf("row digest = %x, want %x", res.row.Blake3, want)
	}
}

// TestReindexStableBytesNoSizeMismatch indexes a file, then re-indexes the
// same untouched bytes. The contents row minted on the first pass carries
// the size of the bytes that were hashed, so the second pass resolves the
// same (digest, size) pair and ApplyIndexBatch does not hit the immutable
// size cross-check that aborts the whole batch.
func TestReindexStableBytesNoSizeMismatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "stable.txt"), "stable content that never changes")

	s := setupStore(t)
	ctx := context.Background()
	if _, err := Index(ctx, s, root, Options{Name: "vol"}); err != nil {
		t.Fatalf("first Index: %v", err)
	}
	rep, err := Index(ctx, s, root, Options{Name: "vol"})
	if err != nil {
		t.Fatalf("second Index aborted (size cross-check?): %v", err)
	}
	if rep.Errors != 0 {
		t.Fatalf("re-index reported errors: %+v", rep.ErrorList)
	}
	if rep.Unchanged != 1 {
		t.Fatalf("re-index unchanged = %d, want 1", rep.Unchanged)
	}
}

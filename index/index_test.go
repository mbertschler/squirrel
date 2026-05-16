package index

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/store"
)

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
	runs, err := s.ListRunsForVolume(ctx, vol.ID)
	if err != nil {
		t.Fatalf("ListRunsForVolume: %v", err)
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

func TestIndexPartialRunOnPerFileError(t *testing.T) {
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
	runs, err := s.ListRunsForVolume(ctx, vol.ID)
	if err != nil {
		t.Fatalf("ListRunsForVolume: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0].Status != store.RunStatusPartial {
		t.Fatalf("status = %q, want partial (per-file error during a completed walk)", runs[0].Status)
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
	// at it. As a stronger assertion we also query the volume id 0 (impossible
	// match) to confirm ListRunsForVolume returns empty for missing volumes.
	absRoot, _ := filepath.Abs(root)
	if _, err := s.GetVolumeByPath(ctx, absRoot); !store.IsNotFound(err) {
		t.Fatalf("dry-run created a volume row: %v", err)
	}
	runs, err := s.ListRunsForVolume(ctx, 0)
	if err != nil {
		t.Fatalf("ListRunsForVolume: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected no runs after dry-run, got %+v", runs)
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

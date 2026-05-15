package index

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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

	// All three rows present.
	a, err := s.GetByPath(ctx, absRoot, "a.txt")
	if err != nil {
		t.Fatalf("GetByPath a: %v", err)
	}
	if a.Status != store.StatusPresent {
		t.Fatalf("a.Status = %s", a.Status)
	}
	if a.Root != absRoot {
		t.Fatalf("a.Root = %q, want %q", a.Root, absRoot)
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
	if !bytes.Equal(dups[0].Blake3, dups[1].Blake3) {
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

	// b should be present in DB but flagged missing.
	row, err := s.GetByPath(ctx, absRoot, "b.txt")
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
	if len(missing) != 1 || missing[0].Path != "b.txt" || missing[0].Root != absRoot {
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
	// DB should be empty.
	if _, err := s.GetByPath(ctx, absRoot, "a.txt"); !store.IsNotFound(err) {
		t.Fatalf("expected not found after dry-run, got err=%v", err)
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
	x, err := s.GetByPath(ctx, absRoot, "x")
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

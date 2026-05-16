package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIVolumesEmpty(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "test.db")
	out := runCLI(t, "volumes", "--db", db)
	if out != "" {
		t.Fatalf("expected empty output for empty DB, got %q", out)
	}
}

func TestCLIVolumesListsImplicitlyCreated(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	for _, dir := range []string{a, b} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(dir, "f.txt"), "hi")
	}
	db := filepath.Join(tmp, "test.db")
	runCLI(t, "index", "--db", db, a)
	runCLI(t, "index", "--db", db, b)

	out := runCLI(t, "volumes", "--db", db)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 volume lines, got %d:\n%s", len(lines), out)
	}
	for i, line := range lines {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			t.Fatalf("line %d not 3-column tab-separated: %q", i, line)
		}
	}
	if !strings.Contains(out, "\ta\t") || !strings.Contains(out, "\tb\t") {
		t.Fatalf("expected volume names 'a' and 'b' in output:\n%s", out)
	}
}

// Two volumes with the same basename get a numeric suffix on the second one.
func TestCLIVolumesBasenameCollision(t *testing.T) {
	tmp := t.TempDir()
	dir1 := filepath.Join(tmp, "alpha", "pictures")
	dir2 := filepath.Join(tmp, "beta", "pictures")
	for _, d := range []string{dir1, dir2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(d, "f.txt"), "x")
	}
	db := filepath.Join(tmp, "test.db")
	runCLI(t, "index", "--db", db, dir1)
	runCLI(t, "index", "--db", db, dir2)

	out := runCLI(t, "volumes", "--db", db)
	if !strings.Contains(out, "\tpictures\t") {
		t.Fatalf("expected first volume 'pictures' in output:\n%s", out)
	}
	if !strings.Contains(out, "\tpictures-2\t") {
		t.Fatalf("expected suffixed volume 'pictures-2' in output:\n%s", out)
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIRunsListsRecentFirst(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	db := filepath.Join(tmp, "test.db")

	runCLI(t, "index", "--db", db, src)
	runCLI(t, "index", "--db", db, src)

	out := runCLI(t, "runs", "--db", db)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines (1 header + N rows), want 3:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "ID") || !strings.Contains(lines[0], "STATUS") {
		t.Fatalf("header missing expected columns: %q", lines[0])
	}
	// Most-recent first: id of the second run > id of the first run.
	firstID, secondID := firstColumn(t, lines[1]), firstColumn(t, lines[2])
	if firstID <= secondID {
		t.Fatalf("rows not in DESC order: first row id=%d, second row id=%d", firstID, secondID)
	}
	for _, line := range lines[1:] {
		if !strings.Contains(line, "index") {
			t.Fatalf("row missing kind=index: %q", line)
		}
		if !strings.Contains(line, "success") {
			t.Fatalf("row missing status=success: %q", line)
		}
	}
}

func TestCLIRunsVolumeFilter(t *testing.T) {
	tmp := t.TempDir()
	dirA := filepath.Join(tmp, "a")
	dirB := filepath.Join(tmp, "b")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(d, "f.txt"), "content")
	}
	db := filepath.Join(tmp, "test.db")
	runCLI(t, "index", "--db", db, dirA)
	runCLI(t, "index", "--db", db, dirB)
	runCLI(t, "index", "--db", db, dirA)

	out := runCLI(t, "runs", "--db", db, "--volume", "a")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 2 runs for volume a, got lines:\n%s", out)
	}
	for _, line := range lines[1:] {
		if !strings.Contains(line, "  a  ") && !strings.HasSuffix(line, "  a") {
			// Tabwriter pads with spaces; volume name "a" should appear as
			// its own column. Use a forgiving substring check.
			if !strings.Contains(line, " a ") {
				t.Fatalf("row missing volume=a: %q", line)
			}
		}
	}
}

func TestCLIRunsLimit(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	db := filepath.Join(tmp, "test.db")
	for range 5 {
		runCLI(t, "index", "--db", db, src)
	}

	out := runCLI(t, "runs", "--db", db, "--limit", "2")
	rows := strings.Count(strings.TrimRight(out, "\n"), "\n") // header + 2 rows = 2 newlines? No, 3 lines = 2 newlines between, so rows ~= total lines.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("--limit 2 produced %d lines (header + rows), want 3:\n%s", rows+1, out)
	}
}

func TestCLIRunsUnknownVolume(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "test.db")
	// Open the DB once via an index command so the schema exists.
	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	runCLI(t, "index", "--db", db, src)

	err, _ := runCLIExpectErr(t, "runs", "--db", db, "--volume", "no-such-volume")
	if !strings.Contains(err.Error(), "no volume named") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// firstColumn returns the integer in the first tab-separated column of a
// runs row. Tabwriter pads with spaces, so we split on whitespace.
func firstColumn(t *testing.T, line string) int64 {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) == 0 {
		t.Fatalf("empty row")
	}
	var n int64
	for _, c := range fields[0] {
		if c < '0' || c > '9' {
			t.Fatalf("first column %q not numeric (full line: %q)", fields[0], line)
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

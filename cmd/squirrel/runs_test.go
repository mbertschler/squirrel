package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateErrorRuneAware guards against slicing a multibyte UTF-8 rune
// in half when an error message exceeds the column budget — that would
// embed an invalid rune in the output that some terminals render as the
// replacement character. Cut on a rune boundary instead.
func TestTruncateErrorRuneAware(t *testing.T) {
	// 70 copies of the 3-byte é → 210 bytes, 70 runes. The byte-slicing
	// implementation would land mid-rune; the rune-aware one must produce
	// only valid UTF-8.
	long := strings.Repeat("é", 70)
	got := truncateError(sql.NullString{String: long, Valid: true})
	if !utf8.ValidString(got) {
		t.Fatalf("truncated output is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis suffix, got %q", got)
	}
	if utf8.RuneCountInString(got) > 60 {
		t.Fatalf("output has %d runes, want ≤ 60", utf8.RuneCountInString(got))
	}

	// Below the limit: returned verbatim, no ellipsis.
	short := "boom"
	if g := truncateError(sql.NullString{String: short, Valid: true}); g != short {
		t.Fatalf("short message changed: %q → %q", short, g)
	}

	// NULL → empty.
	if g := truncateError(sql.NullString{}); g != "" {
		t.Fatalf("NULL string rendered as %q, want empty", g)
	}
}

func TestCLIRunsListsRecentFirst(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	f := writeConfigFor(t, map[string]string{"src": src})

	runCLI(t, "--config", f.configPath, "index", "src")
	runCLI(t, "--config", f.configPath, "index", "src")

	out := runCLI(t, "--config", f.configPath, "runs")
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
	f := writeConfigFor(t, map[string]string{"a": dirA, "b": dirB})
	runCLI(t, "--config", f.configPath, "index", "a")
	runCLI(t, "--config", f.configPath, "index", "b")
	runCLI(t, "--config", f.configPath, "index", "a")

	out := runCLI(t, "--config", f.configPath, "runs", "--volume", "a")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 2 runs for volume a, got lines:\n%s", out)
	}
	// Volume is the 3rd whitespace-separated column (ID, KIND, VOLUME, …).
	const volumeColumn = 2
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) <= volumeColumn || fields[volumeColumn] != "a" {
			t.Fatalf("row volume column = %q, want \"a\": full line %q", fields, line)
		}
	}
}

func TestCLIRunsLimit(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	f := writeConfigFor(t, map[string]string{"src": src})
	for range 5 {
		runCLI(t, "--config", f.configPath, "index", "src")
	}

	out := runCLI(t, "--config", f.configPath, "runs", "--limit", "2")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	const wantLines = 3 // header + 2 capped rows
	if len(lines) != wantLines {
		t.Fatalf("--limit 2 produced %d lines, want %d:\n%s", len(lines), wantLines, out)
	}
}

func TestCLIRunsUnknownVolume(t *testing.T) {
	// Open the DB once via an index command so the schema exists.
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	f := writeConfigFor(t, map[string]string{"src": src})
	runCLI(t, "--config", f.configPath, "index", "src")

	_, err := runCLIExpectErr(t, "--config", f.configPath, "runs", "--volume", "no-such-volume")
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

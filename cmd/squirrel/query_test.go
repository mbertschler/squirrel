package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsHashLike(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{strings.Repeat("0", 64), true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("F", 64), true},
		{"deadbeef", false},                    // too short
		{strings.Repeat("0", 63), false},       // off by one
		{strings.Repeat("0", 65), false},       // off by one
		{strings.Repeat("g", 64), false},       // non-hex char
		{strings.Repeat("0", 63) + "z", false}, // non-hex tail
		{"/Users/me/Pictures/foo.jpg", false},  // path
		{"", false},
	}
	for _, c := range cases {
		if got := isHashLike(c.in); got != c.want {
			t.Errorf("isHashLike(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestJoinVolumePath(t *testing.T) {
	cases := []struct {
		volume, rel, want string
	}{
		{"/a", "b", "/a/b"},
		{"/a/b", "c/d", "/a/b/c/d"},
		{"/root", ".", "/root"}, // root row uses path = '.'
		{"/root", "", "/root"},  // empty rel
	}
	for _, c := range cases {
		if got := joinVolumePath(c.volume, c.rel); got != c.want {
			t.Errorf("joinVolumePath(%q, %q) = %q, want %q", c.volume, c.rel, got, c.want)
		}
	}
}

// A 64-character hex string that happens to exist as a filename must be
// routed as a path, not silently interpreted as a digest. Content-addressed
// workloads (exactly this tool's target) make this a real case.
func TestCLIQueryPrefersExistingPathOverHashLike(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	hexName := strings.Repeat("a", 64)
	hashLikePath := filepath.Join(src, hexName)
	writeTestFile(t, hashLikePath, "payload")

	db := filepath.Join(tmp, "test.db")
	runCLI(t, "index", "--db", db, src)

	out := runCLI(t, "query", "--db", db, hashLikePath)
	if !strings.Contains(out, "path:") || !strings.Contains(out, hexName) {
		t.Fatalf("absolute hex-named path lookup failed; output:\n%s", out)
	}

	// A 64-hex string with no file on disk falls through to the hash branch.
	bogusHex := strings.Repeat("b", 64)
	_, output := runCLIExpectErr(t, "query", "--db", db, bogusHex)
	if !strings.Contains(output, "no rows for blake3") {
		t.Fatalf("expected hash-branch error message, got:\n%s", output)
	}
}

// TestCLIQueryHistoryShowsSupersededRows checks the --history flag adds a
// per-row history table for a path. After two indexings of a modified file
// the table should contain both the live and the superseded content.
func TestCLIQueryHistoryShowsSupersededRows(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(src, "doc.txt")
	writeTestFile(t, doc, "version A")
	db := filepath.Join(tmp, "test.db")
	runCLI(t, "index", "--db", db, src)
	writeTestFile(t, doc, "version B is different")
	runCLI(t, "index", "--db", db, src)

	out := runCLI(t, "query", "--db", db, "--history", doc)
	if !strings.Contains(out, "history:") {
		t.Fatalf("expected history: section, got:\n%s", out)
	}
	// At least the live row plus one superseded row should appear.
	if !strings.Contains(out, "superseded") {
		t.Fatalf("expected a superseded row in history, got:\n%s", out)
	}
	if !strings.Contains(out, "present") {
		t.Fatalf("expected a present row in history, got:\n%s", out)
	}
}

// TestCLIQueryHistoryRejectsHashLookup confirms the flag is not meaningful
// for hash queries (those already return every row for the digest).
func TestCLIQueryHistoryRejectsHashLookup(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	db := filepath.Join(tmp, "test.db")
	runCLI(t, "index", "--db", db, src)

	// Lift the blake3 hash out via a normal path query.
	out := runCLI(t, "query", "--db", db, filepath.Join(src, "a.txt"))
	hex := extractField(t, out, "blake3:")

	err, msg := runCLIExpectErr(t, "query", "--db", db, "--history", hex)
	if !strings.Contains(err.Error(), "applies to path queries") {
		t.Fatalf("unexpected error %v; output:\n%s", err, msg)
	}
}

func TestCLIQueryUnknownPath(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "test.db")
	err, _ := runCLIExpectErr(t, "query", "--db", db, filepath.Join(tmp, "nope.txt"))
	if !strings.Contains(err.Error(), "no row for path") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

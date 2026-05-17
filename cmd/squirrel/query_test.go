package main

import (
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
	src := t.TempDir()
	hexName := strings.Repeat("a", 64)
	hashLikePath := filepath.Join(src, hexName)
	writeTestFile(t, hashLikePath, "payload")

	f := writeConfigFor(t, map[string]string{"src": src})
	runCLI(t, "--config", f.configPath, "index", "src")

	out := runCLI(t, "--config", f.configPath, "query", hashLikePath)
	if !strings.Contains(out, "path:") || !strings.Contains(out, hexName) {
		t.Fatalf("absolute hex-named path lookup failed; output:\n%s", out)
	}

	// A 64-hex string with no file on disk falls through to the hash branch.
	bogusHex := strings.Repeat("b", 64)
	output, _ := runCLIExpectErr(t, "--config", f.configPath, "query", bogusHex)
	if !strings.Contains(output, "no rows for blake3") {
		t.Fatalf("expected hash-branch error message, got:\n%s", output)
	}
}

// TestCLIQueryHistoryShowsSupersededRows checks the --history flag adds a
// per-row history table for a path. After two indexings of a modified file
// the table should contain both the live and the superseded content.
func TestCLIQueryHistoryShowsSupersededRows(t *testing.T) {
	src := t.TempDir()
	doc := filepath.Join(src, "doc.txt")
	writeTestFile(t, doc, "version A")
	f := writeConfigFor(t, map[string]string{"src": src})
	runCLI(t, "--config", f.configPath, "index", "src")
	writeTestFile(t, doc, "version B is different")
	runCLI(t, "--config", f.configPath, "index", "src")

	out := runCLI(t, "--config", f.configPath, "query", "--history", doc)
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
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	f := writeConfigFor(t, map[string]string{"src": src})
	runCLI(t, "--config", f.configPath, "index", "src")

	// Lift the blake3 hash out via a normal path query.
	out := runCLI(t, "--config", f.configPath, "query", filepath.Join(src, "a.txt"))
	hex := extractField(t, out, "blake3:")

	msg, err := runCLIExpectErr(t, "--config", f.configPath, "query", "--history", hex)
	if !strings.Contains(err.Error(), "applies to path queries") {
		t.Fatalf("unexpected error %v; output:\n%s", err, msg)
	}
}

func TestCLIQueryUnknownPath(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "test.db")
	_, err := runCLIExpectErr(t, "query", "--db", db, filepath.Join(tmp, "nope.txt"))
	if !strings.Contains(err.Error(), "no row for path") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

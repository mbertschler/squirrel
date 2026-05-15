package main

import (
	"bytes"
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
		{"deadbeef", false},                                  // too short
		{strings.Repeat("0", 63), false},                     // too short by 1
		{strings.Repeat("0", 65), false},                     // too long by 1
		{strings.Repeat("g", 64), false},                     // non-hex
		{strings.Repeat("0", 63) + "z", false},               // non-hex
		{"/Users/me/Pictures/foo.jpg", false},                // path
		{"", false},                                          // empty
	}
	for _, c := range cases {
		if got := isHashLike(c.in); got != c.want {
			t.Errorf("isHashLike(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestJoinRootPath(t *testing.T) {
	cases := []struct {
		root, rel, want string
	}{
		{"/a", "b", "/a/b"},
		{"/a/b", "c/d", "/a/b/c/d"},
		{"/root", ".", "/root"}, // root row uses path = '.'
		{"/root", "", "/root"},  // empty rel
	}
	for _, c := range cases {
		if got := joinRootPath(c.root, c.rel); got != c.want {
			t.Errorf("joinRootPath(%q, %q) = %q, want %q", c.root, c.rel, got, c.want)
		}
	}
}

// runCLI executes the cobra root with the given args and returns combined stdout+stderr.
func runCLI(t *testing.T, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("CLI %v failed: %v\noutput:\n%s", args, err, buf.String())
	}
	return buf.String()
}

func TestCLIIndexAndQueryRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	writeTestFile(t, filepath.Join(src, "b.txt"), "world")
	writeTestFile(t, filepath.Join(src, "c.txt"), "world") // duplicate of b

	db := filepath.Join(tmp, "test.db")

	out := runCLI(t, "index", "--db", db, src)
	if !strings.Contains(out, "added=3") {
		t.Fatalf("index output missing added=3: %s", out)
	}

	// query by path returns blake3 line; pull the hex out.
	out = runCLI(t, "query", "--db", db, filepath.Join(src, "a.txt"))
	hex := extractField(t, out, "blake3:")
	if len(hex) != 64 {
		t.Fatalf("blake3 hex length = %d, want 64: %q", len(hex), hex)
	}

	// query by hex round-trips back to a row with the same path.
	out = runCLI(t, "query", "--db", db, hex)
	if !strings.Contains(out, filepath.Join(src, "a.txt")) {
		t.Fatalf("hex lookup missing path: %s", out)
	}

	// duplicates lists b and c under one hex header.
	out = runCLI(t, "query", "--db", db, "--duplicates")
	if !strings.Contains(out, filepath.Join(src, "b.txt")) ||
		!strings.Contains(out, filepath.Join(src, "c.txt")) {
		t.Fatalf("duplicates missing expected paths:\n%s", out)
	}
}

func TestCLIQueryPrefersExistingPathOverHashLike(t *testing.T) {
	// A file whose name is exactly 64 hex chars must be routable as a path,
	// not silently interpreted as a digest. This is the workload of any
	// content-addressed store and exactly where the disambiguation matters.
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

	// Absolute path with the hex-named file: must route to path lookup.
	out := runCLI(t, "query", "--db", db, hashLikePath)
	if !strings.Contains(out, "path:") || !strings.Contains(out, hexName) {
		t.Fatalf("absolute hex-named path lookup failed; output:\n%s", out)
	}

	// Bare relative arg that exists on disk in cwd: also route to path.
	// Use os.Chdir-free approach: pass the absolute file via os.Stat already
	// handled above. Here we verify a 64-hex string that does NOT exist falls
	// through to the hash branch.
	bogusHex := strings.Repeat("b", 64)
	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"query", "--db", db, bogusHex})
	if err := root.Execute(); err == nil {
		t.Fatalf("expected error for unknown hash %q, got nil; output: %s", bogusHex, buf.String())
	}
	if !strings.Contains(buf.String(), "no rows for blake3") {
		t.Fatalf("expected hash-branch error message, got:\n%s", buf.String())
	}
}

func TestCLIQueryUnknownPath(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "test.db")
	// Opening with no data is fine; query an absent path.
	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"query", "--db", db, filepath.Join(tmp, "nope.txt")})
	err := root.Execute()
	if err == nil {
		t.Fatalf("expected error querying unknown path, got nil; output: %s", buf.String())
	}
	if !strings.Contains(err.Error(), "no row for path") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func extractField(t *testing.T, out, prefix string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("field %q not found in output:\n%s", prefix, out)
	return ""
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

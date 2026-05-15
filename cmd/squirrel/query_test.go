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

func TestCLIQueryUnknownPath(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "test.db")
	err, _ := runCLIExpectErr(t, "query", "--db", db, filepath.Join(tmp, "nope.txt"))
	if !strings.Contains(err.Error(), "no row for path") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

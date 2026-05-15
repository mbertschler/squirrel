package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI executes the cobra root with the given args and returns combined
// stdout+stderr. Shared by the per-subcommand tests in this package.
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

// runCLIExpectErr executes the cobra root expecting a non-nil error, and
// returns (err, captured output).
func runCLIExpectErr(t *testing.T, args ...string) (error, string) {
	t.Helper()
	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		t.Fatalf("CLI %v unexpectedly succeeded; output:\n%s", args, buf.String())
	}
	return err, buf.String()
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
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

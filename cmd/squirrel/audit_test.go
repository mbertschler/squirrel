package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLIAuditRecordsAuditRun is the issue #17 acceptance fixture:
// a file is recorded under blake3 X, then modified out-of-band to
// blake3 Y, then `squirrel audit <volume>` detects the drift, with
// the supersede chain recording both versions and the run row tagged
// as kind='audit'. The runs listing shows the new row.
func TestCLIAuditRecordsAuditRun(t *testing.T) {
	src := t.TempDir()
	doc := filepath.Join(src, "doc.md")
	writeTestFile(t, doc, "version-X")

	f := writeConfigFor(t, map[string]string{"src": src})

	// Initial index records doc.md at version-X.
	runCLI(t, "--config", f.configPath, "index", "src")

	// Out-of-band modification.
	writeTestFile(t, doc, "version-Y-drift")

	// Audit picks it up.
	out := runCLI(t, "--config", f.configPath, "audit", "src")
	if !strings.Contains(out, "modified=1") {
		t.Fatalf("audit output missing modified=1: %s", out)
	}

	// runs listing shows the audit row alongside the index row.
	runsOut := runCLI(t, "--config", f.configPath, "runs")
	if !strings.Contains(runsOut, "audit") {
		t.Fatalf("runs listing missing 'audit' kind:\n%s", runsOut)
	}
	if !strings.Contains(runsOut, "index") {
		t.Fatalf("runs listing missing 'index' kind:\n%s", runsOut)
	}
}

// TestCLIAuditDeepFlag covers the --deep path: a file whose
// (size, mtime) is unchanged but whose bytes were rewritten in place
// (a contrived bit-rot stand-in) is only caught when --deep forces a
// re-hash. Without --deep the shallow shortcut skips the file.
func TestCLIAuditDeepFlag(t *testing.T) {
	src := t.TempDir()
	doc := filepath.Join(src, "doc.md")
	writeTestFile(t, doc, "version-X")
	info, err := os.Stat(doc)
	if err != nil {
		t.Fatal(err)
	}
	originalMtime := info.ModTime()

	f := writeConfigFor(t, map[string]string{"src": src})
	runCLI(t, "--config", f.configPath, "index", "src")

	// Rewrite with same length and the same mtime — the (size, mtime)
	// shortcut treats this as unchanged.
	if err := os.WriteFile(doc, []byte("Version-Y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(doc, originalMtime, originalMtime); err != nil {
		t.Fatal(err)
	}

	// Default audit: shallow shortcut misses the drift.
	out := runCLI(t, "--config", f.configPath, "audit", "src")
	if !strings.Contains(out, "modified=0") {
		t.Fatalf("shallow audit reported drift: %s", out)
	}

	// --deep re-hashes, detects the change.
	out = runCLI(t, "--config", f.configPath, "audit", "src", "--deep")
	if !strings.Contains(out, "modified=1") {
		t.Fatalf("deep audit missed drift: %s", out)
	}
}

// TestCLIAuditUnknownVolume mirrors index's guard: an unknown volume
// name fails fast rather than silently no-op'ing.
func TestCLIAuditUnknownVolume(t *testing.T) {
	f := writeConfigFor(t, map[string]string{"declared": t.TempDir()})
	_, err := runCLIExpectErr(t, "--config", f.configPath, "audit", "missing")
	if !strings.Contains(err.Error(), `unknown volume "missing"`) {
		t.Fatalf("expected unknown-volume error, got %v", err)
	}
}

// TestCLIAuditNoArgsFansOut runs `squirrel audit` (no positional
// volume) against a config with two volumes and confirms both are
// walked in a single invocation.
func TestCLIAuditNoArgsFansOut(t *testing.T) {
	srcA := t.TempDir()
	srcB := t.TempDir()
	writeTestFile(t, filepath.Join(srcA, "a.txt"), "alpha")
	writeTestFile(t, filepath.Join(srcB, "b.txt"), "beta")
	f := writeConfigFor(t, map[string]string{"a": srcA, "b": srcB})

	out := runCLI(t, "--config", f.configPath, "audit")
	if !strings.Contains(out, "audit a:") || !strings.Contains(out, "audit b:") {
		t.Fatalf("audit output missing one of the volumes:\n%s", out)
	}
}

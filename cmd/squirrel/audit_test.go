package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
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

// TestCLIAuditFoldersClean: a freshly indexed volume's stored folder
// hashes match a re-derivation, so `audit --folders` reports consistent
// and exits zero (M2 happy path).
func TestCLIAuditFoldersClean(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "a.txt"), "alpha")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(src, "sub", "b.txt"), "beta")
	f := writeConfigFor(t, map[string]string{"src": src})
	runCLI(t, "--config", f.configPath, "index", "src")

	out := runCLI(t, "--config", f.configPath, "audit", "src", "--folders")
	if !strings.Contains(out, "audit src --folders: consistent") {
		t.Fatalf("clean folder audit not reported consistent:\n%s", out)
	}
}

// TestCLIAuditFoldersDetectsCorruption: corrupting one folder's stored
// shallow hash makes `audit --folders` report the divergence and exit
// non-zero (M2 acceptance).
func TestCLIAuditFoldersDetectsCorruption(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(src, "sub", "b.txt"), "beta")
	f := writeConfigFor(t, map[string]string{"src": src})
	runCLI(t, "--config", f.configPath, "index", "src")

	corruptFolderShallowHash(t, f.dbPath, "sub")

	out, err := runCLIExpectErr(t, "--config", f.configPath, "audit", "src", "--folders")
	if err == nil {
		t.Fatalf("corrupt folder audit unexpectedly succeeded:\n%s", out)
	}
	if !strings.Contains(out, "diverged") || !strings.Contains(out, "sub shallow") {
		t.Fatalf("corrupt folder audit output missing divergence detail:\n%s", out)
	}
}

// corruptFolderShallowHash flips one byte of the stored shallow_blake3
// for the folder at the given path, simulating index corruption /
// bit-rot of the Merkle column. Opens the DB directly (the store's hash
// columns are deliberately immutable through the public API) and writes
// the mutated digest back.
func corruptFolderShallowHash(t *testing.T, dbPath, folderPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db for corruption: %v", err)
	}
	defer db.Close()
	var stored []byte
	if err := db.QueryRow(
		`SELECT shallow_blake3 FROM folders WHERE path = ?`, folderPath).Scan(&stored); err != nil {
		t.Fatalf("read folder %q shallow hash: %v", folderPath, err)
	}
	if len(stored) != 32 {
		t.Fatalf("folder %q shallow hash len = %d, want 32", folderPath, len(stored))
	}
	stored[0] ^= 0xFF
	if _, err := db.Exec(
		`UPDATE folders SET shallow_blake3 = ? WHERE path = ?`, stored, folderPath); err != nil {
		t.Fatalf("corrupt folder %q shallow hash: %v", folderPath, err)
	}
}

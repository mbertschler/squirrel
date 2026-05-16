package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLIRestoreRoundTrip is the end-to-end smoke test: sync a volume,
// remove the local copy, restore from the destination, and verify the
// contents match.
func TestCLIRestoreRoundTrip(t *testing.T) {
	requireRcloneCLI(t)
	f := writeSyncFixture(t)
	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")
	writeTestFile(t, filepath.Join(f.volumeDir, "b.txt"), "beta")

	runCLI(t, "--config", f.configPath, "index", f.volumeName)
	runCLI(t, "--config", f.configPath, "sync", "pics")

	// Wipe the local volume so the restore has work to do.
	if err := os.RemoveAll(f.volumeDir); err != nil {
		t.Fatalf("remove volume: %v", err)
	}
	if err := os.MkdirAll(f.volumeDir, 0o755); err != nil {
		t.Fatalf("recreate volume: %v", err)
	}

	out := runCLI(t, "--config", f.configPath, "restore", "pics")
	if !strings.Contains(out, "status=success") {
		t.Fatalf("restore did not succeed:\n%s", out)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		path := filepath.Join(f.volumeDir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("missing restored %s: %v", path, err)
		}
		if len(body) == 0 {
			t.Fatalf("restored %s is empty", path)
		}
	}
}

// TestCLIRestoreToPathOverridesVolumePath confirms --to writes to the
// override directory rather than the volume's declared path.
func TestCLIRestoreToPathOverridesVolumePath(t *testing.T) {
	requireRcloneCLI(t)
	f := writeSyncFixture(t)
	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")

	runCLI(t, "--config", f.configPath, "index", f.volumeName)
	runCLI(t, "--config", f.configPath, "sync", "pics")

	target := filepath.Join(t.TempDir(), "recovered")
	out := runCLI(t, "--config", f.configPath, "restore", "pics", "--to", target)
	if !strings.Contains(out, "status=success") {
		t.Fatalf("restore did not succeed:\n%s", out)
	}
	body, err := os.ReadFile(filepath.Join(target, "a.txt"))
	if err != nil {
		t.Fatalf("override target missing a.txt: %v", err)
	}
	if string(body) != "alpha" {
		t.Fatalf("override target a.txt = %q, want alpha", body)
	}
	// Live volume path should still have its (pre-restore) bytes.
	live, err := os.ReadFile(filepath.Join(f.volumeDir, "a.txt"))
	if err != nil || string(live) != "alpha" {
		t.Fatalf("live volume changed unexpectedly: %q err=%v", live, err)
	}
}

// TestCLIRestoreRecordsRunsRowWithKindRestore verifies that the restore
// command produces a runs row distinguishable from sync runs.
func TestCLIRestoreRecordsRunsRowWithKindRestore(t *testing.T) {
	requireRcloneCLI(t)
	f := writeSyncFixture(t)
	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")
	runCLI(t, "--config", f.configPath, "index", f.volumeName)
	runCLI(t, "--config", f.configPath, "sync", "pics")
	runCLI(t, "--config", f.configPath, "restore", "pics", "--to", filepath.Join(t.TempDir(), "recovered"))

	out := runCLI(t, "--config", f.configPath, "runs")
	if !strings.Contains(out, "restore") {
		t.Fatalf("runs output missing 'restore' kind:\n%s", out)
	}
}

// TestCLIRestoreNeedsExplicitFromWhenAmbiguous: when a volume syncs to
// multiple destinations and the user does not pass --from, the command
// must error and tell the user to disambiguate.
func TestCLIRestoreNeedsExplicitFromWhenAmbiguous(t *testing.T) {
	root := t.TempDir()
	volumeDir := filepath.Join(root, "pics")
	if err := os.MkdirAll(volumeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.toml")
	dbPath := filepath.Join(root, "index.db")
	body := "" +
		"db = \"" + dbPath + "\"\n\n" +
		"[destinations.a]\ntype = \"local\"\nroot = \"" + filepath.Join(root, "a") + "\"\n\n" +
		"[destinations.b]\ntype = \"local\"\nroot = \"" + filepath.Join(root, "b") + "\"\n\n" +
		"[volumes.pics]\npath = \"" + volumeDir + "\"\nsync_to = [\"a\", \"b\"]\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	err, _ := runCLIExpectErr(t, "--config", cfgPath, "restore", "pics")
	if !strings.Contains(err.Error(), "multiple destinations") {
		t.Fatalf("expected multiple-destinations error, got %v", err)
	}
}

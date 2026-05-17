package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireRcloneCLI mirrors sync.requireRclone but for the CLI tests in
// this package. Sync tests against the real rclone binary; without it
// installed, the test is skipped (and that's surfaced in test output).
func requireRcloneCLI(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("rclone not on PATH; install rclone ≥ 1.66 to run these tests")
	}
}

// writeSyncFixture lays out a workspace with: a squirrel config file
// pointing at a `pics` volume and a `scratch` local destination, the
// volume directory itself with one file, an empty destination directory,
// and a fresh DB path. Returns the config and db paths so callers can
// pass them via --config / --db on the CLI.
type syncFixturePaths struct {
	volumeDir  string
	destDir    string
	configPath string
	dbPath     string
	volumeName string
}

func writeSyncFixture(t *testing.T) syncFixturePaths {
	t.Helper()
	root := t.TempDir()
	// Use "pics" as the path basename so the volume row's auto-derived
	// name lines up with the config-declared name. After commit (e) the
	// indexer takes the name from config and this aliasing won't matter.
	volumeDir := filepath.Join(root, "pics")
	if err := os.MkdirAll(volumeDir, 0o755); err != nil {
		t.Fatalf("mkdir volume: %v", err)
	}
	destDir := filepath.Join(root, "dst")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	dbPath := filepath.Join(root, "index.db")
	configPath := filepath.Join(root, "config.toml")
	body := "" +
		"db = \"" + dbPath + "\"\n\n" +
		"[destinations.scratch]\n" +
		"type = \"local\"\n" +
		"root = \"" + destDir + "\"\n\n" +
		"[volumes.pics]\n" +
		"path = \"" + volumeDir + "\"\n" +
		"sync_to = [\"scratch\"]\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return syncFixturePaths{
		volumeDir: volumeDir, destDir: destDir,
		configPath: configPath, dbPath: dbPath, volumeName: "pics",
	}
}

func TestCLISyncErrorsWhenVolumeNotIndexed(t *testing.T) {
	requireRcloneCLI(t)
	f := writeSyncFixture(t)
	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")

	out, err := runCLIExpectErr(t, "--config", f.configPath, "sync", "pics")
	if !strings.Contains(err.Error(), "never been indexed") &&
		!strings.Contains(out, "never been indexed") {
		t.Fatalf("expected unindexed-volume error, got err=%v out=%q", err, out)
	}
}

func TestCLISyncHappyPath(t *testing.T) {
	requireRcloneCLI(t)
	f := writeSyncFixture(t)
	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")
	writeTestFile(t, filepath.Join(f.volumeDir, "b.txt"), "beta")

	runCLI(t, "--config", f.configPath, "index", f.volumeName)
	out := runCLI(t, "--config", f.configPath, "sync", "pics")
	if !strings.Contains(out, "status=success") {
		t.Fatalf("sync did not report success: %s", out)
	}
	if !strings.Contains(out, "transferred=2") {
		t.Fatalf("expected transferred=2 in summary: %s", out)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		path := filepath.Join(f.destDir, f.volumeName, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("destination missing %s: %v", path, err)
		}
	}
}

func TestCLISyncUnknownDestinationFlag(t *testing.T) {
	f := writeSyncFixture(t)
	_, err := runCLIExpectErr(t, "--config", f.configPath, "sync", "pics", "--to", "ghost")
	if !strings.Contains(err.Error(), "unknown destination") {
		t.Fatalf("expected unknown-destination error, got %v", err)
	}
}

func TestCLISyncRequiresConfig(t *testing.T) {
	// No config file at the chosen --config path: sync errors with a
	// pointer to the missing file instead of a generic IO error.
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "no-config.toml")
	_, err := runCLIExpectErr(t, "--config", missing, "sync", "pics")
	if !strings.Contains(err.Error(), "no config at") {
		t.Fatalf("expected missing-config error, got %v", err)
	}
}

// TestCLISyncDryRun is the CLI counterpart to the dry-run sync test in
// sync/. We verify (a) the command succeeds and prints a report, (b)
// nothing lands at the destination, and (c) no new runs row is recorded.
func TestCLISyncDryRun(t *testing.T) {
	requireRcloneCLI(t)
	f := writeSyncFixture(t)
	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")
	runCLI(t, "--config", f.configPath, "index", f.volumeName)

	runsBefore := runCLI(t, "--config", f.configPath, "runs")
	out := runCLI(t, "--config", f.configPath, "sync", "pics", "--dry-run")
	if !strings.Contains(out, "pics → scratch") {
		t.Fatalf("dry-run did not print pair line:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(f.destDir, f.volumeName, "a.txt")); err == nil {
		t.Fatalf("dry-run wrote to destination; want no-op")
	}
	runsAfter := runCLI(t, "--config", f.configPath, "runs")
	if runsBefore != runsAfter {
		t.Fatalf("dry-run added a runs row:\nbefore:\n%s\nafter:\n%s", runsBefore, runsAfter)
	}
}

// TestCLISyncMissingExplicitConfigErrors checks the tryLoadConfig
// disambiguation: a user-supplied --config path that doesn't exist must
// be an error rather than silently degrading to no-config behavior.
// This also indirectly covers the path-validation message format.
func TestCLISyncMissingExplicitConfigErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.toml")
	_, err := runCLIExpectErr(t, "--config", missing, "query", "--db", filepath.Join(t.TempDir(), "x.db"), "abc")
	if !strings.Contains(err.Error(), "no config at") {
		t.Fatalf("expected missing-config error when --config is explicit, got %v", err)
	}
}

func TestCLISyncRunsRowVisibleViaRunsCommand(t *testing.T) {
	requireRcloneCLI(t)
	f := writeSyncFixture(t)
	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")

	runCLI(t, "--config", f.configPath, "index", f.volumeName)
	runCLI(t, "--config", f.configPath, "sync", "pics")

	// `squirrel runs` should now show one sync row with destination=scratch.
	out := runCLI(t, "--config", f.configPath, "runs")
	if !strings.Contains(out, "sync") {
		t.Fatalf("runs output missing 'sync' kind: %s", out)
	}
	if !strings.Contains(out, "scratch") {
		t.Fatalf("runs output missing 'scratch' destination: %s", out)
	}
}

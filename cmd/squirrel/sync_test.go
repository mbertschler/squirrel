package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

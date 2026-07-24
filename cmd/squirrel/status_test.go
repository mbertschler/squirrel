package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStatusConfig lays down a volume with a sync destination, plus its
// DB, and returns the config path. Callers index the volume themselves.
func writeStatusConfig(t *testing.T) (configPath string) {
	t.Helper()
	root := t.TempDir()
	volumeDir := filepath.Join(root, "pics")
	if err := os.MkdirAll(volumeDir, 0o755); err != nil {
		t.Fatalf("mkdir volume: %v", err)
	}
	writeTestFile(t, filepath.Join(volumeDir, "a.txt"), "hello")
	writeTestFile(t, filepath.Join(volumeDir, "b.txt"), "world")

	destDir := filepath.Join(root, "dst")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	dbPath := filepath.Join(root, "index.db")
	configPath = filepath.Join(root, "config.toml")
	body := "" +
		"db = \"" + dbPath + "\"\n\n" +
		"[destinations.scratch]\n" +
		"type = \"local\"\n" +
		"root = \"" + destDir + "\"\n\n" +
		"[volumes.pics]\n" +
		"path = \"" + volumeDir + "\"\n" +
		"sync_to = [\"scratch\"]\n" +
		"sync_every = \"1h\"\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return configPath
}

// TestCLIStatusNeverSyncedIsAmber: a configured but never-synced target
// leaves the volume amber, and the command exits with code 1 carried by an
// exitCodeError.
func TestCLIStatusNeverSyncedIsAmber(t *testing.T) {
	cfg := writeStatusConfig(t)
	runCLI(t, "--config", cfg, "index", "pics")

	out, err := runCLIExpectErr(t, "--config", cfg, "status")
	var ec exitCodeError
	if !errors.As(err, &ec) {
		t.Fatalf("expected exitCodeError, got %v", err)
	}
	if ec.code != 1 {
		t.Fatalf("exit code = %d, want 1 (amber)", ec.code)
	}
	if !strings.Contains(out, "pics") || !strings.Contains(out, "scratch") {
		t.Fatalf("grid missing volume/target:\n%s", out)
	}
	if !strings.Contains(out, "never") {
		t.Fatalf("never-synced target should read 'never':\n%s", out)
	}
	if !strings.Contains(out, "overall: amber") {
		t.Fatalf("summary should be amber:\n%s", out)
	}
	// Cobra's own "Error:" line must be silenced — only the grid prints.
	if strings.Contains(out, "Error:") {
		t.Fatalf("cobra error line leaked into output:\n%s", out)
	}
}

// TestCLIStatusAfterSyncIsGreen: once a fresh sync lands, the pair is
// caught up within cadence and the command exits 0.
func TestCLIStatusAfterSyncIsGreen(t *testing.T) {
	requireRcloneCLI(t)
	cfg := writeStatusConfig(t)
	runCLI(t, "--config", cfg, "index", "pics")
	runCLI(t, "--config", cfg, "sync", "pics", "--init")

	out := runCLI(t, "--config", cfg, "status")
	if !strings.Contains(out, "overall: green") {
		t.Fatalf("expected green after a fresh sync:\n%s", out)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("expected an 'ok' target state:\n%s", out)
	}
}

// TestCLIStatusUnknownVolume errors clearly on a volume not in config.
// SilenceErrors (needed to keep the amber/red exit-code signal quiet) must
// not swallow this genuine error: it has to reach the user's stderr, not
// just the returned error value.
func TestCLIStatusUnknownVolume(t *testing.T) {
	cfg := writeStatusConfig(t)
	out, err := runCLIExpectErr(t, "--config", cfg, "status", "nope")
	if err == nil || !strings.Contains(err.Error(), `unknown volume "nope"`) {
		t.Fatalf("expected unknown-volume error, got %v", err)
	}
	if !strings.Contains(out, `unknown volume "nope"`) {
		t.Fatalf("error must reach the user, not be silenced:\n%s", out)
	}
}

// TestCLIStatusRequiresConfig: status is a question about configured
// coverage, so a missing config is a clear error, not an empty grid — and
// the message must be printed, not silently swallowed by SilenceErrors.
func TestCLIStatusRequiresConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-config.toml")
	out, err := runCLIExpectErr(t, "--config", missing, "status")
	if err == nil || !strings.Contains(err.Error(), "no config at") {
		t.Fatalf("expected missing-config error, got %v", err)
	}
	if !strings.Contains(out, "no config at") {
		t.Fatalf("error must reach the user, not be silenced:\n%s", out)
	}
}

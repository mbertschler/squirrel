package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/store"
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

// TestCLIStatusReportsConfigDrift: an operator who edits the config and
// forgets to restart the agent is told so by `squirrel status` — on every
// scope, including a single volume, and with the amber exit code (F9).
func TestCLIStatusReportsConfigDrift(t *testing.T) {
	cfg := writeStatusConfig(t)
	runCLI(t, "--config", cfg, "index", "pics")
	raiseTestConfigDrift(t, filepath.Join(filepath.Dir(cfg), "index.db"), cfg)

	for _, args := range [][]string{
		{"--config", cfg, "status"},
		{"--config", cfg, "status", "pics"},
	} {
		out, err := runCLIExpectErr(t, args...)
		var ec exitCodeError
		if !errors.As(err, &ec) || ec.code != 1 {
			t.Fatalf("%v: exit = %v, want amber (1)", args, err)
		}
		if !strings.Contains(out, "restart to apply") || !strings.Contains(out, cfg) {
			t.Fatalf("%v: status does not report the drift:\n%s", args, out)
		}
	}
}

// raiseTestConfigDrift opens the CLI's DB directly and latches config drift
// as the agent's monitor would, so the introspection surfaces have a
// standing latch to render without running an agent. The handle is closed
// before returning so the next CLI invocation has no concurrent connection
// from this process.
func raiseTestConfigDrift(t *testing.T, dbPath, configPath string) {
	t.Helper()
	s, err := store.OpenWithOptions(dbPath, store.OpenOptions{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	loaded, disk := make([]byte, 32), make([]byte, 32)
	disk[0] = 1
	if _, err := s.RaiseConfigDrift(context.Background(), store.ConfigDriftState{Path: configPath, Loaded: loaded, Disk: disk}); err != nil {
		t.Fatalf("RaiseConfigDrift: %v", err)
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

// seedFleetEvidence records what a verified sync to the destination would
// leave behind — a durability component covering the volume's present set
// and a successful sync run — without needing rclone on the test machine.
func seedFleetEvidence(t *testing.T, dbPath, destination string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.OpenWithOptions(dbPath, store.OpenOptions{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	vol, err := s.GetVolumeByName(ctx, "pics")
	if err != nil {
		t.Fatalf("GetVolumeByName: %v", err)
	}
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	components, err := s.PresentOriginMaxima(ctx, vol.ID, self.ID)
	if err != nil {
		t.Fatalf("PresentOriginMaxima: %v", err)
	}
	if err := s.AdvanceDestinationVectorTo(ctx, vol.ID, destination, store.VerifyMethodBlake3, components); err != nil {
		t.Fatalf("AdvanceDestinationVectorTo: %v", err)
	}
	id, err := s.BeginRun(ctx, store.RunKindSync, vol.ID, destination, false)
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	if err := s.FinishRun(ctx, id, store.RunStatusSuccess, "", 2); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}

// TestCLIStatusRendersFleetBlock: under the target grid, `squirrel status`
// answers where else the volume lives and how current that copy is — the
// fleet block (#187), rendered from the same query layer as the grid.
func TestCLIStatusRendersFleetBlock(t *testing.T) {
	cfg := writeStatusConfig(t)
	runCLI(t, "--config", cfg, "index", "pics")
	seedFleetEvidence(t, filepath.Join(filepath.Dir(cfg), "index.db"), "scratch")

	out := runCLI(t, "--config", cfg, "status")
	if !strings.Contains(out, "FLEET") || !strings.Contains(out, "AS OF") {
		t.Fatalf("status has no fleet block:\n%s", out)
	}
	fleetRow := statusLineWith(t, out, "scratch", "same")
	if !strings.Contains(fleetRow, "destination") {
		t.Errorf("fleet row does not say what kind of place it is: %q", fleetRow)
	}
	if strings.Contains(out, "overall: green") == false {
		t.Errorf("a covered, freshly synced volume should be green:\n%s", out)
	}
}

// TestCLIStatusFleetSaysUnknownNotZero: with nothing ever pushed there, the
// row must say the coverage is unknown rather than render a confident zero
// — the fail-closed reading the whole view depends on.
func TestCLIStatusFleetSaysUnknownNotZero(t *testing.T) {
	cfg := writeStatusConfig(t)
	runCLI(t, "--config", cfg, "index", "pics")

	out, err := runCLIExpectErr(t, "--config", cfg, "status")
	var ec exitCodeError
	if !errors.As(err, &ec) || ec.code != 1 {
		t.Fatalf("exit = %v, want amber (1)", err)
	}
	row := statusLineWith(t, out, "scratch", "unknown")
	if !strings.Contains(row, "—") {
		t.Errorf("unknown coverage must not render as a count: %q", row)
	}
}

// statusLineWith returns the single output line containing both substrings,
// failing the test when no line does.
func statusLineWith(t *testing.T, out, first, second string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, first) && strings.Contains(line, second) {
			return line
		}
	}
	t.Fatalf("no line contains %q and %q:\n%s", first, second, out)
	return ""
}

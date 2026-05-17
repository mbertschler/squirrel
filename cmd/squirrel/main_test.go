package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// requireRcloneCLI mirrors sync.requireRclone but for the CLI tests in
// this package. Sync and restore tests both shell out to the real
// rclone binary; without it installed, the test is skipped (and that's
// surfaced in test output).
func requireRcloneCLI(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("rclone not on PATH; install rclone ≥ 1.66 to run these tests")
	}
}

// syncFixturePaths bundles the paths writeSyncFixture lays down so callers
// can pass them straight to the CLI via --config / --db.
type syncFixturePaths struct {
	volumeDir  string
	destDir    string
	configPath string
	dbPath     string
	volumeName string
}

// writeSyncFixture lays out a workspace with: a squirrel config file
// pointing at a `pics` volume and a `scratch` local destination, the
// volume directory itself, an empty destination directory, and a fresh DB
// path. Shared by sync_test.go and restore_test.go.
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

// isolateConfig points SQUIRREL_CONFIG at a path inside t.TempDir that
// will never exist. Without this, runCLI would inherit the developer's
// real ~/.squirrel/config.toml when a test doesn't pass --config, which
// would silently leak host state (volumes, destinations, db path) into
// tests. t.Setenv ensures the variable is reverted at test end.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("SQUIRREL_CONFIG", filepath.Join(t.TempDir(), "no-config.toml"))
}

// runCLI executes the cobra root with the given args and returns combined
// stdout+stderr. Shared by the per-subcommand tests in this package.
func runCLI(t *testing.T, args ...string) string {
	t.Helper()
	isolateConfig(t)
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
// returns (captured output, err).
func runCLIExpectErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	isolateConfig(t)
	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		t.Fatalf("CLI %v unexpectedly succeeded; output:\n%s", args, buf.String())
	}
	return buf.String(), err
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// configFixture is the result of writeConfigFor: paths the test can pass
// via --config to the CLI, plus the resolved DB path it also wrote into
// the config so tests don't need to also pass --db.
type configFixture struct {
	configPath string
	dbPath     string
}

// writeConfigFor builds a minimal squirrel config containing the given
// volumes (name → absolute path) and a temp DB path. Used by the existing
// CLI tests that previously relied on `squirrel index <path>` — the new
// CLI takes volume names, so the volumes must exist in config first.
func writeConfigFor(t *testing.T, volumes map[string]string) configFixture {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.db")
	configPath := filepath.Join(dir, "config.toml")

	names := make([]string, 0, len(volumes))
	for n := range volumes {
		names = append(names, n)
	}
	sort.Strings(names)

	var body strings.Builder
	fmt.Fprintf(&body, "db = %q\n\n", dbPath)
	for _, n := range names {
		fmt.Fprintf(&body, "[volumes.%s]\npath = %q\n\n", n, volumes[n])
	}
	if err := os.WriteFile(configPath, []byte(body.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return configFixture{configPath: configPath, dbPath: dbPath}
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

	f := writeConfigFor(t, map[string]string{"src": src})

	out := runCLI(t, "--config", f.configPath, "index", "src")
	if !strings.Contains(out, "added=3") {
		t.Fatalf("index output missing added=3: %s", out)
	}

	// query by path returns blake3 line; pull the hex out.
	out = runCLI(t, "--config", f.configPath, "query", filepath.Join(src, "a.txt"))
	hex := extractField(t, out, "blake3:")
	if len(hex) != 64 {
		t.Fatalf("blake3 hex length = %d, want 64: %q", len(hex), hex)
	}

	// query by hex round-trips back to a row with the same path.
	out = runCLI(t, "--config", f.configPath, "query", hex)
	if !strings.Contains(out, filepath.Join(src, "a.txt")) {
		t.Fatalf("hex lookup missing path: %s", out)
	}

	// duplicates lists b and c under one hex header.
	out = runCLI(t, "--config", f.configPath, "query", "--duplicates")
	if !strings.Contains(out, filepath.Join(src, "b.txt")) ||
		!strings.Contains(out, filepath.Join(src, "c.txt")) {
		t.Fatalf("duplicates missing expected paths:\n%s", out)
	}
}

func TestCLIIndexUnknownVolume(t *testing.T) {
	f := writeConfigFor(t, map[string]string{"declared": t.TempDir()})
	_, err := runCLIExpectErr(t, "--config", f.configPath, "index", "missing")
	if !strings.Contains(err.Error(), `unknown volume "missing"`) {
		t.Fatalf("expected unknown-volume error, got %v", err)
	}
}

func TestCLIIndexRequiresConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-config.toml")
	_, err := runCLIExpectErr(t, "--config", missing, "index", "src")
	if !strings.Contains(err.Error(), "no config at") {
		t.Fatalf("expected missing-config error, got %v", err)
	}
}

// TestCLIDBFlagOverridesConfigDB exercises the precedence rule: an
// explicit --db should win over config.db. We point config.db at an
// unwritable path so a precedence regression would surface as an error
// rather than silently writing to the config-declared DB.
func TestCLIDBFlagOverridesConfigDB(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")

	// Build a config where db points at a deliberately bogus path under
	// /dev/null/... — opening the DB at that path would fail. If --db
	// overrides correctly we never touch it.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	body := "" +
		"db = \"/dev/null/cannot-be-a-db\"\n\n" +
		"[volumes.src]\npath = \"" + src + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	overrideDB := filepath.Join(dir, "override.db")

	out := runCLI(t, "--config", cfgPath, "--db", overrideDB, "index", "src")
	if !strings.Contains(out, "added=1") {
		t.Fatalf("index didn't run against the --db override: %s", out)
	}
	if _, err := os.Stat(overrideDB); err != nil {
		t.Fatalf("override DB was not created at %s: %v", overrideDB, err)
	}
}

// TestCLIOrphanVolumeWarning checks that a volume present in the DB but
// not declared in config triggers a stderr advisory on the next
// config-aware command. We bootstrap an orphan by indexing one volume,
// then re-running with a slimmer config that omits it.
func TestCLIOrphanVolumeWarning(t *testing.T) {
	srcA := t.TempDir()
	srcB := t.TempDir()
	writeTestFile(t, filepath.Join(srcA, "a.txt"), "alpha")
	writeTestFile(t, filepath.Join(srcB, "b.txt"), "beta")

	// First config has both volumes; index both.
	full := writeConfigFor(t, map[string]string{"a": srcA, "b": srcB})
	runCLI(t, "--config", full.configPath, "index", "a")
	runCLI(t, "--config", full.configPath, "index", "b")

	// Second config drops 'b' but reuses the same DB so the b row is
	// orphaned. We point --db at the original DB to share state.
	dir := t.TempDir()
	slim := filepath.Join(dir, "config.toml")
	body := "" +
		"db = \"" + full.dbPath + "\"\n\n" +
		"[volumes.a]\npath = \"" + srcA + "\"\n"
	if err := os.WriteFile(slim, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runCLI(t, "--config", slim, "index", "a")
	if !strings.Contains(out, `warning: volume "b"`) {
		t.Fatalf("orphan-volume warning missing for 'b':\n%s", out)
	}
}

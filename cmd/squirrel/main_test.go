package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

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

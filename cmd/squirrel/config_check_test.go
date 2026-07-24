package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCheckConfig writes body to a config.toml in a fresh temp dir and
// returns its path.
func writeCheckConfig(t *testing.T, body string) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return configPath
}

func TestCLIConfigCheckAffirmative(t *testing.T) {
	dir := t.TempDir()
	photos := filepath.Join(dir, "photos")
	dest := filepath.Join(dir, "dest")
	nodePath := filepath.Join(dir, "peer-mount")
	for _, d := range []string{photos, dest, nodePath} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(photos, "a.jpg"), "x")

	body := "" +
		"node_name = \"thisnode\"\n\n" +
		"[volumes.photos]\npath = \"" + photos + "\"\nsync_to = [\"scratch\"]\n\n" +
		"[destinations.scratch]\ntype = \"local\"\nroot = \"" + dest + "\"\n\n" +
		"[nodes.peer]\nendpoint = \"https://peer.home:8443\"\npath = \"" + nodePath + "\"\n" +
		"[nodes.peer.auth]\nbearer = \"tok\"\n"
	cfgPath := writeCheckConfig(t, body)

	out := runCLI(t, "--config", cfgPath, "config", "check")
	if !strings.Contains(out, "1 volumes, 1 destinations, 1 nodes — all resolvable") {
		t.Fatalf("missing affirmative summary:\n%s", out)
	}
}

func TestCLIConfigCheckEmptyVolumeWarns(t *testing.T) {
	dir := t.TempDir()
	photos := filepath.Join(dir, "photos")
	if err := os.MkdirAll(photos, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[volumes.photos]\npath = \"" + photos + "\"\n"
	cfgPath := writeCheckConfig(t, body)

	out := runCLI(t, "--config", cfgPath, "config", "check")
	if !strings.Contains(out, "warn") || !strings.Contains(out, "empty") {
		t.Fatalf("expected empty-volume warning:\n%s", out)
	}
	if !strings.Contains(out, "resolvable, 1 warning") {
		t.Fatalf("expected warning summary:\n%s", out)
	}
}

func TestCLIConfigCheckMissingVolumeFails(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-there")
	body := "[volumes.photos]\npath = \"" + missing + "\"\n"
	cfgPath := writeCheckConfig(t, body)

	out, err := runCLIExpectErr(t, "--config", cfgPath, "config", "check")
	if !strings.Contains(out, "does not exist") {
		t.Fatalf("expected does-not-exist line:\n%s", out)
	}
	if !strings.Contains(err.Error(), "problem") {
		t.Fatalf("expected problem error, got %v", err)
	}
}

// TestCLIConfigCheckOffloadMirrorCrypt exercises the offload_requires
// satisfiability check (F21): a mirror-layout crypt destination can never
// yield offload evidence, so naming it is a fatal misconfiguration.
func TestCLIConfigCheckOffloadMirrorCrypt(t *testing.T) {
	dir := t.TempDir()
	docs := filepath.Join(dir, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(docs, "d.txt"), "x")
	body := "" +
		"[volumes.docs]\npath = \"" + docs + "\"\noffload_requires = [\"cloudbox\"]\n\n" +
		"[destinations.cloudbox]\ntype = \"sftp\"\nhost = \"h\"\nuser = \"u\"\nroot = \"/data\"\n" +
		"[destinations.cloudbox.crypt]\npassword = \"pw\"\n"
	cfgPath := writeCheckConfig(t, body)

	out, err := runCLIExpectErr(t, "--config", cfgPath, "config", "check")
	if !strings.Contains(out, "never yields offload evidence") {
		t.Fatalf("expected offload-evidence problem:\n%s", out)
	}
	if !strings.Contains(err.Error(), "problem") {
		t.Fatalf("expected problem error, got %v", err)
	}
}

// TestCLIConfigCheckReadOnly guards principle 2: `config check` must not
// mutate anything. The config declares a db path that must never be
// created by a check.
func TestCLIConfigCheckReadOnly(t *testing.T) {
	dir := t.TempDir()
	photos := filepath.Join(dir, "photos")
	if err := os.MkdirAll(photos, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(photos, "a.jpg"), "x")
	dbPath := filepath.Join(dir, "index.db")
	body := "db = \"" + dbPath + "\"\n\n[volumes.photos]\npath = \"" + photos + "\"\n"
	cfgPath := writeCheckConfig(t, body)

	runCLI(t, "--config", cfgPath, "config", "check")
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("config check created the database at %s (must be read-only)", dbPath)
	}
}

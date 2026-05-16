package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIVolumesEmpty(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "test.db")
	out := runCLI(t, "volumes", "--db", db)
	if out != "" {
		t.Fatalf("expected empty output for empty DB, got %q", out)
	}
}

func TestCLIVolumesListsConfigDeclaredVolumesAfterIndex(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	for _, dir := range []string{a, b} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(dir, "f.txt"), "hi")
	}
	f := writeConfigFor(t, map[string]string{"a": a, "b": b})
	runCLI(t, "--config", f.configPath, "index", "a")
	runCLI(t, "--config", f.configPath, "index", "b")

	out := runCLI(t, "--config", f.configPath, "volumes")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 volume lines, got %d:\n%s", len(lines), out)
	}
	for i, line := range lines {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			t.Fatalf("line %d not 3-column tab-separated: %q", i, line)
		}
	}
	if !strings.Contains(out, "\ta\t") || !strings.Contains(out, "\tb\t") {
		t.Fatalf("expected volume names 'a' and 'b' in output:\n%s", out)
	}
}

// Two volumes can share a basename when each gets a distinct config name —
// in config-strict mode there is no auto-suffix because the name comes
// directly from the user's config and cannot collide (TOML map keys are
// unique). The DB volume name equals the config name.
func TestCLIVolumesDistinctNamesSharedBasename(t *testing.T) {
	tmp := t.TempDir()
	alpha := filepath.Join(tmp, "alpha", "pictures")
	beta := filepath.Join(tmp, "beta", "pictures")
	for _, d := range []string{alpha, beta} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(d, "f.txt"), "x")
	}
	f := writeConfigFor(t, map[string]string{
		"alpha-pictures": alpha,
		"beta-pictures":  beta,
	})
	runCLI(t, "--config", f.configPath, "index", "alpha-pictures")
	runCLI(t, "--config", f.configPath, "index", "beta-pictures")

	out := runCLI(t, "--config", f.configPath, "volumes")
	if !strings.Contains(out, "\talpha-pictures\t") {
		t.Fatalf("expected 'alpha-pictures' in output:\n%s", out)
	}
	if !strings.Contains(out, "\tbeta-pictures\t") {
		t.Fatalf("expected 'beta-pictures' in output:\n%s", out)
	}
}

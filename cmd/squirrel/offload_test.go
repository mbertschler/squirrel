package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/store"
)

// writeOffloadConfig builds a config with one `pics` volume whose
// offload policy requires the listed targets, returning the fixture
// paths plus the volume directory.
func writeOffloadConfig(t *testing.T, requires []string) (configFixture, string) {
	t.Helper()
	dir := t.TempDir()
	volumeDir := filepath.Join(dir, "pics")
	if err := os.MkdirAll(volumeDir, 0o755); err != nil {
		t.Fatalf("mkdir volume: %v", err)
	}
	dbPath := filepath.Join(dir, "index.db")
	configPath := filepath.Join(dir, "config.toml")
	var body strings.Builder
	fmt.Fprintf(&body, "db = %q\n\n[volumes.pics]\npath = %q\n", dbPath, volumeDir)
	if len(requires) != 0 {
		fmt.Fprintf(&body, "offload_requires = [")
		for i, r := range requires {
			if i > 0 {
				fmt.Fprint(&body, ", ")
			}
			fmt.Fprintf(&body, "%q", r)
		}
		fmt.Fprintln(&body, "]")
	}
	if err := os.WriteFile(configPath, []byte(body.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return configFixture{configPath: configPath, dbPath: dbPath}, volumeDir
}

// seedOffloadEvidence opens the fixture DB directly and records
// durability for the self node at the file's introduction run for every
// target, the same write the destination handlers and the peer
// durability pull perform.
func seedOffloadEvidence(t *testing.T, dbPath, relPath string, targets []string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	v, err := s.GetVolumeByName(ctx, "pics")
	if err != nil {
		t.Fatalf("GetVolumeByName: %v", err)
	}
	row, err := s.GetByPath(ctx, v.ID, relPath)
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	for _, target := range targets {
		if err := s.UpsertDestinationRunID(ctx, v.ID, target, self.ID, row.FirstSeenRunID, false); err != nil {
			t.Fatalf("UpsertDestinationRunID(%s): %v", target, err)
		}
	}
}

func TestCLIOffloadRefusesWithoutPolicy(t *testing.T) {
	f, volumeDir := writeOffloadConfig(t, nil)
	writeTestFile(t, filepath.Join(volumeDir, "a.txt"), "alpha")
	runCLI(t, "--config", f.configPath, "index", "pics")

	out, err := runCLIExpectErr(t, "--config", f.configPath, "offload", "pics", ".")
	if !strings.Contains(err.Error(), "offload_requires") {
		t.Fatalf("err = %v (output %s), want offload_requires refusal", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(volumeDir, "a.txt")); statErr != nil {
		t.Fatalf("a.txt should be untouched: %v", statErr)
	}
}

func TestCLIOffloadHappyPath(t *testing.T) {
	f, volumeDir := writeOffloadConfig(t, []string{"vault"})
	writeTestFile(t, filepath.Join(volumeDir, "a.txt"), "alpha")
	runCLI(t, "--config", f.configPath, "index", "pics")
	seedOffloadEvidence(t, f.dbPath, "a.txt", []string{"vault"})

	out := runCLI(t, "--config", f.configPath, "offload", "pics", ".")
	if !strings.Contains(out, "offloaded a.txt") || !strings.Contains(out, "offloaded=1 not_durable=0 drift=0 errors=0") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(volumeDir, "a.txt")); err == nil {
		t.Fatalf("a.txt should be deleted")
	}
}

func TestCLIOffloadDryRun(t *testing.T) {
	f, volumeDir := writeOffloadConfig(t, []string{"vault"})
	writeTestFile(t, filepath.Join(volumeDir, "a.txt"), "alpha")
	runCLI(t, "--config", f.configPath, "index", "pics")
	seedOffloadEvidence(t, f.dbPath, "a.txt", []string{"vault"})

	out := runCLI(t, "--config", f.configPath, "offload", "pics", ".", "--dry-run")
	if !strings.Contains(out, "would offload a.txt") || !strings.Contains(out, "(dry-run) offloaded=1") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(volumeDir, "a.txt")); err != nil {
		t.Fatalf("a.txt should survive a dry-run: %v", err)
	}
}

func TestCLIOffloadReportsGateFailures(t *testing.T) {
	f, volumeDir := writeOffloadConfig(t, []string{"vault", "second"})
	writeTestFile(t, filepath.Join(volumeDir, "a.txt"), "alpha")
	runCLI(t, "--config", f.configPath, "index", "pics")
	seedOffloadEvidence(t, f.dbPath, "a.txt", []string{"vault"})

	out := runCLI(t, "--config", f.configPath, "offload", "pics", ".")
	if !strings.Contains(out, "skipped a.txt: not durable") ||
		!strings.Contains(out, "second: missing component for origin") ||
		!strings.Contains(out, "offloaded=0 not_durable=1") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(volumeDir, "a.txt")); err != nil {
		t.Fatalf("a.txt should be untouched: %v", err)
	}
}

func TestCLIOffloadRejectsBadOlderThan(t *testing.T) {
	f, volumeDir := writeOffloadConfig(t, []string{"vault"})
	writeTestFile(t, filepath.Join(volumeDir, "a.txt"), "alpha")
	runCLI(t, "--config", f.configPath, "index", "pics")

	_, err := runCLIExpectErr(t, "--config", f.configPath, "offload", "pics", "--older-than", "soon")
	if !strings.Contains(err.Error(), "--older-than") {
		t.Fatalf("err = %v, want --older-than parse error", err)
	}
}

func TestParseOlderThan(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"90d", 90 * 24 * time.Hour, false},
		{"720h", 720 * time.Hour, false},
		{"1h30m", 90 * time.Minute, false},
		{"0d", 0, true},
		{"-5h", 0, true},
		{"soon", 0, true},
		{"5", 0, true},
	}
	for _, c := range cases {
		got, err := parseOlderThan(c.in)
		if c.wantErr {
			if err == nil {
				t.Fatalf("parseOlderThan(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("parseOlderThan(%q) = (%v, %v), want %v", c.in, got, err, c.want)
		}
	}
}

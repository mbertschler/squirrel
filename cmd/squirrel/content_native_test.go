package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLIContentLayoutsOnLocalWithoutRclone: a content-addressed and a
// packed destination on a local disk sync, verify and restore with no
// rclone on PATH, and the sync fingerprints what it landed.
func TestCLIContentLayoutsOnLocalWithoutRclone(t *testing.T) {
	for _, layout := range []string{"content-addressed", "packed"} {
		t.Run(layout, func(t *testing.T) {
			withoutRclone(t)
			f := writeSyncFixture(t)
			cfg, err := os.ReadFile(f.configPath)
			if err != nil {
				t.Fatal(err)
			}
			cfg = []byte(strings.Replace(string(cfg), "type = \"local\"\n", "type = \"local\"\nlayout = \""+layout+"\"\n", 1))
			if err := os.WriteFile(f.configPath, cfg, 0o600); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")
			runCLI(t, "--config", f.configPath, "index", f.volumeName)

			out := runCLI(t, "--config", f.configPath, "sync", "pics")
			if !strings.Contains(out, "status=success") || !strings.Contains(out, "fingerprints=1") || strings.Contains(out, "rclone") {
				t.Fatalf("sync output = %q, want a fingerprinted success with no rclone preamble", out)
			}
			if out := runCLI(t, "--config", f.configPath, "verify", "scratch"); !strings.Contains(out, "mismatched=0 missing=0") {
				t.Fatalf("verify output = %q, want a clean pass", out)
			}
			to := t.TempDir()
			runCLI(t, "--config", f.configPath, "restore", "pics", "--from", "scratch", "--to", to)
			if got, err := os.ReadFile(filepath.Join(to, "a.txt")); err != nil || string(got) != "alpha" {
				t.Fatalf("restored a.txt = %q, %v", got, err)
			}
		})
	}
}

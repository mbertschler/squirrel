package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

// writeResetConfig writes a config with one local destination "arch" and a
// db path, returning the config path and the db path.
func writeResetConfig(t *testing.T) (configPath, dbPath string) {
	t.Helper()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "index.db")
	destDir := filepath.Join(dir, "arch")
	body := fmt.Sprintf("db = %q\n\n[destinations.arch]\ntype = \"local\"\nroot = %q\n", dbPath, destDir)
	configPath = writeCheckConfig(t, body)
	return configPath, dbPath
}

// seedResetVector opens the fixture DB directly and records one durability
// vector component for destination, so the reset has something to clear.
func seedResetVector(t *testing.T, dbPath, destination string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	v, err := s.GetOrCreateVolume(ctx, "/v")
	if err != nil {
		t.Fatalf("GetOrCreateVolume: %v", err)
	}
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, destination, self.ID, 5, store.VerifyMethodBlake3, false); err != nil {
		t.Fatalf("UpsertDestinationRunIDVerified: %v", err)
	}
}

// resetVectorCount reports how many vector components destination still has.
func resetVectorCount(t *testing.T, dbPath, destination string) int {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	v, err := s.GetOrCreateVolume(ctx, "/v")
	if err != nil {
		t.Fatalf("GetOrCreateVolume: %v", err)
	}
	vec, err := s.ListDestinationRunIDs(ctx, v.ID, destination)
	if err != nil {
		t.Fatalf("ListDestinationRunIDs: %v", err)
	}
	return len(vec)
}

// TestCLIDestinationResetUnknown: a destination not in config is refused
// before the store is touched.
func TestCLIDestinationResetUnknown(t *testing.T) {
	cfgPath, _ := writeResetConfig(t)
	out, err := runCLIExpectErr(t, "--config", cfgPath, "destination", "reset", "ghost", "--yes")
	if !strings.Contains(out, "unknown destination") && !strings.Contains(err.Error(), "unknown destination") {
		t.Fatalf("expected unknown-destination error, got out=%q err=%v", out, err)
	}
}

// TestCLIDestinationResetNothing: a configured destination with no recorded
// state reports "nothing to reset" affirmatively and mints no run.
func TestCLIDestinationResetNothing(t *testing.T) {
	cfgPath, _ := writeResetConfig(t)
	out := runCLI(t, "--config", cfgPath, "destination", "reset", "arch", "--yes")
	if !strings.Contains(out, "nothing to reset") {
		t.Fatalf("expected nothing-to-reset message:\n%s", out)
	}
}

// TestCLIDestinationResetDryRun: --dry-run previews the counts and clears
// nothing.
func TestCLIDestinationResetDryRun(t *testing.T) {
	cfgPath, dbPath := writeResetConfig(t)
	seedResetVector(t, dbPath, "arch")

	out := runCLI(t, "--config", cfgPath, "destination", "reset", "arch", "--dry-run")
	if !strings.Contains(out, "would clear") {
		t.Fatalf("expected dry-run preview:\n%s", out)
	}
	if n := resetVectorCount(t, dbPath, "arch"); n != 1 {
		t.Fatalf("dry-run cleared state: vector count = %d, want 1", n)
	}
}

// TestCLIDestinationResetNeedsConfirmation: without --yes the command
// previews and refuses, changing nothing (principle 2: weighty change).
func TestCLIDestinationResetNeedsConfirmation(t *testing.T) {
	cfgPath, dbPath := writeResetConfig(t)
	seedResetVector(t, dbPath, "arch")

	out, err := runCLIExpectErr(t, "--config", cfgPath, "destination", "reset", "arch")
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("expected confirmation-required error, got %v", err)
	}
	if !strings.Contains(out, "this will clear") {
		t.Fatalf("expected preview before refusal:\n%s", out)
	}
	if n := resetVectorCount(t, dbPath, "arch"); n != 1 {
		t.Fatalf("refused reset still cleared state: vector count = %d, want 1", n)
	}
}

// TestCLIDestinationResetConfirmed: --yes clears the state and names the
// audit run that recorded it.
func TestCLIDestinationResetConfirmed(t *testing.T) {
	cfgPath, dbPath := writeResetConfig(t)
	seedResetVector(t, dbPath, "arch")

	out := runCLI(t, "--config", cfgPath, "destination", "reset", "arch", "--yes")
	if !strings.Contains(out, "reset destination") || !strings.Contains(out, "run ") {
		t.Fatalf("expected loud reset confirmation with run id:\n%s", out)
	}
	if n := resetVectorCount(t, dbPath, "arch"); n != 0 {
		t.Fatalf("confirmed reset left state: vector count = %d, want 0", n)
	}
}

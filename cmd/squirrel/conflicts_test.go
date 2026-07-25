package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

// raiseTestContested opens the CLI's DB directly and freezes one path, so
// the conflicts commands have a standing latch to read and clear without
// driving a whole peer sync. The store handle is closed before returning
// so the next CLI invocation has no concurrent connection from this
// process.
func raiseTestContested(t *testing.T, dbPath, volumeName, relPath string) {
	t.Helper()
	s, err := store.OpenWithOptions(dbPath, store.OpenOptions{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	vol, err := s.GetVolumeByName(ctx, volumeName)
	if err != nil {
		t.Fatalf("GetVolumeByName %q: %v", volumeName, err)
	}
	run, err := s.BeginIndexRun(ctx, store.RunKindIndex, vol.ID, false)
	if err != nil {
		t.Fatalf("BeginIndexRun: %v", err)
	}
	if err := s.RaiseContested(ctx, store.ContestedPath{
		VolumeID:        vol.ID,
		Path:            relPath,
		LiveBlake3:      make([]byte, 32),
		PreservedBlake3: make([]byte, 32),
		PreservedAtPath: ".squirrel-conflicts/run-" + relPath,
		RaisedRunID:     run,
	}); err != nil {
		t.Fatalf("RaiseContested: %v", err)
	}
}

// TestCLIConflictsListAndResolve walks the question→change pair: an empty
// listing, a frozen path surfacing in the list, and an explicit resolve
// that clears it so the list empties again.
func TestCLIConflictsListAndResolve(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	f := writeConfigFor(t, map[string]string{"src": src})
	runCLI(t, "--config", f.configPath, "index", "src")

	// Nothing frozen yet.
	if out := runCLI(t, "--config", f.configPath, "conflicts"); !strings.Contains(out, "no contested paths") {
		t.Fatalf("empty listing = %q, want 'no contested paths'", out)
	}

	raiseTestContested(t, f.dbPath, "src", "a.txt")

	out := runCLI(t, "--config", f.configPath, "conflicts")
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "1 contested path") {
		t.Fatalf("listing = %q, want the frozen a.txt", out)
	}

	// Resolve clears it and reports what was kept.
	out = runCLI(t, "--config", f.configPath, "conflicts", "resolve", "src", "a.txt")
	if !strings.Contains(out, "contested freeze cleared") {
		t.Fatalf("resolve output = %q, want cleared confirmation", out)
	}

	if out := runCLI(t, "--config", f.configPath, "conflicts"); !strings.Contains(out, "no contested paths") {
		t.Fatalf("listing after resolve = %q, want empty", out)
	}
}

// TestCLIRunsContestedReminder covers the reminder beneath `squirrel runs`:
// it names volumes (never the internal id) and, under --volume, scopes to
// that volume rather than listing freezes on unrelated ones.
func TestCLIRunsContestedReminder(t *testing.T) {
	tmp := t.TempDir()
	dirA := filepath.Join(tmp, "a")
	dirB := filepath.Join(tmp, "b")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(d, "f.txt"), "content")
	}
	f := writeConfigFor(t, map[string]string{"a": dirA, "b": dirB})
	runCLI(t, "--config", f.configPath, "index", "a")
	runCLI(t, "--config", f.configPath, "index", "b")
	raiseTestContested(t, f.dbPath, "a", "f.txt")
	raiseTestContested(t, f.dbPath, "b", "f.txt")

	// Unfiltered: both volumes appear, rendered by name not numeric id.
	out := runCLI(t, "--config", f.configPath, "runs")
	if !strings.Contains(out, "2 contested path(s)") {
		t.Fatalf("unfiltered reminder = %q, want both freezes", out)
	}
	if !strings.Contains(out, "volume=a ") || !strings.Contains(out, "volume=b ") {
		t.Fatalf("reminder did not name both volumes: %q", out)
	}
	if strings.Contains(out, "volume=1") || strings.Contains(out, "volume=2") {
		t.Fatalf("reminder leaked a numeric volume id: %q", out)
	}

	// Filtered to volume a: only a's freeze is reminded.
	out = runCLI(t, "--config", f.configPath, "runs", "--volume", "a")
	if !strings.Contains(out, "1 contested path(s)") || !strings.Contains(out, "volume=a ") {
		t.Fatalf("filtered reminder = %q, want only volume a", out)
	}
	if strings.Contains(out, "volume=b ") {
		t.Fatalf("filtered reminder leaked volume b: %q", out)
	}
}

// TestCLIConflictsResolveUnknown: resolving a path that isn't contested is
// a clean error, not a silent no-op.
func TestCLIConflictsResolveUnknown(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	f := writeConfigFor(t, map[string]string{"src": src})
	runCLI(t, "--config", f.configPath, "index", "src")

	_, err := runCLIExpectErr(t, "--config", f.configPath, "conflicts", "resolve", "src", "nope.txt")
	if err == nil || !strings.Contains(err.Error(), "is not contested") {
		t.Fatalf("error = %v, want 'is not contested'", err)
	}

	// An unknown volume is a distinct, legible error.
	_, err = runCLIExpectErr(t, "--config", f.configPath, "conflicts", "resolve", "ghost", "a.txt")
	if err == nil || !strings.Contains(err.Error(), "no volume named") {
		t.Fatalf("error = %v, want 'no volume named'", err)
	}
}

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

// beginRunningRun creates a fresh kind='index' runs row stuck in
// status='running' against the named volume — the state a crashed
// indexer leaves behind. The store handle is closed before returning so
// the next CLI invocation has no concurrent SQLite connection from this
// process.
func beginRunningRun(t *testing.T, dbPath, volumeName string) int64 {
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
	runID, err := s.BeginIndexRun(ctx, store.RunKindIndex, vol.ID, false)
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	return runID
}

func getRun(t *testing.T, dbPath string, runID int64) store.Run {
	t.Helper()
	s, err := store.OpenWithOptions(dbPath, store.OpenOptions{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	r, err := s.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun %d: %v", runID, err)
	}
	return r
}

// TestCLIRunsFailFlipsRunning verifies the happy path: a stuck running
// row becomes failed with a synthesized error and a populated
// ended_at_ns.
func TestCLIRunsFailFlipsRunning(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	f := writeConfigFor(t, map[string]string{"src": src})
	// Index once so the volume row exists.
	runCLI(t, "--config", f.configPath, "index", "src")
	stuck := beginRunningRun(t, f.dbPath, "src")

	out := runCLI(t, "--config", f.configPath, "runs", "fail", fmt.Sprint(stuck))
	if !strings.Contains(out, fmt.Sprintf("marked run %d as failed", stuck)) {
		t.Fatalf("unexpected output: %q", out)
	}

	got := getRun(t, f.dbPath, stuck)
	if got.Status != store.RunStatusFailed {
		t.Fatalf("status = %q, want %q", got.Status, store.RunStatusFailed)
	}
	if !got.EndedAtNs.Valid {
		t.Fatalf("ended_at_ns is NULL after fail")
	}
	if !got.Error.Valid || !strings.Contains(got.Error.String, "marked failed manually at ") {
		t.Fatalf("error column = %+v, want synthesized message", got.Error)
	}
}

// TestCLIRunsFailRefusesTerminal: the manual fail path must not
// overwrite a row that already reached a terminal state. The previous
// outcome (success/failed/partial) is the audit record — silently
// rewriting it would lose history.
func TestCLIRunsFailRefusesTerminal(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	f := writeConfigFor(t, map[string]string{"src": src})
	runCLI(t, "--config", f.configPath, "index", "src")

	// Index runs by `squirrel index` end in success. Grab the row id.
	var doneID int64
	func() {
		s, err := store.OpenWithOptions(f.dbPath, store.OpenOptions{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer s.Close()
		runs, err := s.ListRuns(context.Background(), store.ListRunsOpts{Limit: 1, Descending: true})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(runs) != 1 || runs[0].Status != store.RunStatusSuccess {
			t.Fatalf("unexpected runs %+v", runs)
		}
		doneID = runs[0].ID
	}()

	_, err := runCLIExpectErr(t, "--config", f.configPath, "runs", "fail", fmt.Sprint(doneID))
	want := fmt.Sprintf("run %d is already success, refusing to overwrite", doneID)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want substring %q", err, want)
	}

	// Status must not have flipped.
	if got := getRun(t, f.dbPath, doneID); got.Status != store.RunStatusSuccess {
		t.Fatalf("status changed to %q after refused fail", got.Status)
	}
}

// TestCLIRunsFailUnknownID: failing a nonexistent id is a clean error,
// not a silent no-op or a wrapped sql.ErrNoRows.
func TestCLIRunsFailUnknownID(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	f := writeConfigFor(t, map[string]string{"src": src})
	runCLI(t, "--config", f.configPath, "index", "src")

	_, err := runCLIExpectErr(t, "--config", f.configPath, "runs", "fail", "99999")
	if !strings.Contains(err.Error(), "no run with id 99999") {
		t.Fatalf("error = %v, want 'no run with id 99999'", err)
	}
}

// TestCLIRunsFailRejectsBadID: a non-numeric id is a parse error, not
// "no run with id ..." — surface what the operator typed so the
// recovery path doesn't mask a typo as a missing-row condition.
func TestCLIRunsFailRejectsBadID(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	f := writeConfigFor(t, map[string]string{"src": src})
	runCLI(t, "--config", f.configPath, "index", "src")

	_, err := runCLIExpectErr(t, "--config", f.configPath, "runs", "fail", "abc")
	if !strings.Contains(err.Error(), `invalid run id "abc"`) {
		t.Fatalf("error = %v, want invalid-run-id error", err)
	}
}

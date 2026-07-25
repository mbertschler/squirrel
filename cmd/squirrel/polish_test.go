package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/store"
)

// TestRunNeedsAttention pins the --failed predicate: only the non-clean
// terminal states qualify (F19).
func TestRunNeedsAttention(t *testing.T) {
	cases := map[string]bool{
		store.RunStatusFailed:  true,
		store.RunStatusRefused: true,
		store.RunStatusAborted: true,
		store.RunStatusPartial: true,
		store.RunStatusSuccess: false,
		store.RunStatusRunning: false,
	}
	for status, want := range cases {
		if got := runNeedsAttention(status); got != want {
			t.Errorf("runNeedsAttention(%q) = %v, want %v", status, got, want)
		}
	}
}

// TestRunIsInteresting pins the --changes predicate: clean successes that
// touched nothing fold; anything that moved files, resolved a conflict, or
// needs attention shows (F19).
func TestRunIsInteresting(t *testing.T) {
	conflicts := map[int64]int{7: 2}
	tests := []struct {
		name string
		r    store.Run
		want bool
	}{
		{"clean no-op", store.Run{ID: 1, Status: store.RunStatusSuccess, FileCount: 0}, false},
		{"success moved files", store.Run{ID: 2, Status: store.RunStatusSuccess, FileCount: 5}, true},
		{"failed", store.Run{ID: 3, Status: store.RunStatusFailed}, true},
		{"refused", store.Run{ID: 4, Status: store.RunStatusRefused}, true},
		{"success with conflict", store.Run{ID: 7, Status: store.RunStatusSuccess, FileCount: 0}, true},
		// #182: a bucket push counts already-correct files in file_count,
		// so only changed_count can tell the in-sync no-op from the push
		// that actually moved content.
		{"in-sync bucket push", store.Run{ID: 8, Status: store.RunStatusSuccess, FileCount: 42,
			ChangedCount: sql.NullInt64{Int64: 0, Valid: true}}, false},
		{"bucket push that transferred", store.Run{ID: 9, Status: store.RunStatusSuccess, FileCount: 42,
			ChangedCount: sql.NullInt64{Int64: 3, Valid: true}}, true},
		// Pre-v28 history has no changed count and keeps the conservative
		// file_count rendering.
		{"pre-v28 bucket push", store.Run{ID: 10, Status: store.RunStatusSuccess, FileCount: 42}, true},
	}
	for _, tc := range tests {
		if got := runIsInteresting(tc.r, conflicts); got != tc.want {
			t.Errorf("%s: runIsInteresting = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestFilterRuns exercises both filters end to end, including that the
// descending order is preserved.
func TestFilterRuns(t *testing.T) {
	changed := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	runs := []store.Run{
		// A steady-state bucket push and an all-unchanged re-index: both
		// consider the whole volume, both changed nothing (#182).
		{ID: 7, Status: store.RunStatusSuccess, FileCount: 42, ChangedCount: changed(0)},
		{ID: 6, Status: store.RunStatusSuccess, FileCount: 42, ChangedCount: changed(2)}, // moved content
		{ID: 5, Status: store.RunStatusSuccess, FileCount: 0},                            // no-op
		{ID: 4, Status: store.RunStatusSuccess, FileCount: 3},                            // change
		{ID: 3, Status: store.RunStatusFailed},                                           // failed
		{ID: 2, Status: store.RunStatusRefused},                                          // refused
		{ID: 1, Status: store.RunStatusSuccess, FileCount: 0},                            // no-op
	}
	conflicts := map[int64]int{}

	if ids := runIDs(filterRuns(runs, conflicts, true, false)); !sameIDs(ids, []int64{3, 2}) {
		t.Fatalf("--failed ids = %v, want [3 2]", ids)
	}
	if ids := runIDs(filterRuns(runs, conflicts, false, true)); !sameIDs(ids, []int64{6, 4, 3, 2}) {
		t.Fatalf("--changes ids = %v, want [6 4 3 2]", ids)
	}
}

// TestPeerDirectionArrow / TestPeerLabelDirection cover F18: the initiator
// records runs.shallow, the receiver leaves it NULL, and the arrow follows.
func TestPeerDirectionArrow(t *testing.T) {
	if got := peerDirectionArrow(store.Run{Shallow: sql.NullBool{Valid: true}}); got != "→" {
		t.Errorf("initiator arrow = %q, want →", got)
	}
	if got := peerDirectionArrow(store.Run{Shallow: sql.NullBool{}}); got != "←" {
		t.Errorf("receiver arrow = %q, want ←", got)
	}
}

func TestPeerLabelDirection(t *testing.T) {
	nodes := map[int64]string{9: "htpc"}
	out := peerLabel(store.Run{PeerNodeID: sql.NullInt64{Int64: 9, Valid: true}, Shallow: sql.NullBool{Valid: true}}, nodes)
	if !strings.HasPrefix(out, "→ htpc") {
		t.Errorf("outbound peer label = %q, want prefix %q", out, "→ htpc")
	}
	in := peerLabel(store.Run{PeerNodeID: sql.NullInt64{Int64: 9, Valid: true}}, nodes)
	if !strings.HasPrefix(in, "← htpc") {
		t.Errorf("inbound peer label = %q, want prefix %q", in, "← htpc")
	}
}

// TestCatchUpNote pins the F24 heuristic and its rendering.
func TestCatchUpNote(t *testing.T) {
	if _, ok := catchUpNote(catchUpMinFiles, catchUpMinQuiet); !ok {
		t.Fatal("expected catch-up at the thresholds")
	}
	note, ok := catchUpNote(404, 21*24*time.Hour)
	if !ok || !strings.Contains(note, "404 new files") || !strings.Contains(note, "21 days") {
		t.Fatalf("catch-up note = %q ok=%v", note, ok)
	}
	if _, ok := catchUpNote(catchUpMinFiles-1, catchUpMinQuiet); ok {
		t.Error("below file threshold should not trigger")
	}
	if _, ok := catchUpNote(catchUpMinFiles, catchUpMinQuiet-time.Hour); ok {
		t.Error("below quiet threshold should not trigger")
	}
}

// TestCLIIndexNamesVolume covers F11a: the index summary names the volume.
func TestCLIIndexNamesVolume(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "a.txt"), "hello")
	f := writeConfigFor(t, map[string]string{"docs": src})
	out := runCLI(t, "--config", f.configPath, "index", "docs")
	if !strings.Contains(out, "docs: added=1") {
		t.Fatalf("index summary should name the volume: %q", out)
	}
}

// TestCLIIndexEmptyVolumeWarns covers F8: a first index of an empty tree
// warns, and a subsequent populated re-index does not.
func TestCLIIndexEmptyVolumeWarns(t *testing.T) {
	src := t.TempDir() // empty
	f := writeConfigFor(t, map[string]string{"docs": src})
	out := runCLI(t, "--config", f.configPath, "index", "docs")
	if !strings.Contains(out, "no files found") {
		t.Fatalf("empty first index should warn: %q", out)
	}
	writeTestFile(t, filepath.Join(src, "a.txt"), "hi")
	out2 := runCLI(t, "--config", f.configPath, "index", "docs")
	if strings.Contains(out2, "no files found") {
		t.Fatalf("populated re-index should not warn: %q", out2)
	}
}

func runIDs(runs []store.Run) []int64 {
	out := make([]int64, len(runs))
	for i, r := range runs {
		out[i] = r.ID
	}
	return out
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

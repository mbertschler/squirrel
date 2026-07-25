package store

import (
	"bytes"
	"context"
	"testing"
)

// TestContestedPathLifecycle exercises raise → idempotent re-raise →
// query → clear and the runs_audit trail each transition leaves.
func TestContestedPathLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	volID := makeVolume(t, s, "/v")
	first := makeRun(t, s, volID)
	live := digest(0xAA)
	loser := digest(0xBB)

	c := ContestedPath{
		VolumeID:        volID,
		Path:            "doc.md",
		LiveBlake3:      live,
		PreservedBlake3: loser,
		PreservedAtPath: ".squirrel-conflicts/run-1/doc.md",
		RaisedRunID:     first,
	}
	if err := s.RaiseContested(ctx, c); err != nil {
		t.Fatalf("RaiseContested: %v", err)
	}

	got, err := s.GetContestedPath(ctx, volID, "doc.md")
	if err != nil {
		t.Fatalf("GetContestedPath: %v", err)
	}
	if !bytes.Equal(got.LiveBlake3, live) || !bytes.Equal(got.PreservedBlake3, loser) {
		t.Fatalf("digests = live %x preserved %x, want %x / %x", got.LiveBlake3, got.PreservedBlake3, live, loser)
	}
	if got.PreservedAtPath != c.PreservedAtPath || got.RaisedRunID != first || got.RaisedAtNs == 0 {
		t.Fatalf("latch = %+v, want preserved-path/run/timestamp set", got)
	}
	firstAt := got.RaisedAtNs

	// IsPathContested returns the frozen winner so the classifier can tell
	// the winner re-asserting from a divergent re-assertion.
	winner, contested, err := s.IsPathContested(ctx, volID, "doc.md")
	if err != nil || !contested {
		t.Fatalf("IsPathContested = (%x, %v, %v), want frozen", winner, contested, err)
	}
	if !bytes.Equal(winner, live) {
		t.Fatalf("IsPathContested winner = %x, want %x", winner, live)
	}

	// A second conflict on the same path keeps the original latch: same
	// run, same timestamp — "contested since" never resets, and no second
	// copy is minted (the F27 fix is preserved-once).
	second := makeRun(t, s, volID)
	if err := s.RaiseContested(ctx, ContestedPath{
		VolumeID: volID, Path: "doc.md", LiveBlake3: digest(0xCC),
		PreservedBlake3: digest(0xDD), RaisedRunID: second,
	}); err != nil {
		t.Fatalf("re-raise: %v", err)
	}
	got, err = s.GetContestedPath(ctx, volID, "doc.md")
	if err != nil {
		t.Fatalf("GetContestedPath after re-raise: %v", err)
	}
	if got.RaisedRunID != first || got.RaisedAtNs != firstAt || !bytes.Equal(got.LiveBlake3, live) {
		t.Fatalf("re-raise mutated the latch: %+v", got)
	}

	// Exactly one contested-raise audit row against the first run; none
	// against the second (the re-raise was a no-op).
	if n := countTransition(t, s, first, TransitionContestedRaise); n != 1 {
		t.Fatalf("first run contested-raise count = %d, want 1", n)
	}
	if n := countTransition(t, s, second, TransitionContestedRaise); n != 0 {
		t.Fatalf("second run contested-raise count = %d, want 0 (idempotent)", n)
	}

	// The run-row conflict column derivation counts one freeze against the
	// raising run.
	counts, err := s.CountContestedRaisedByRun(ctx, []int64{first, second})
	if err != nil {
		t.Fatalf("CountContestedRaisedByRun: %v", err)
	}
	if counts[first] != 1 || counts[second] != 0 {
		t.Fatalf("raised-by-run counts = %v, want {first:1}", counts)
	}

	// Clear via resolve: recorded against the raising run, tagged operator.
	cleared, err := s.ClearContested(ctx, volID, "doc.md", "alice")
	if err != nil {
		t.Fatalf("ClearContested: %v", err)
	}
	if !cleared {
		t.Fatal("ClearContested reported no active freeze")
	}
	if _, err := s.GetContestedPath(ctx, volID, "doc.md"); !IsNotFound(err) {
		t.Fatalf("latch still present after clear: %v", err)
	}
	if _, contested, _ := s.IsPathContested(ctx, volID, "doc.md"); contested {
		t.Fatal("IsPathContested still reports frozen after clear")
	}
	if n := countTransition(t, s, first, TransitionContestedClear); n != 1 {
		t.Fatalf("contested-clear count against raising run = %d, want 1", n)
	}

	// Clearing an already-cleared path is a benign no-op, not an error.
	cleared, err = s.ClearContested(ctx, volID, "doc.md", "alice")
	if err != nil || cleared {
		t.Fatalf("second ClearContested = (%v, %v), want (false, nil)", cleared, err)
	}
}

// TestContestedPathUnknownDigest covers a freeze recorded without a known
// winner digest (an initiator mirror that lost the hex): the NULL columns
// round-trip as nil, and IsPathContested still reports frozen.
func TestContestedPathUnknownDigest(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	volID := makeVolume(t, s, "/v")
	run := makeRun(t, s, volID)

	if err := s.RaiseContested(ctx, ContestedPath{
		VolumeID: volID, Path: "a/b.txt", RaisedRunID: run,
	}); err != nil {
		t.Fatalf("RaiseContested: %v", err)
	}
	winner, contested, err := s.IsPathContested(ctx, volID, "a/b.txt")
	if err != nil || !contested {
		t.Fatalf("IsPathContested = (%x, %v, %v), want frozen", winner, contested, err)
	}
	if winner != nil {
		t.Fatalf("winner = %x, want nil (unknown digest)", winner)
	}
	list, err := s.ListContestedPaths(ctx)
	if err != nil {
		t.Fatalf("ListContestedPaths: %v", err)
	}
	if len(list) != 1 || list[0].Path != "a/b.txt" || list[0].PreservedAtPath != "" {
		t.Fatalf("list = %+v, want one row with empty preserved path", list)
	}
}

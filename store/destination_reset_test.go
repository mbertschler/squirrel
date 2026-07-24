package store

import (
	"context"
	"testing"
)

// seedDestinationState populates every category of derived state for
// destination on volume vID, plus one durability advance for a second
// destination, and returns the self node id. It leaves a runs row (the
// index run) and a destination_run_ids_history row behind so the
// audit-preservation assertions have something to check against.
func seedDestinationState(t *testing.T, s *Store, vID, runID int64, destination string) int64 {
	t.Helper()
	ctx := context.Background()
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}

	objContent := packContentFixture(t, s, vID, runID, "obj.txt", 0xa1)
	if err := s.InsertRemoteObject(ctx, RemoteObject{
		ContentID: objContent, Destination: destination, UploadedRunID: runID,
	}); err != nil {
		t.Fatalf("InsertRemoteObject: %v", err)
	}

	packContent := packContentFixture(t, s, vID, runID, "packed.txt", 0xa2)
	if err := s.InsertPacks(ctx, []PackWrite{{
		Pack:    Pack{PackKey: packKey(0x11), SizeBytes: 10, MemberCount: 1, CreatedRunID: runID},
		Members: []PackMember{{ContentID: packContent, ByteOffset: 0, ByteLength: 1}},
	}}); err != nil {
		t.Fatalf("InsertPacks: %v", err)
	}
	pack, err := s.GetPackByKey(ctx, packKey(0x11))
	if err != nil {
		t.Fatalf("GetPackByKey: %v", err)
	}
	if err := s.InsertRemotePack(ctx, RemotePack{
		PackID: pack.ID, Destination: destination, UploadedRunID: runID,
	}); err != nil {
		t.Fatalf("InsertRemotePack: %v", err)
	}

	if err := s.UpsertDestinationRunIDVerified(ctx, vID, destination, self.ID, 5, VerifyMethodBlake3, false); err != nil {
		t.Fatalf("UpsertDestinationRunIDVerified: %v", err)
	}
	if err := s.UpsertDestinationPushFreshness(ctx, vID, destination, self.ID, 5); err != nil {
		t.Fatalf("UpsertDestinationPushFreshness: %v", err)
	}
	// A second destination whose state must survive a reset of the first.
	if err := s.UpsertDestinationRunIDVerified(ctx, vID, "other", self.ID, 7, VerifyMethodBlake3, false); err != nil {
		t.Fatalf("UpsertDestinationRunIDVerified(other): %v", err)
	}
	return self.ID
}

// TestResetDestinationClearsDerivedStateAuditPreservingly is the F20 core:
// reset forgets a destination's upload ledgers, vector, and freshness while
// leaving the runs table, the append-only advance log, other destinations,
// and the content rows intact — and records itself as an audit run.
func TestResetDestinationClearsDerivedStateAuditPreservingly(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	runID := makeRun(t, s, vID)
	self := seedDestinationState(t, s, vID, runID, "arch")

	counts, err := s.CountDestinationRecordedState(ctx, "arch")
	if err != nil {
		t.Fatalf("CountDestinationRecordedState: %v", err)
	}
	if counts.RemoteObjects != 1 || counts.RemotePacks != 1 || counts.VectorComponents != 1 || counts.FreshnessRows != 1 {
		t.Fatalf("pre-reset counts = %+v, want one of each", counts)
	}

	_, cleared, err := s.ResetDestination(ctx, "arch")
	if err != nil {
		t.Fatalf("ResetDestination: %v", err)
	}
	if cleared != counts {
		t.Fatalf("cleared = %+v, want %+v", cleared, counts)
	}

	// Derived state is gone.
	after, err := s.CountDestinationRecordedState(ctx, "arch")
	if err != nil {
		t.Fatalf("CountDestinationRecordedState after: %v", err)
	}
	if !after.Empty() {
		t.Fatalf("post-reset counts = %+v, want empty", after)
	}
	vec, err := s.ListDestinationRunIDs(ctx, vID, "arch")
	if err != nil {
		t.Fatalf("ListDestinationRunIDs: %v", err)
	}
	if len(vec) != 0 {
		t.Fatalf("vector for arch = %+v, want empty", vec)
	}

	// The append-only advance log survives — the reset is audit-preserving.
	hist, err := s.ListDestinationRunIDHistory(ctx, vID, "arch")
	if err != nil {
		t.Fatalf("ListDestinationRunIDHistory: %v", err)
	}
	if len(hist) == 0 {
		t.Fatalf("advance history for arch was cleared; reset must preserve it")
	}

	// The other destination is untouched.
	otherVec, err := s.ListDestinationRunIDs(ctx, vID, "other")
	if err != nil {
		t.Fatalf("ListDestinationRunIDs(other): %v", err)
	}
	if len(otherVec) != 1 || otherVec[0].OriginNodeID != self {
		t.Fatalf("other vector = %+v, want one component for self", otherVec)
	}

	// The content rows are never touched.
	if _, err := s.GetByPath(ctx, vID, "obj.txt"); err != nil {
		t.Fatalf("content row for obj.txt lost: %v", err)
	}
}

// TestResetDestinationRecordsAuditRun: the reset is a kind='audit' run that
// finishes success and carries a 'reset-destination' runs_audit note with
// the cleared counts, alongside the usual 'finish' transition.
func TestResetDestinationRecordsAuditRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	runID := makeRun(t, s, vID)
	seedDestinationState(t, s, vID, runID, "arch")

	resetRun, cleared, err := s.ResetDestination(ctx, "arch")
	if err != nil {
		t.Fatalf("ResetDestination: %v", err)
	}
	run, err := s.GetRun(ctx, resetRun)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Kind != RunKindAudit || run.Status != RunStatusSuccess || run.VolumeID.Valid || run.Destination.Valid {
		t.Fatalf("reset run = %+v, want a success audit run with NULL volume+destination", run)
	}
	if run.FileCount != cleared.Total() {
		t.Fatalf("run file_count = %d, want cleared total %d", run.FileCount, cleared.Total())
	}

	audits, err := s.ListRunAudit(ctx, resetRun)
	if err != nil {
		t.Fatalf("ListRunAudit: %v", err)
	}
	var sawReset, sawFinish bool
	for _, a := range audits {
		switch a.Transition {
		case TransitionResetDestination:
			sawReset = true
			if !a.Note.Valid || a.Note.String == "" {
				t.Fatalf("reset-destination note is empty: %+v", a)
			}
		case TransitionFinish:
			sawFinish = true
		}
	}
	if !sawReset || !sawFinish {
		t.Fatalf("audit transitions = %+v, want both reset-destination and finish", audits)
	}

	// The original index run is preserved — reset never prunes runs.
	if _, err := s.GetRun(ctx, runID); err != nil {
		t.Fatalf("original index run %d lost: %v", runID, err)
	}
}

// TestResetDestinationEmpty: a destination with no recorded state reports
// empty counts, so the CLI can report "nothing to reset" rather than mint a
// run.
func TestResetDestinationEmpty(t *testing.T) {
	s := openTestStore(t)
	counts, err := s.CountDestinationRecordedState(context.Background(), "never-used")
	if err != nil {
		t.Fatalf("CountDestinationRecordedState: %v", err)
	}
	if !counts.Empty() {
		t.Fatalf("counts = %+v, want empty for an unused destination", counts)
	}
}

// TestResetDestinationRejectsEmptyName: both entry points refuse a blank
// destination rather than clearing everything with an empty-string match.
func TestResetDestinationRejectsEmptyName(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.CountDestinationRecordedState(ctx, ""); err == nil {
		t.Fatalf("CountDestinationRecordedState(\"\") succeeded, want error")
	}
	if _, _, err := s.ResetDestination(ctx, ""); err == nil {
		t.Fatalf("ResetDestination(\"\") succeeded, want error")
	}
}

package store

import (
	"context"
	"testing"
)

// remotePathFixture indexes one path, "2024/cat.jpg", and returns its
// PathDelta (which carries the files row key) and the observing run.
func remotePathFixture(t *testing.T, s *Store) (PathDelta, int64, int64) {
	t.Helper()
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	runID := makeRun(t, s, vID)
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "2024/cat.jpg", Blake3: digest(0xaa), SizeBytes: 3, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	delta, err := s.ListPathDeltaSince(ctx, vID, 0)
	if err != nil || len(delta) != 1 {
		t.Fatalf("ListPathDeltaSince = %v, %v; want one row", delta, err)
	}
	return delta[0], vID, runID
}

func commitWrite(d PathDelta, runID int64) RemotePathWrite {
	return RemotePathWrite{Destination: "usb", FolderID: d.FolderID, Name: "cat.jpg", ContentID: d.ContentID, RunID: runID, MtimeNs: 42}
}

// TestRemotePathLifecycle walks one version through every recorded move —
// committing, live, displacing, displaced — and checks that each read sees
// it where the move left it, with its path and size resolved.
func TestRemotePathLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	d, vID, runID := remotePathFixture(t, s)

	id, err := s.BeginRemotePathCommit(ctx, commitWrite(d, runID))
	if err != nil {
		t.Fatalf("BeginRemotePathCommit: %v", err)
	}
	unsettled, err := s.ListUnsettledRemotePaths(ctx, "usb", vID)
	if err != nil || len(unsettled) != 1 || unsettled[0].State != RemotePathCommitting {
		t.Fatalf("unsettled after commit intent = %+v, %v", unsettled, err)
	}
	if got := unsettled[0]; got.Path != "2024/cat.jpg" || got.SizeBytes != 3 || got.MtimeNs != 42 {
		t.Fatalf("row = %+v, want path 2024/cat.jpg size 3 mtime 42", got)
	}

	if err := s.ConfirmRemotePathsLive(ctx, id); err != nil {
		t.Fatalf("ConfirmRemotePathsLive: %v", err)
	}
	live, err := s.ListLiveRemotePaths(ctx, "usb", vID)
	if err != nil || len(live) != 1 || live[0].ID != id {
		t.Fatalf("live = %+v, %v", live, err)
	}

	if err := s.BeginRemotePathsDisplace(ctx, runID, id); err != nil {
		t.Fatalf("BeginRemotePathsDisplace: %v", err)
	}
	if err := s.ConfirmRemotePathsDisplaced(ctx, id); err != nil {
		t.Fatalf("ConfirmRemotePathsDisplaced: %v", err)
	}
	rows, err := s.ListRemotePaths(ctx, "usb", vID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListRemotePaths = %+v, %v", rows, err)
	}
	if rows[0].State != RemotePathDisplaced || rows[0].DisplacedRunID.Int64 != runID {
		t.Fatalf("row = %+v, want displaced by run %d", rows[0], runID)
	}
	if live, _ := s.ListLiveRemotePaths(ctx, "usb", vID); len(live) != 0 {
		t.Fatalf("a displaced row still reads as live: %+v", live)
	}
}

// TestRemotePathTransitionsNeedTheirSourceState: a move applies only to a
// row in the state it starts from, and a batch with one wrong row changes
// nothing, so a record never claims a move it did not witness.
func TestRemotePathTransitionsNeedTheirSourceState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	d, vID, runID := remotePathFixture(t, s)
	id, err := s.BeginRemotePathCommit(ctx, commitWrite(d, runID))
	if err != nil {
		t.Fatalf("BeginRemotePathCommit: %v", err)
	}

	if err := s.ConfirmRemotePathsDisplaced(ctx, id); err == nil {
		t.Fatal("a committing row was confirmed displaced")
	}
	if err := s.BeginRemotePathsDisplace(ctx, runID, id); err == nil {
		t.Fatal("a committing row began a displacement")
	}
	if err := s.RevertRemotePathsLive(ctx, id); err == nil {
		t.Fatal("a committing row was reverted to live")
	}
	if err := s.ConfirmRemotePathsLive(ctx, id, id+1); err == nil {
		t.Fatal("a batch naming a missing row succeeded")
	}
	rows, _ := s.ListRemotePaths(ctx, "usb", vID)
	if len(rows) != 1 || rows[0].State != RemotePathCommitting {
		t.Fatalf("a failed batch changed the row: %+v", rows)
	}

	if err := s.MarkRemotePathsLost(ctx, id); err != nil {
		t.Fatalf("MarkRemotePathsLost: %v", err)
	}
	if err := s.MarkRemotePathsLost(ctx, id); err == nil {
		t.Fatal("a lost row was marked lost twice")
	}
}

// TestRemotePathOneLiveVersionPerPath: while a version is live, no second
// commit may start at its path; once the live one is displacing, it may.
// A displacement that never happened reverts to live.
func TestRemotePathOneLiveVersionPerPath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	d, vID, runID := remotePathFixture(t, s)
	first, err := s.BeginRemotePathCommit(ctx, commitWrite(d, runID))
	if err != nil {
		t.Fatalf("BeginRemotePathCommit: %v", err)
	}
	if err := s.ConfirmRemotePathsLive(ctx, first); err != nil {
		t.Fatalf("ConfirmRemotePathsLive: %v", err)
	}
	if _, err := s.BeginRemotePathCommit(ctx, commitWrite(d, runID)); err == nil {
		t.Fatal("a second commit started at a path with a live version")
	}
	// Another destination is independent.
	other := commitWrite(d, runID)
	other.Destination = "nas"
	if _, err := s.BeginRemotePathCommit(ctx, other); err != nil {
		t.Fatalf("commit on another destination: %v", err)
	}

	if err := s.BeginRemotePathsDisplace(ctx, runID, first); err != nil {
		t.Fatalf("BeginRemotePathsDisplace: %v", err)
	}
	if err := s.RevertRemotePathsLive(ctx, first); err != nil {
		t.Fatalf("RevertRemotePathsLive: %v", err)
	}
	rows, _ := s.ListLiveRemotePaths(ctx, "usb", vID)
	if len(rows) != 1 || rows[0].DisplacedRunID.Valid {
		t.Fatalf("reverted row = %+v, want live with no displacing run", rows)
	}
}

// TestContentPresentOnDestinationCountsContentLayoutsOnly: the content
// layouts' upload-once check counts only bytes where they read them. A
// live mirror copy sits at its path, so a destination whose mirror records
// outlived a switch to a content layout still uploads the object.
func TestContentPresentOnDestinationCountsContentLayoutsOnly(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	d, _, runID := remotePathFixture(t, s)
	id, err := s.BeginRemotePathCommit(ctx, commitWrite(d, runID))
	if err != nil {
		t.Fatalf("BeginRemotePathCommit: %v", err)
	}
	if err := s.ConfirmRemotePathsLive(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ContentPresentOnDestination(ctx, d.ContentID, "usb"); err != nil || ok {
		t.Fatalf("ContentPresentOnDestination with only a mirror copy = %t, %v; want false", ok, err)
	}
	if err := s.InsertRemoteObject(ctx, RemoteObject{ContentID: d.ContentID, Destination: "usb", UploadedRunID: runID}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ContentPresentOnDestination(ctx, d.ContentID, "usb"); err != nil || !ok {
		t.Fatalf("ContentPresentOnDestination with an object = %t, %v; want true", ok, err)
	}
}

// TestResetDestinationClearsRemotePaths: a reset clears the mirror ledger
// with the others, counts it, and leaves the destination without upload
// records.
func TestResetDestinationClearsRemotePaths(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	d, vID, runID := remotePathFixture(t, s)
	if _, err := s.BeginRemotePathCommit(ctx, commitWrite(d, runID)); err != nil {
		t.Fatalf("BeginRemotePathCommit: %v", err)
	}
	if has, err := s.DestinationHasUploadRecords(ctx, "usb"); err != nil || !has {
		t.Fatalf("DestinationHasUploadRecords = %t, %v; want true", has, err)
	}

	_, counts, err := s.ResetDestination(ctx, "usb")
	if err != nil {
		t.Fatalf("ResetDestination: %v", err)
	}
	if counts.RemotePaths != 1 {
		t.Fatalf("counts = %+v, want one remote path", counts)
	}
	if rows, _ := s.ListRemotePaths(ctx, "usb", vID); len(rows) != 0 {
		t.Fatalf("rows after reset = %+v", rows)
	}
	if has, err := s.DestinationHasUploadRecords(ctx, "usb"); err != nil || has {
		t.Fatalf("DestinationHasUploadRecords after reset = %t, %v; want false", has, err)
	}
}

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

// liveRemotePath commits d at runID on "usb" with checksum (empty for
// none) and confirms it live.
func liveRemotePath(t *testing.T, s *Store, d PathDelta, runID int64, checksum string) int64 {
	t.Helper()
	ctx := context.Background()
	w := commitWrite(d, runID)
	w.Checksum, w.VerifiedAtNs = checksum, 7
	id, err := s.BeginRemotePathCommit(ctx, w)
	if err != nil {
		t.Fatalf("BeginRemotePathCommit: %v", err)
	}
	if err := s.ConfirmRemotePathsLive(ctx, id); err != nil {
		t.Fatalf("ConfirmRemotePathsLive: %v", err)
	}
	return id
}

// TestMirrorFingerprintsGateOnlyWhileStored: a live or displaced mirror copy
// whose read-back recorded its BLAKE3 counts as fingerprint-verified; one
// without a checksum, or a lost one, does not.
func TestMirrorFingerprintsGateOnlyWhileStored(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	d, vID, runID := remotePathFixture(t, s)
	verified := func() bool {
		t.Helper()
		ok, err := s.ContentFingerprintVerified(ctx, d.ContentID, "usb")
		if err != nil {
			t.Fatal(err)
		}
		pending, err := s.CountVolumeContentsPendingFingerprint(ctx, vID, "usb")
		if err != nil {
			t.Fatal(err)
		}
		if ok != (pending == 0) {
			t.Fatalf("ContentFingerprintVerified = %t but %d content(s) pending", ok, pending)
		}
		return ok
	}

	unhashed := liveRemotePath(t, s, d, runID, "")
	if verified() {
		t.Fatal("a mirror copy without a checksum counts as verified")
	}
	if err := s.MarkRemotePathsLost(ctx, unhashed); err != nil {
		t.Fatal(err)
	}
	id := liveRemotePath(t, s, d, runID, "abc")
	if !verified() {
		t.Fatal("a live mirror copy with a read-back checksum does not count as verified")
	}
	if err := s.BeginRemotePathsDisplace(ctx, runID, id); err != nil {
		t.Fatal(err)
	}
	if verified() {
		t.Fatal("a copy on its way into history counts as verified")
	}
	if err := s.ConfirmRemotePathsDisplaced(ctx, id); err != nil {
		t.Fatal(err)
	}
	if !verified() {
		t.Fatal("a displaced copy with a read-back checksum does not count as verified")
	}
	if err := s.MarkRemotePathsLost(ctx, id); err != nil {
		t.Fatal(err)
	}
	if verified() {
		t.Fatal("a lost copy counts as verified")
	}
}

// TestListStoredRemotePathsRotatesByVerification: the verify listing holds
// every live and displaced row, never-verified first, then oldest verified,
// and a re-read moves a row to the back.
func TestListStoredRemotePathsRotatesByVerification(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	d, _, runID := remotePathFixture(t, s)
	first := liveRemotePath(t, s, d, runID, "abc")
	if err := s.BeginRemotePathsDisplace(ctx, runID, first); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmRemotePathsDisplaced(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := liveRemotePath(t, s, d, runID, "")
	ids := func() []int64 {
		t.Helper()
		rows, err := s.ListStoredRemotePaths(ctx, "usb")
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, r := range rows {
			if r.Volume != "v" || r.Path != "2024/cat.jpg" {
				t.Fatalf("row = %+v, want volume v at 2024/cat.jpg", r)
			}
			out = append(out, r.ID)
		}
		return out
	}
	if got := ids(); len(got) != 2 || got[0] != second || got[1] != first {
		t.Fatalf("order = %v, want the never-verified %d before %d", got, second, first)
	}
	if err := s.RecordRemotePathVerified(ctx, second, "abc", 9); err != nil {
		t.Fatalf("RecordRemotePathVerified: %v", err)
	}
	if got := ids(); got[0] != first || got[1] != second {
		t.Fatalf("order after re-reading %d = %v", second, got)
	}
	if err := s.RecordRemotePathVerified(ctx, second, "def", 10); err == nil {
		t.Fatal("a read replaced the checksum it should re-confirm")
	}
}

// TestListRemotePathRepairs: a present path whose current content the
// destination lost is a repair until a new live row holds it again.
func TestListRemotePathRepairs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	d, vID, runID := remotePathFixture(t, s)
	repairs := func() []PathDelta {
		t.Helper()
		out, err := s.ListRemotePathRepairs(ctx, "usb", vID)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	id := liveRemotePath(t, s, d, runID, "")
	if got := repairs(); len(got) != 0 {
		t.Fatalf("repairs with a live copy = %+v", got)
	}
	if err := s.MarkRemotePathsLost(ctx, id); err != nil {
		t.Fatal(err)
	}
	got := repairs()
	if len(got) != 1 || got[0].Path != d.Path || got[0].ContentID != d.ContentID || got[0].Status != StatusPresent {
		t.Fatalf("repairs after the copy was lost = %+v, want %s", got, d.Path)
	}
	liveRemotePath(t, s, d, runID, "")
	if got := repairs(); len(got) != 0 {
		t.Fatalf("repairs once a new copy is live = %+v", got)
	}
}

// TestLoseStoredRemotePathOnlyFromTheStateRead: a verify finding marks a
// row lost only while it is still in the state the pass read, so a push
// that moved the row in between keeps its record.
func TestLoseStoredRemotePathOnlyFromTheStateRead(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	d, _, runID := remotePathFixture(t, s)
	id := liveRemotePath(t, s, d, runID, "")
	if err := s.BeginRemotePathsDisplace(ctx, runID, id); err != nil {
		t.Fatal(err)
	}
	if moved, err := s.LoseStoredRemotePath(ctx, id, RemotePathLive); err != nil || moved {
		t.Fatalf("lose a row read live that is now displacing = %t, %v; want untouched", moved, err)
	}
	if err := s.ConfirmRemotePathsDisplaced(ctx, id); err != nil {
		t.Fatal(err)
	}
	if moved, err := s.LoseStoredRemotePath(ctx, id, RemotePathDisplaced); err != nil || !moved {
		t.Fatalf("lose a displaced row = %t, %v; want moved", moved, err)
	}
}

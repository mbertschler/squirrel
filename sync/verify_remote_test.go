package sync

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// TestVerifyRemoteMatchStampsVerified: a clean pass stamps every
// recorded object verified and records an audit run whose
// 'verify-destination' note names the destination.
func TestVerifyRemoteMatchStampsVerified(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.write(t, "b.txt", "beta")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}

	ctx := context.Background()
	rep, err := VerifyRemote(ctx, f.store, f.rcl, f.pair.Destination)
	if err != nil {
		t.Fatalf("VerifyRemote: %v", err)
	}
	if rep.Objects != 2 || rep.Verified != 2 || !rep.Clean() {
		t.Fatalf("rep = %+v, want 2/2 verified and clean", rep)
	}
	for _, path := range []string{"a.txt", "b.txt"} {
		if obj := f.remoteObject(t, path); !obj.VerifiedAtNs.Valid {
			t.Fatalf("object for %s not stamped verified: %+v", path, obj)
		}
	}

	run, err := f.store.GetRun(ctx, rep.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Kind != store.RunKindAudit || run.Status != store.RunStatusSuccess ||
		run.VolumeID.Valid || run.Destination.Valid || run.FileCount != 2 {
		t.Fatalf("run = %+v, want a successful destination-less audit run over 2 objects", run)
	}
	audits, err := f.store.ListRunAudit(ctx, rep.RunID)
	if err != nil {
		t.Fatalf("ListRunAudit: %v", err)
	}
	var note string
	for _, a := range audits {
		if a.Transition == store.TransitionVerifyDestination {
			note = a.Note.String
		}
	}
	if !strings.Contains(note, "destination=offsite") || !strings.Contains(note, "verified=2") {
		t.Fatalf("verify-destination note = %q, want destination and counters", note)
	}
}

// TestVerifyRemoteMismatchLatchesAlarmThenClears: a mismatch latches a
// standing per-destination alarm (#157, F30), and a subsequent clean pass
// auto-clears it. Both transitions land in the runs_audit trail.
func TestVerifyRemoteMismatchLatchesAlarmThenClears(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}
	ctx := context.Background()

	if err := os.Setenv("RCLONE_FAKE_HASH_PREFIX", "tampered-"); err != nil {
		t.Fatalf("set tamper: %v", err)
	}
	dirty, err := VerifyRemote(ctx, f.store, f.rcl, f.pair.Destination)
	if err != nil {
		t.Fatalf("VerifyRemote (tampered): %v", err)
	}
	if !dirty.AlarmRaised {
		t.Fatalf("tampered pass did not raise an alarm: %+v", dirty)
	}
	alarm, err := f.store.GetDestinationAlarm(ctx, f.pair.Destination.Name)
	if err != nil {
		t.Fatalf("GetDestinationAlarm: %v", err)
	}
	if alarm.Kind != store.AlarmKindVerifyMismatch || alarm.RaisedRunID != dirty.RunID {
		t.Fatalf("alarm = %+v, want verify-mismatch raised by run %d", alarm, dirty.RunID)
	}
	if n := countAuditTransition(t, f.store, dirty.RunID, store.TransitionAlarmRaise); n != 1 {
		t.Fatalf("alarm-raise audit count = %d, want 1", n)
	}

	if err := os.Unsetenv("RCLONE_FAKE_HASH_PREFIX"); err != nil {
		t.Fatalf("unset tamper: %v", err)
	}
	clean, err := VerifyRemote(ctx, f.store, f.rcl, f.pair.Destination)
	if err != nil {
		t.Fatalf("VerifyRemote (clean): %v", err)
	}
	if !clean.Clean() || !clean.AlarmCleared {
		t.Fatalf("clean pass did not auto-clear the alarm: %+v", clean)
	}
	if _, err := f.store.GetDestinationAlarm(ctx, f.pair.Destination.Name); !store.IsNotFound(err) {
		t.Fatalf("alarm still latched after clean pass: %v", err)
	}
	if n := countAuditTransition(t, f.store, clean.RunID, store.TransitionAlarmClear); n != 1 {
		t.Fatalf("alarm-clear audit count = %d, want 1", n)
	}
}

func countAuditTransition(t *testing.T, s *store.Store, runID int64, transition string) int {
	t.Helper()
	audits, err := s.ListRunAudit(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListRunAudit(%d): %v", runID, err)
	}
	n := 0
	for _, a := range audits {
		if a.Transition == transition {
			n++
		}
	}
	return n
}

// TestVerifyRemoteMismatchIsLoudAndPreservesEvidence: a changed provider
// checksum is reported per object, marks the run partial, and leaves
// both the recorded fingerprint and the verification stamp untouched.
func TestVerifyRemoteMismatchIsLoudAndPreservesEvidence(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}
	recorded := f.remoteObject(t, "a.txt")

	t.Setenv("RCLONE_FAKE_HASH_PREFIX", "tampered-")
	ctx := context.Background()
	rep, err := VerifyRemote(ctx, f.store, f.rcl, f.pair.Destination)
	if err != nil {
		t.Fatalf("VerifyRemote: %v", err)
	}
	if rep.Clean() || len(rep.Mismatched) != 1 || rep.Verified != 0 {
		t.Fatalf("rep = %+v, want exactly one mismatch", rep)
	}
	m := rep.Mismatched[0]
	if m.Hash != blake3Hex("alpha") || m.Algo != "sha256" ||
		m.Recorded != recorded.Checksum.String || m.Actual != "tampered-"+recorded.Checksum.String {
		t.Fatalf("mismatch = %+v, want recorded vs tampered values", m)
	}

	after := f.remoteObject(t, "a.txt")
	if after.Checksum != recorded.Checksum || after.VerifiedAtNs != recorded.VerifiedAtNs {
		t.Fatalf("object = %+v after mismatch, want the recorded fingerprint and capture-time stamp preserved unchanged", after)
	}
	run, err := f.store.GetRun(ctx, rep.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunStatusPartial || !run.Error.Valid {
		t.Fatalf("run = %+v, want partial with an error message", run)
	}
}

// TestVerifyRemotePopulatesPendingPair: objects uploaded without a
// fingerprint get one recorded and stamped verified on the first pass
// (counted as populated, since it is the first record), and re-confirm as
// matches on the next.
func TestVerifyRemotePopulatesPendingPair(t *testing.T) {
	f := setupContentAddressedFixture(t)
	t.Setenv("RCLONE_FAKE_NO_HASHES", "1")
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}

	t.Setenv("RCLONE_FAKE_NO_HASHES", "")
	ctx := context.Background()
	rep, err := VerifyRemote(ctx, f.store, f.rcl, f.pair.Destination)
	if err != nil {
		t.Fatalf("first VerifyRemote: %v", err)
	}
	if rep.Populated != 1 || rep.Verified != 0 || !rep.Clean() {
		t.Fatalf("rep = %+v, want one populated fingerprint and none verified", rep)
	}
	obj := f.remoteObject(t, "a.txt")
	if obj.ChecksumAlgo.String != "sha256" || !obj.VerifiedAtNs.Valid {
		t.Fatalf("object = %+v, want a fresh sha256 fingerprint stamped verified", obj)
	}

	rep, err = VerifyRemote(ctx, f.store, f.rcl, f.pair.Destination)
	if err != nil {
		t.Fatalf("second VerifyRemote: %v", err)
	}
	if rep.Verified != 1 || rep.Populated != 0 {
		t.Fatalf("rep = %+v, want the populated fingerprint verified on the second pass", rep)
	}
}

// TestVerifyRemotePendingStaysPendingWithoutChecksums: a backend that
// exposes no checksums keeps the pair pending without failing the pass.
func TestVerifyRemotePendingStaysPendingWithoutChecksums(t *testing.T) {
	f := setupContentAddressedFixture(t)
	t.Setenv("RCLONE_FAKE_NO_HASHES", "1")
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}

	rep, err := VerifyRemote(context.Background(), f.store, f.rcl, f.pair.Destination)
	if err != nil {
		t.Fatalf("VerifyRemote: %v", err)
	}
	if rep.Pending != 1 || rep.Populated != 0 || !rep.Clean() {
		t.Fatalf("rep = %+v, want one still-pending object on a clean pass", rep)
	}
}

// TestVerifyRemoteMissingObject: a recorded object absent from the
// remote is reported loudly and marks the run partial.
func TestVerifyRemoteMissingObject(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := os.Remove(f.remoteBlob(ObjectsDirName, blake3Hex("alpha"))); err != nil {
		t.Fatalf("remove remote object: %v", err)
	}

	ctx := context.Background()
	rep, err := VerifyRemote(ctx, f.store, f.rcl, f.pair.Destination)
	if err != nil {
		t.Fatalf("VerifyRemote: %v", err)
	}
	if len(rep.Missing) != 1 || rep.Missing[0] != blake3Hex("alpha") || rep.Clean() {
		t.Fatalf("rep = %+v, want the missing object reported", rep)
	}
	run, err := f.store.GetRun(ctx, rep.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunStatusPartial {
		t.Fatalf("run status = %q, want partial", run.Status)
	}
}

// TestVerifyRemoteCountsUnrecordedObjects: orphan objects from runs that
// failed before recording are counted, not failed on — they are harmless
// without a manifest mapping them.
func TestVerifyRemoteCountsUnrecordedObjects(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}
	orphan := f.remoteBlob(ObjectsDirName, strings.Repeat("ab", 32))
	if err := os.WriteFile(orphan, []byte("orphan"), 0o644); err != nil {
		t.Fatalf("write orphan: %v", err)
	}

	rep, err := VerifyRemote(context.Background(), f.store, f.rcl, f.pair.Destination)
	if err != nil {
		t.Fatalf("VerifyRemote: %v", err)
	}
	if rep.Unrecorded != 1 || rep.Verified != 1 || !rep.Clean() {
		t.Fatalf("rep = %+v, want one unrecorded orphan on a clean pass", rep)
	}
}

// TestVerifyRemoteNoRecordedObjects: a destination with no upload
// records reports zero objects and writes no run.
func TestVerifyRemoteNoRecordedObjects(t *testing.T) {
	f := setupContentAddressedFixture(t)
	rep, err := VerifyRemote(context.Background(), f.store, f.rcl, f.pair.Destination)
	if err != nil {
		t.Fatalf("VerifyRemote: %v", err)
	}
	if rep.Objects != 0 || rep.RunID != 0 {
		t.Fatalf("rep = %+v, want no objects and no run", rep)
	}
}

func TestVerifyRemoteRefusesMirrorDestination(t *testing.T) {
	f := setupContentAddressedFixture(t)
	dest := &config.Destination{Name: "mirror", Type: "sftp", Root: "/data", Layout: config.LayoutMirror}
	_, err := VerifyRemote(context.Background(), f.store, f.rcl, dest)
	if err == nil || !strings.Contains(err.Error(), "content-addressed") {
		t.Fatalf("err = %v, want content-addressed refusal", err)
	}
}

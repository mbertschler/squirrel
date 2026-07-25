package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// remoteObjectFixture upserts one file so a contents row exists, and
// returns its content id plus the run that observed it.
func remoteObjectFixture(t *testing.T, s *Store) (contentID, runID int64) {
	t.Helper()
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	runID = makeRun(t, s, vID)
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "a.txt", Blake3: digest(0xaa), SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	row, err := s.GetByPath(ctx, vID, "a.txt")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	return row.ContentID, runID
}

// nullStr wraps a literal into a valid sql.NullString for fixture
// brevity.
func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: true}
}

// TestRemoteObjectRoundTrip: insert records the fingerprint verbatim,
// Get returns it, and a verification pass stamps verified_at_ns.
func TestRemoteObjectRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	contentID, runID := remoteObjectFixture(t, s)

	obj := RemoteObject{
		ContentID:     contentID,
		Destination:   "bucket-a",
		UploadedRunID: runID,
		ChecksumAlgo:  nullStr("etag-md5"),
		Checksum:      nullStr("9e107d9d372bb6826bd81d3542a419d6"),
	}
	if err := s.InsertRemoteObject(ctx, obj); err != nil {
		t.Fatalf("InsertRemoteObject: %v", err)
	}

	got, err := s.GetRemoteObject(ctx, contentID, "bucket-a")
	if err != nil {
		t.Fatalf("GetRemoteObject: %v", err)
	}
	if got.ChecksumAlgo != obj.ChecksumAlgo || got.Checksum != obj.Checksum || got.UploadedRunID != runID {
		t.Fatalf("round trip = %+v, want %+v", got, obj)
	}
	if got.VerifiedAtNs.Valid {
		t.Fatalf("fresh upload already verified: %+v", got.VerifiedAtNs)
	}

	if err := s.MarkRemoteObjectVerified(ctx, contentID, "bucket-a", 12345); err != nil {
		t.Fatalf("MarkRemoteObjectVerified: %v", err)
	}
	got, err = s.GetRemoteObject(ctx, contentID, "bucket-a")
	if err != nil {
		t.Fatalf("GetRemoteObject after verify: %v", err)
	}
	if !got.VerifiedAtNs.Valid || got.VerifiedAtNs.Int64 != 12345 {
		t.Fatalf("VerifiedAtNs = %+v, want 12345", got.VerifiedAtNs)
	}
}

// TestSetRemoteObjectFingerprintFillsPendingPair: the scan-back pass fills
// the NULL pair left by a fingerprint-pending upload and stamps the
// verification instant in the same write (the read-back is the first
// verification), leaving the rest of the row untouched.
func TestSetRemoteObjectFingerprintFillsPendingPair(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	contentID, runID := remoteObjectFixture(t, s)

	if err := s.InsertRemoteObject(ctx, RemoteObject{
		ContentID: contentID, Destination: "bucket-a", UploadedRunID: runID,
	}); err != nil {
		t.Fatalf("InsertRemoteObject: %v", err)
	}
	if err := s.SetRemoteObjectFingerprint(ctx, contentID, "bucket-a", "sha256", "deadbeef", 12345); err != nil {
		t.Fatalf("SetRemoteObjectFingerprint: %v", err)
	}
	got, err := s.GetRemoteObject(ctx, contentID, "bucket-a")
	if err != nil {
		t.Fatalf("GetRemoteObject: %v", err)
	}
	if got.ChecksumAlgo != nullStr("sha256") || got.Checksum != nullStr("deadbeef") {
		t.Fatalf("pair = (%+v, %+v), want (sha256, deadbeef)", got.ChecksumAlgo, got.Checksum)
	}
	if got.UploadedRunID != runID || !got.VerifiedAtNs.Valid || got.VerifiedAtNs.Int64 != 12345 {
		t.Fatalf("row = %+v, want run %d and verification stamp 12345", got, runID)
	}
}

// TestSetRemoteObjectFingerprintRefusesOverwrite: a recorded fingerprint is
// the comparison baseline for every later verification and must never be
// silently replaced.
func TestSetRemoteObjectFingerprintRefusesOverwrite(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	contentID, runID := remoteObjectFixture(t, s)

	if err := s.InsertRemoteObject(ctx, RemoteObject{
		ContentID: contentID, Destination: "bucket-a", UploadedRunID: runID,
		ChecksumAlgo: nullStr("sha256"), Checksum: nullStr("original"),
	}); err != nil {
		t.Fatalf("InsertRemoteObject: %v", err)
	}
	err := s.SetRemoteObjectFingerprint(ctx, contentID, "bucket-a", "sha256", "tampered", 12345)
	if err == nil {
		t.Fatalf("overwrite of a recorded fingerprint succeeded")
	}
	got, err := s.GetRemoteObject(ctx, contentID, "bucket-a")
	if err != nil {
		t.Fatalf("GetRemoteObject: %v", err)
	}
	if got.Checksum != nullStr("original") {
		t.Fatalf("checksum = %+v after refused overwrite, want original", got.Checksum)
	}
}

func TestSetRemoteObjectFingerprintUnknownPair(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetRemoteObjectFingerprint(context.Background(), 1, "bucket-a", "sha256", "x", 12345); err == nil {
		t.Fatalf("fingerprint for unrecorded upload succeeded, want error")
	}
}

// TestListRemoteObjects: the listing joins the content hash in, filters
// by destination, and orders by hash.
func TestListRemoteObjects(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	runID := makeRun(t, s, vID)
	for i, b := range []byte{0xbb, 0xaa} {
		path := []string{"b.txt", "a.txt"}[i]
		if err := s.Upsert(ctx, FileRow{
			VolumeID: vID, Path: path, Blake3: digest(b), SizeBytes: 1, MtimeNs: 1,
			Status: StatusPresent, FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: 1,
		}, nil); err != nil {
			t.Fatalf("Upsert %s: %v", path, err)
		}
		row, err := s.GetByPath(ctx, vID, path)
		if err != nil {
			t.Fatalf("GetByPath %s: %v", path, err)
		}
		if err := s.InsertRemoteObject(ctx, RemoteObject{
			ContentID: row.ContentID, Destination: "bucket-a", UploadedRunID: runID,
		}); err != nil {
			t.Fatalf("InsertRemoteObject %s: %v", path, err)
		}
		if i == 0 {
			if err := s.InsertRemoteObject(ctx, RemoteObject{
				ContentID: row.ContentID, Destination: "bucket-b", UploadedRunID: runID,
				ChecksumAlgo: nullStr("sha1"), Checksum: nullStr("ff"),
			}); err != nil {
				t.Fatalf("InsertRemoteObject bucket-b: %v", err)
			}
		}
	}

	got, err := s.ListRemoteObjects(ctx, "bucket-a")
	if err != nil {
		t.Fatalf("ListRemoteObjects: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (bucket-b row filtered out): %+v", len(got), got)
	}
	if string(got[0].Blake3) != string(digest(0xaa)) || string(got[1].Blake3) != string(digest(0xbb)) {
		t.Fatalf("order = %x, %x; want ascending by hash", got[0].Blake3, got[1].Blake3)
	}
	if got[0].Destination != "bucket-a" || got[0].ChecksumAlgo.Valid {
		t.Fatalf("row = %+v, want pending bucket-a record", got[0])
	}
}

// TestBeginRemoteVerifyRun: the verification pass rides on a kind='audit'
// run with no volume and no destination, finishable like any other run.
func TestBeginRemoteVerifyRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id, err := s.BeginRemoteVerifyRun(ctx)
	if err != nil {
		t.Fatalf("BeginRemoteVerifyRun: %v", err)
	}
	run, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Kind != RunKindAudit || run.VolumeID.Valid || run.Destination.Valid || run.Status != RunStatusRunning {
		t.Fatalf("run = %+v, want a running audit run with NULL volume and destination", run)
	}
	if err := s.FinishRun(ctx, id, RunStatusSuccess, "", 3); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}

// TestBeginDurabilityPullRun: an agent-scheduled durability pull rides on a
// kind='audit' run with no volume and no destination, so it stays out of the
// per-volume drift-audit reads even though it concerns a specific volume —
// the pulled volume/peer live in the run's runs_audit note instead.
func TestBeginDurabilityPullRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	v, err := s.CreateVolume(ctx, "media", "/data/media")
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	id, err := s.BeginDurabilityPullRun(ctx)
	if err != nil {
		t.Fatalf("BeginDurabilityPullRun: %v", err)
	}
	run, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Kind != RunKindAudit || run.VolumeID.Valid || run.Destination.Valid || run.Status != RunStatusRunning {
		t.Fatalf("run = %+v, want a running audit run with NULL volume and destination", run)
	}
	if err := s.AppendRunAudit(ctx, RunAuditEntry{RunID: id, Transition: TransitionPullDurability, Note: "volume=media peer=nas fetched=3 applied=2 dropped=0 rewinds=0"}); err != nil {
		t.Fatalf("AppendRunAudit: %v", err)
	}
	if err := s.FinishRun(ctx, id, RunStatusSuccess, "", 2); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	// The NULL volume_id keeps it out of the drift-since-last-sync handshake
	// read, which is scoped to a volume.
	audits, err := s.ListAuditRunsSince(ctx, v.ID, 0)
	if err != nil {
		t.Fatalf("ListAuditRunsSince: %v", err)
	}
	if len(audits) != 0 {
		t.Fatalf("durability-pull run leaked into volume drift audits: %+v", audits)
	}
}

// TestRemoteObjectInsertRefusesDuplicate: the fingerprint recorded at
// upload time is what later verifications compare against, so a second
// insert for the same (content, destination) must fail loudly instead
// of replacing it.
func TestRemoteObjectInsertRefusesDuplicate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	contentID, runID := remoteObjectFixture(t, s)

	obj := RemoteObject{
		ContentID: contentID, Destination: "bucket-a", UploadedRunID: runID,
		ChecksumAlgo: nullStr("etag-md5"), Checksum: nullStr("aaaa"),
	}
	if err := s.InsertRemoteObject(ctx, obj); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	obj.Checksum = nullStr("bbbb")
	if err := s.InsertRemoteObject(ctx, obj); err == nil {
		t.Fatalf("duplicate insert succeeded; fingerprint silently replaced")
	}
	got, err := s.GetRemoteObject(ctx, contentID, "bucket-a")
	if err != nil {
		t.Fatalf("GetRemoteObject: %v", err)
	}
	if got.Checksum != nullStr("aaaa") {
		t.Fatalf("checksum = %+v after refused duplicate, want original %q", got.Checksum, "aaaa")
	}
}

// TestRemoteObjectFingerprintPending: the content-addressed push
// records the upload with the checksum pair NULL; the record gates
// upload-once dedup (HasRemoteObject) until the scan-back pass fills
// the fingerprint in.
func TestRemoteObjectFingerprintPending(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	contentID, runID := remoteObjectFixture(t, s)

	has, err := s.HasRemoteObject(ctx, contentID, "bucket-a")
	if err != nil || has {
		t.Fatalf("HasRemoteObject before insert = (%t, %v), want (false, nil)", has, err)
	}
	if err := s.InsertRemoteObject(ctx, RemoteObject{
		ContentID: contentID, Destination: "bucket-a", UploadedRunID: runID,
	}); err != nil {
		t.Fatalf("InsertRemoteObject (pending fingerprint): %v", err)
	}
	got, err := s.GetRemoteObject(ctx, contentID, "bucket-a")
	if err != nil {
		t.Fatalf("GetRemoteObject: %v", err)
	}
	if got.ChecksumAlgo.Valid || got.Checksum.Valid {
		t.Fatalf("pending upload carries a fingerprint: %+v", got)
	}
	has, err = s.HasRemoteObject(ctx, contentID, "bucket-a")
	if err != nil || !has {
		t.Fatalf("HasRemoteObject after insert = (%t, %v), want (true, nil)", has, err)
	}
}

// TestRemoteObjectChecksumPairEnforced: a checksum without its
// algorithm (or vice versa) is uninterpretable, refused by both the Go
// validation and the schema CHECK.
func TestRemoteObjectChecksumPairEnforced(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	contentID, runID := remoteObjectFixture(t, s)

	if err := s.InsertRemoteObject(ctx, RemoteObject{
		ContentID: contentID, Destination: "bucket-a", UploadedRunID: runID,
		ChecksumAlgo: nullStr("etag-md5"),
	}); err == nil {
		t.Fatalf("algo without checksum accepted")
	}
	if err := s.InsertRemoteObject(ctx, RemoteObject{
		ContentID: contentID, Destination: "bucket-a", UploadedRunID: runID,
		Checksum: nullStr("aaaa"),
	}); err == nil {
		t.Fatalf("checksum without algo accepted")
	}
	// The schema CHECK is the backstop when a write bypasses
	// InsertRemoteObject's validation.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO remote_objects (content_id, destination, uploaded_run_id, checksum_algo, checksum)
		VALUES (?, 'bucket-a', ?, 'etag-md5', NULL)
	`, contentID, runID); err == nil {
		t.Fatalf("schema CHECK accepted a half-set checksum pair")
	}
}

// TestRemoteObjectFKsEnforced: content_id and uploaded_run_id are real
// FKs — a fingerprint for content or a run the index doesn't know is a
// caller bug.
func TestRemoteObjectFKsEnforced(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	contentID, runID := remoteObjectFixture(t, s)

	if err := s.InsertRemoteObject(ctx, RemoteObject{
		ContentID: 99999, Destination: "bucket-a", UploadedRunID: runID,
		ChecksumAlgo: nullStr("etag-md5"), Checksum: nullStr("aaaa"),
	}); err == nil {
		t.Fatalf("bogus content id accepted; FK not enforced")
	}
	if err := s.InsertRemoteObject(ctx, RemoteObject{
		ContentID: contentID, Destination: "bucket-a", UploadedRunID: 99999,
		ChecksumAlgo: nullStr("etag-md5"), Checksum: nullStr("aaaa"),
	}); err == nil {
		t.Fatalf("bogus run id accepted; FK not enforced")
	}
}

// TestMarkRemoteObjectVerifiedUnknownPair: verifying an unrecorded
// upload errors.
func TestMarkRemoteObjectVerifiedUnknownPair(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.MarkRemoteObjectVerified(ctx, 1, "bucket-a", 1); err == nil {
		t.Fatalf("verify of unrecorded upload succeeded, want error")
	}
}

// TestGetRemoteObjectNotFound: the missing pair surfaces as
// sql.ErrNoRows so callers share the store's IsNotFound convention.
func TestGetRemoteObjectNotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, err := s.GetRemoteObject(ctx, 1, "bucket-a")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

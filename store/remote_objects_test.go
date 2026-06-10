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
		ChecksumAlgo:  "etag-md5",
		Checksum:      "9e107d9d372bb6826bd81d3542a419d6",
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
		ChecksumAlgo: "etag-md5", Checksum: "aaaa",
	}
	if err := s.InsertRemoteObject(ctx, obj); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	obj.Checksum = "bbbb"
	if err := s.InsertRemoteObject(ctx, obj); err == nil {
		t.Fatalf("duplicate insert succeeded; fingerprint silently replaced")
	}
	got, err := s.GetRemoteObject(ctx, contentID, "bucket-a")
	if err != nil {
		t.Fatalf("GetRemoteObject: %v", err)
	}
	if got.Checksum != "aaaa" {
		t.Fatalf("checksum = %q after refused duplicate, want original %q", got.Checksum, "aaaa")
	}
}

// TestRemoteObjectFKsEnforced: content_id and uploaded_run_id are real
// FKs — a fingerprint for content or a run the index doesn't know is a
// caller bug, not recordable state.
func TestRemoteObjectFKsEnforced(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	contentID, runID := remoteObjectFixture(t, s)

	if err := s.InsertRemoteObject(ctx, RemoteObject{
		ContentID: 99999, Destination: "bucket-a", UploadedRunID: runID,
		ChecksumAlgo: "etag-md5", Checksum: "aaaa",
	}); err == nil {
		t.Fatalf("bogus content id accepted; FK not enforced")
	}
	if err := s.InsertRemoteObject(ctx, RemoteObject{
		ContentID: contentID, Destination: "bucket-a", UploadedRunID: 99999,
		ChecksumAlgo: "etag-md5", Checksum: "aaaa",
	}); err == nil {
		t.Fatalf("bogus run id accepted; FK not enforced")
	}
}

// TestMarkRemoteObjectVerifiedUnknownPair: verifying an unrecorded
// upload errors rather than silently affecting zero rows.
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

package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// remotePackFixture creates one pack (with one member) so a packs row and a
// pack_members row exist, and returns the pack id, its single member's
// content id, and the run that created it.
func remotePackFixture(t *testing.T, s *Store) (packID, contentID, runID int64) {
	t.Helper()
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	runID = makeRun(t, s, vID)
	contentID = packContentFixture(t, s, vID, runID, "a.txt", 0xa1)
	if err := s.InsertPacks(ctx, []PackWrite{{
		Pack:    Pack{PackKey: packKey(0x11), SizeBytes: 500, MemberCount: 1, CreatedRunID: runID},
		Members: []PackMember{{ContentID: contentID, ByteOffset: 512, ByteLength: 1}},
	}}); err != nil {
		t.Fatalf("InsertPacks: %v", err)
	}
	pack, err := s.GetPackByKey(ctx, packKey(0x11))
	if err != nil {
		t.Fatalf("GetPackByKey: %v", err)
	}
	return pack.ID, contentID, runID
}

// TestRemotePackRoundTrip: insert records a pending pack upload, the
// scan-back fingerprint fills the pair and stamps verified in one write,
// and Get reads it back.
func TestRemotePackRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	packID, _, runID := remotePackFixture(t, s)

	if err := s.InsertRemotePack(ctx, RemotePack{
		PackID: packID, Destination: "bucket-a", UploadedRunID: runID,
	}); err != nil {
		t.Fatalf("InsertRemotePack: %v", err)
	}
	got, err := s.GetRemotePack(ctx, packID, "bucket-a")
	if err != nil {
		t.Fatalf("GetRemotePack: %v", err)
	}
	if got.ChecksumAlgo.Valid || got.Checksum.Valid || got.VerifiedAtNs.Valid {
		t.Fatalf("fresh upload carries a fingerprint: %+v", got)
	}

	if err := s.SetRemotePackFingerprint(ctx, packID, "bucket-a", "etag-md5", "abc-12", 4242); err != nil {
		t.Fatalf("SetRemotePackFingerprint: %v", err)
	}
	got, err = s.GetRemotePack(ctx, packID, "bucket-a")
	if err != nil {
		t.Fatalf("GetRemotePack after fingerprint: %v", err)
	}
	if got.ChecksumAlgo != nullStr("etag-md5") || got.Checksum != nullStr("abc-12") {
		t.Fatalf("pair = (%+v, %+v), want (etag-md5, abc-12)", got.ChecksumAlgo, got.Checksum)
	}
	if !got.VerifiedAtNs.Valid || got.VerifiedAtNs.Int64 != 4242 {
		t.Fatalf("VerifiedAtNs = %+v, want 4242 (scan-back read verifies at capture)", got.VerifiedAtNs)
	}
}

// TestSetRemotePackFingerprintRefusesOverwrite: a recorded fingerprint is
// the comparison baseline and must never be silently replaced.
func TestSetRemotePackFingerprintRefusesOverwrite(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	packID, _, runID := remotePackFixture(t, s)

	if err := s.InsertRemotePack(ctx, RemotePack{
		PackID: packID, Destination: "bucket-a", UploadedRunID: runID,
		ChecksumAlgo: nullStr("sha256"), Checksum: nullStr("original"),
	}); err != nil {
		t.Fatalf("InsertRemotePack: %v", err)
	}
	if err := s.SetRemotePackFingerprint(ctx, packID, "bucket-a", "sha256", "tampered", 1); err == nil {
		t.Fatalf("overwrite of a recorded pack fingerprint succeeded")
	}
	got, err := s.GetRemotePack(ctx, packID, "bucket-a")
	if err != nil {
		t.Fatalf("GetRemotePack: %v", err)
	}
	if got.Checksum != nullStr("original") {
		t.Fatalf("checksum = %+v after refused overwrite, want original", got.Checksum)
	}
}

// TestSetRemotePackFingerprintUnknownPair: filling a fingerprint for an
// unrecorded upload errors.
func TestSetRemotePackFingerprintUnknownPair(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetRemotePackFingerprint(context.Background(), 1, "bucket-a", "sha256", "x", 1); err == nil {
		t.Fatalf("fingerprint for unrecorded pack upload succeeded, want error")
	}
}

// TestRemotePackInsertRefusesDuplicate: the fingerprint recorded at upload
// is the verification baseline, so a second insert for the same (pack,
// destination) must fail rather than replace it.
func TestRemotePackInsertRefusesDuplicate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	packID, _, runID := remotePackFixture(t, s)

	p := RemotePack{PackID: packID, Destination: "bucket-a", UploadedRunID: runID,
		ChecksumAlgo: nullStr("etag-md5"), Checksum: nullStr("aaaa")}
	if err := s.InsertRemotePack(ctx, p); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	p.Checksum = nullStr("bbbb")
	if err := s.InsertRemotePack(ctx, p); err == nil {
		t.Fatalf("duplicate insert succeeded; fingerprint silently replaced")
	}
}

// TestRemotePackChecksumPairEnforced: a half-set pair is refused by both
// the Go validation and the schema CHECK.
func TestRemotePackChecksumPairEnforced(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	packID, _, runID := remotePackFixture(t, s)

	if err := s.InsertRemotePack(ctx, RemotePack{
		PackID: packID, Destination: "bucket-a", UploadedRunID: runID, ChecksumAlgo: nullStr("etag-md5"),
	}); err == nil {
		t.Fatalf("algo without checksum accepted")
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO remote_packs (pack_id, destination, uploaded_run_id, checksum_algo, checksum)
		VALUES (?, 'bucket-a', ?, 'etag-md5', NULL)
	`, packID, runID); err == nil {
		t.Fatalf("schema CHECK accepted a half-set checksum pair")
	}
}

// TestRemotePackFKsEnforced: pack_id and uploaded_run_id are real FKs.
func TestRemotePackFKsEnforced(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	packID, _, runID := remotePackFixture(t, s)

	if err := s.InsertRemotePack(ctx, RemotePack{
		PackID: 99999, Destination: "bucket-a", UploadedRunID: runID,
	}); err == nil {
		t.Fatalf("bogus pack id accepted; FK not enforced")
	}
	if err := s.InsertRemotePack(ctx, RemotePack{
		PackID: packID, Destination: "bucket-a", UploadedRunID: 99999,
	}); err == nil {
		t.Fatalf("bogus run id accepted; FK not enforced")
	}
}

// TestListRemotePacks joins the pack key in, filters by destination, and
// orders by key.
func TestListRemotePacks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	runID := makeRun(t, s, vID)
	c1 := packContentFixture(t, s, vID, runID, "a.txt", 0xa1)
	c2 := packContentFixture(t, s, vID, runID, "b.txt", 0xa2)
	if err := s.InsertPacks(ctx, []PackWrite{
		{Pack: Pack{PackKey: packKey(0x22), SizeBytes: 1, MemberCount: 1, CreatedRunID: runID},
			Members: []PackMember{{ContentID: c1, ByteOffset: 0, ByteLength: 1}}},
		{Pack: Pack{PackKey: packKey(0x11), SizeBytes: 1, MemberCount: 1, CreatedRunID: runID},
			Members: []PackMember{{ContentID: c2, ByteOffset: 0, ByteLength: 1}}},
	}); err != nil {
		t.Fatalf("InsertPacks: %v", err)
	}
	for _, k := range [][]byte{packKey(0x22), packKey(0x11)} {
		pack, err := s.GetPackByKey(ctx, k)
		if err != nil {
			t.Fatalf("GetPackByKey: %v", err)
		}
		if err := s.InsertRemotePack(ctx, RemotePack{PackID: pack.ID, Destination: "bucket-a", UploadedRunID: runID}); err != nil {
			t.Fatalf("InsertRemotePack: %v", err)
		}
	}
	got, err := s.ListRemotePacks(ctx, "bucket-a")
	if err != nil {
		t.Fatalf("ListRemotePacks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if string(got[0].PackKey) != string(packKey(0x11)) || string(got[1].PackKey) != string(packKey(0x22)) {
		t.Fatalf("order = %x, %x; want ascending by key", got[0].PackKey, got[1].PackKey)
	}
}

// TestGetRemotePackNotFound: the missing pair surfaces as sql.ErrNoRows.
func TestGetRemotePackNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetRemotePack(context.Background(), 1, "bucket-a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

// TestTwoSourcePresence: a packed content counts as present-and-verified on
// a destination via its pack's remote_packs row — the two-source predicate
// the dedup check and the offload gate share. A content-addressed object
// row still counts too, and neither counts on a different destination.
func TestTwoSourcePresence(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	packID, packed, runID := remotePackFixture(t, s)
	// A separate content uploaded as a plain object (same volume; makeVolume
	// is idempotent on the path).
	vID := makeVolume(t, s, "/v")
	obj := packContentFixture(t, s, vID, runID, "big.bin", 0xc0)

	// Before any upload record: neither present nor verified.
	assertPresence(t, s, packed, "bucket-a", false, false)
	assertPresence(t, s, obj, "bucket-a", false, false)

	// Pending pack upload: present (dedup) but not verified (gate).
	if err := s.InsertRemotePack(ctx, RemotePack{PackID: packID, Destination: "bucket-a", UploadedRunID: runID}); err != nil {
		t.Fatalf("InsertRemotePack: %v", err)
	}
	assertPresence(t, s, packed, "bucket-a", true, false)
	// Not present on a different destination.
	assertPresence(t, s, packed, "bucket-b", false, false)

	// Fingerprint the pack: now verified via the pack source.
	if err := s.SetRemotePackFingerprint(ctx, packID, "bucket-a", "etag-md5", "abc-9", NowNs()); err != nil {
		t.Fatalf("SetRemotePackFingerprint: %v", err)
	}
	assertPresence(t, s, packed, "bucket-a", true, true)

	// The object source: pending, then verified.
	if err := s.InsertRemoteObject(ctx, RemoteObject{ContentID: obj, Destination: "bucket-a", UploadedRunID: runID}); err != nil {
		t.Fatalf("InsertRemoteObject: %v", err)
	}
	assertPresence(t, s, obj, "bucket-a", true, false)
	if err := s.SetRemoteObjectChecksum(ctx, obj, "bucket-a", "sha256", "deadbeef"); err != nil {
		t.Fatalf("SetRemoteObjectChecksum: %v", err)
	}
	assertPresence(t, s, obj, "bucket-a", true, false) // checksum but not yet verified
	if err := s.MarkRemoteObjectVerified(ctx, obj, "bucket-a", NowNs()); err != nil {
		t.Fatalf("MarkRemoteObjectVerified: %v", err)
	}
	assertPresence(t, s, obj, "bucket-a", true, true)
}

func assertPresence(t *testing.T, s *Store, contentID int64, dest string, wantPresent, wantVerified bool) {
	t.Helper()
	ctx := context.Background()
	present, err := s.ContentPresentOnDestination(ctx, contentID, dest)
	if err != nil {
		t.Fatalf("ContentPresentOnDestination: %v", err)
	}
	if present != wantPresent {
		t.Fatalf("ContentPresentOnDestination(content %d, %q) = %t, want %t", contentID, dest, present, wantPresent)
	}
	verified, err := s.ContentFingerprintVerified(ctx, contentID, dest)
	if err != nil {
		t.Fatalf("ContentFingerprintVerified: %v", err)
	}
	if verified != wantVerified {
		t.Fatalf("ContentFingerprintVerified(content %d, %q) = %t, want %t", contentID, dest, verified, wantVerified)
	}
}

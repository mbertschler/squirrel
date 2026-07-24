package sync

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

func TestExtractChecksum(t *testing.T) {
	cases := []struct {
		name   string
		dest   *config.Destination
		hashes map[string]string
		want   remoteChecksum
		ok     bool
	}{
		{
			name:   "s3 plain etag",
			dest:   &config.Destination{Type: "s3"},
			hashes: map[string]string{"md5": "9e107d9d372bb6826bd81d3542a419d6"},
			want:   remoteChecksum{Algo: AlgoEtagMD5, Value: "9e107d9d372bb6826bd81d3542a419d6"},
			ok:     true,
		},
		{
			name:   "s3 multipart composite etag is opaque",
			dest:   &config.Destination{Type: "s3"},
			hashes: map[string]string{"md5": "9e107d9d372bb6826bd81d3542a419d6-12"},
			want:   remoteChecksum{Algo: AlgoEtagMD5Composite, Value: "9e107d9d372bb6826bd81d3542a419d6-12"},
			ok:     true,
		},
		{
			name:   "s3 without etag",
			dest:   &config.Destination{Type: "s3"},
			hashes: map[string]string{"sha1": "aa"},
			ok:     false,
		},
		{
			name:   "sftp configured algo",
			dest:   &config.Destination{Type: "sftp", HashAlgo: "sha256"},
			hashes: map[string]string{"sha256": "aa", "md5": "bb"},
			want:   remoteChecksum{Algo: "sha256", Value: "aa"},
			ok:     true,
		},
		{
			name:   "sftp configured algo not exposed",
			dest:   &config.Destination{Type: "sftp", HashAlgo: "sha256"},
			hashes: map[string]string{"md5": "bb"},
			ok:     false,
		},
		{
			name:   "preference picks the strongest exposed hash",
			dest:   &config.Destination{Type: "b2"},
			hashes: map[string]string{"md5": "bb", "sha1": "aa"},
			want:   remoteChecksum{Algo: "sha1", Value: "aa"},
			ok:     true,
		},
		{
			name:   "unlisted hash names fall back to name order",
			dest:   &config.Destination{Type: "b2"},
			hashes: map[string]string{"quickxor": "qq", "dropbox": "dd"},
			want:   remoteChecksum{Algo: "dropbox", Value: "dd"},
			ok:     true,
		},
		{
			name: "no hashes",
			dest: &config.Destination{Type: "b2"},
			ok:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := extractChecksum(c.dest, c.hashes)
			if ok != c.ok || got != c.want {
				t.Fatalf("extractChecksum = (%+v, %t), want (%+v, %t)", got, ok, c.want, c.ok)
			}
		})
	}
}

// shimLog returns the recorded fake-rclone argv lines.
func (f *caFixture) shimLog(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.logPath)
	if err != nil {
		t.Fatalf("read shim log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// captureLines filters the shim log to the fingerprint/verify listing
// invocations: lsjson directory listings, as opposed to the --stat
// presence confirms.
func (f *caFixture) captureLines(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, line := range f.shimLog(t) {
		if strings.Contains(line, " lsjson ") && !strings.Contains(line, "--stat") {
			out = append(out, line)
		}
	}
	return out
}

// TestCaptureReadsUnderlyingRemote pins the privacy-critical argv shape
// of fingerprint capture on a crypt destination: the listing addresses
// the base remote (the fingerprint is over the stored ciphertext; with
// filename encryption off the underlying key equals the overlay path),
// requests exactly the configured hash type, and scopes the listing to
// this run's uploads.
func TestCaptureReadsUnderlyingRemote(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}

	lines := f.captureLines(t)
	if len(lines) != 1 {
		t.Fatalf("capture lsjson invocations = %d, want one batched call:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	line := lines[0]
	for _, want := range []string{
		"offsite:/data/objects",
		"--hash-type sha256",
		"--include " + blake3Hex("alpha"),
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("capture argv lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "offsite-crypt:") {
		t.Fatalf("capture addressed the crypt overlay:\n%s", line)
	}
}

// s3DestBlock declares a plain (no-crypt) content-addressed s3
// destination named offsite for the capture/verify tests, whose ETag
// reads are mocked via installS3Reader.
const s3DestBlock = `[destinations.offsite]
type     = "s3"
provider = "Other"
bucket   = "b"
root     = "data"
layout   = "content-addressed"
`

// TestCaptureRecordsEtagFlavorForS3: on an s3 destination the recorded
// fingerprint is the object ETag read via the S3 API, labeled etag-md5 for
// a plain whole-object value — and no rclone hash listing runs, since the
// ETag never comes through rclone.
func TestCaptureRecordsEtagFlavorForS3(t *testing.T) {
	f := setupCAFixture(t, s3DestBlock, "")
	installS3Reader(t, fakeS3Reader{etags: map[string]string{
		blake3Hex("alpha"): "9e107d9d372bb6826bd81d3542a419d6",
	}})
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}

	obj := f.remoteObject(t, "a.txt")
	if obj.ChecksumAlgo.String != AlgoEtagMD5 || obj.Checksum.String != "9e107d9d372bb6826bd81d3542a419d6" {
		t.Fatalf("remote object = %+v, want an etag-md5 fingerprint read via the S3 API", obj)
	}
	if lines := f.captureLines(t); len(lines) != 0 {
		t.Fatalf("s3 capture must not run an rclone hash listing:\n%s", strings.Join(lines, "\n"))
	}
}

// TestCaptureCompositeEtagRecordedOpaquely: a multipart-style composite
// ETag read via the S3 API is recorded as-is under its own flavor and
// verifies by verbatim comparison — no recomputation anywhere.
func TestCaptureCompositeEtagRecordedOpaquely(t *testing.T) {
	f := setupCAFixture(t, s3DestBlock, "")
	installS3Reader(t, fakeS3Reader{etags: map[string]string{
		blake3Hex("alpha"): "9e107d9d372bb6826bd81d3542a419d6-12",
	}})
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}

	obj := f.remoteObject(t, "a.txt")
	if obj.ChecksumAlgo.String != AlgoEtagMD5Composite || obj.Checksum.String != "9e107d9d372bb6826bd81d3542a419d6-12" {
		t.Fatalf("remote object = %+v, want the composite etag recorded verbatim", obj)
	}

	rep, err := VerifyRemote(context.Background(), f.store, f.rcl, f.pair.Destination)
	if err != nil {
		t.Fatalf("VerifyRemote: %v", err)
	}
	if rep.Verified != 1 || !rep.Clean() {
		t.Fatalf("rep = %+v, want the composite etag verified by verbatim compare", rep)
	}
}

// TestCaptureS3ReadErrorStaysPending: a transient S3 read failure leaves
// every fingerprint pending with a warning and never fabricates one — the
// push still succeeds, since capture is off the durability critical path.
func TestCaptureS3ReadErrorStaysPending(t *testing.T) {
	f := setupCAFixture(t, s3DestBlock, "")
	installS3Reader(t, fakeS3Reader{err: errors.New("connection reset")})
	f.write(t, "a.txt", "alpha")
	f.index(t)

	rep, err := f.sync(t)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if rep.Status != store.RunStatusSuccess || rep.Fingerprints != 0 {
		t.Fatalf("rep = status=%q fingerprints=%d, want success with none recorded", rep.Status, rep.Fingerprints)
	}
	if !warnsContains(rep.Warnings, "checksums stay pending") {
		t.Fatalf("Warnings = %v, want a capture-failed advisory", rep.Warnings)
	}
	if obj := f.remoteObject(t, "a.txt"); obj.ChecksumAlgo.Valid || obj.Checksum.Valid {
		t.Fatalf("remote object = %+v, want a pending pair", obj)
	}
}

// TestCaptureS3AbsentObjectStaysPending: an object the S3 listing does not
// return yet stays pending with the distinct "not yet returned" advisory,
// separate from a hard read failure — the push still succeeds.
func TestCaptureS3AbsentObjectStaysPending(t *testing.T) {
	f := setupCAFixture(t, s3DestBlock, "")
	installS3Reader(t, fakeS3Reader{etags: map[string]string{}})
	f.write(t, "a.txt", "alpha")
	f.index(t)

	rep, err := f.sync(t)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if rep.Status != store.RunStatusSuccess || rep.Fingerprints != 0 {
		t.Fatalf("rep = status=%q fingerprints=%d, want success with none recorded", rep.Status, rep.Fingerprints)
	}
	if !warnsContains(rep.Warnings, "not yet returned by the remote listing") {
		t.Fatalf("Warnings = %v, want a not-yet-listed advisory", rep.Warnings)
	}
	if obj := f.remoteObject(t, "a.txt"); obj.ChecksumAlgo.Valid || obj.Checksum.Valid {
		t.Fatalf("remote object = %+v, want a pending pair", obj)
	}
}

// warnsContains reports whether any warning contains sub.
func warnsContains(warns []string, sub string) bool {
	for _, w := range warns {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// TestCaptureNoChecksumWarns: a backend exposing no checksum leaves the
// pair pending with a run-report warning — the push still succeeds.
func TestCaptureNoChecksumWarns(t *testing.T) {
	f := setupContentAddressedFixture(t)
	t.Setenv("RCLONE_FAKE_NO_HASHES", "1")
	f.write(t, "a.txt", "alpha")
	f.index(t)

	rep, err := f.sync(t)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if rep.Status != store.RunStatusSuccess || rep.Fingerprints != 0 {
		t.Fatalf("rep = status=%q fingerprints=%d, want success with none recorded", rep.Status, rep.Fingerprints)
	}
	var warned bool
	for _, w := range rep.Warnings {
		if strings.Contains(w, "fingerprint stays pending") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("Warnings = %v, want a fingerprint-pending advisory", rep.Warnings)
	}
	obj := f.remoteObject(t, "a.txt")
	if obj.ChecksumAlgo.Valid || obj.Checksum.Valid {
		t.Fatalf("remote object = %+v, want a pending pair", obj)
	}
}

// TestCaptureMissingFromListingStaysPending: an object the remote
// listing does not return yet leaves the pair pending with a distinct
// "not yet returned" advisory — separate from a returned-but-no-hash
// object — and the push still succeeds.
func TestCaptureMissingFromListingStaysPending(t *testing.T) {
	f := setupContentAddressedFixture(t)
	t.Setenv("RCLONE_FAKE_EMPTY_LISTING", "1")
	f.write(t, "a.txt", "alpha")
	f.index(t)

	rep, err := f.sync(t)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if rep.Status != store.RunStatusSuccess || rep.Fingerprints != 0 {
		t.Fatalf("rep = status=%q fingerprints=%d, want success with none recorded", rep.Status, rep.Fingerprints)
	}
	var warned bool
	for _, w := range rep.Warnings {
		if strings.Contains(w, "not yet returned by the remote listing") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("Warnings = %v, want a not-yet-listed advisory distinct from no-checksum", rep.Warnings)
	}
	obj := f.remoteObject(t, "a.txt")
	if obj.ChecksumAlgo.Valid || obj.Checksum.Valid {
		t.Fatalf("remote object = %+v, want a pending pair", obj)
	}
}

// TestCheckersCapInArgv: a destination's checkers cap reaches every
// rclone invocation the content-addressed push and the verify pass run
// against it.
func TestCheckersCapInArgv(t *testing.T) {
	f := setupCAFixture(t, `[destinations.offsite]
type     = "sftp"
host     = "remote.invalid"
user     = "u"
root     = "/data"
layout   = "content-addressed"
checkers = 3

[destinations.offsite.crypt]
password = "obscured-pw"
`, "/data")
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := VerifyRemote(context.Background(), f.store, f.rcl, f.pair.Destination); err != nil {
		t.Fatalf("VerifyRemote: %v", err)
	}

	for _, line := range f.shimLog(t) {
		if strings.Contains(line, " copyto ") || strings.Contains(line, " lsjson ") {
			if !strings.Contains(line, "--checkers 3") {
				t.Fatalf("argv lacks --checkers 3:\n%s", line)
			}
		}
	}
}

// remoteObject fetches the upload record for a path on the fixture's
// offsite destination.
func (f *caFixture) remoteObject(t *testing.T, path string) store.RemoteObject {
	t.Helper()
	row, err := f.store.GetByPath(context.Background(), f.volumeID(t), path)
	if err != nil {
		t.Fatalf("GetByPath %s: %v", path, err)
	}
	obj, err := f.store.GetRemoteObject(context.Background(), row.ContentID, "offsite")
	if err != nil {
		t.Fatalf("GetRemoteObject %s: %v", path, err)
	}
	return obj
}

// TestListedNameCryptSuffix pins the crypt data-suffix mapping between
// recorded overlay names and underlying-remote listings: names read from
// a crypt destination's underlying remote shed ".bin", filters passed to
// such listings gain it, and non-crypt destinations pass through
// untouched. Without this mapping every scan-back and verify listing on
// a crypt destination reports recorded artifacts missing and listed
// ones unrecorded.
func TestListedNameCryptSuffix(t *testing.T) {
	crypt := &config.Destination{Name: "c", Type: "s3", Crypt: &config.Crypt{Password: "x"}}
	plain := &config.Destination{Name: "p", Type: "s3"}
	if got := listedPlainName(crypt, "abc.bin"); got != "abc" {
		t.Errorf("crypt listed name = %q, want abc", got)
	}
	if got := listedRemoteName(crypt, "abc"); got != "abc.bin" {
		t.Errorf("crypt remote name = %q, want abc.bin", got)
	}
	if got := listedPlainName(plain, "abc.bin"); got != "abc.bin" {
		t.Errorf("plain listed name = %q, want abc.bin (untouched)", got)
	}
	if got := listedRemoteName(plain, "abc"); got != "abc" {
		t.Errorf("plain remote name = %q, want abc (untouched)", got)
	}
}

package sync

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
)

// fakeRcloneScript is the PATH-shim stand-in for the rclone binary,
// mirroring the kopia shim: it logs every argv line to
// $RCLONE_FAKE_LOG, then plays back the two subcommands the
// content-addressed push and the verify pass drive. Remote URIs
// (`remote:path`) map onto the local directory $RCLONE_FAKE_ROOT
// (after stripping the $RCLONE_FAKE_STRIP prefix, so overlay and
// underlying URIs land in the same tree); $RCLONE_FAKE_FAIL_GLOB
// injects per-destination copyto failures. lsjson hashes are derived
// from the file bytes via cksum, emitted under each requested
// --hash-type (or md5+sha1 when none is requested), with
// $RCLONE_FAKE_HASH_PREFIX simulating remote-side tampering,
// $RCLONE_FAKE_HASH_VALUE forcing an exact value, and
// $RCLONE_FAKE_NO_HASHES a backend that exposes no checksums;
// $RCLONE_FAKE_EMPTY_LISTING a directory lsjson that returns no entries.
const fakeRcloneScript = `#!/bin/sh
{
  printf 'argv:'
  for a in "$@"; do printf ' %s' "$a"; done
  printf '\n'
} >> "$RCLONE_FAKE_LOG"
if [ "$1" = "--config" ]; then shift 2; fi
cmd=$1; shift
resolve() {
  case "$1" in
  *:*)
    r="${1%%:*}"
    p="${1#*:}"
    case "$p" in
    "${RCLONE_FAKE_STRIP:-//none//}"/*) p="${p#"${RCLONE_FAKE_STRIP}"/}" ;;
    esac
    sfx=""
    case "$r" in
    *-crypt) sfx="$RCLONE_FAKE_CRYPT_SUFFIX" ;;
    esac
    printf '%s/%s%s' "$RCLONE_FAKE_ROOT" "$p" "$sfx" ;;
  *) printf '%s' "$1" ;;
  esac
}
hashtypes="" includes="" stat=0 a1="" a2=""
while [ $# -gt 0 ]; do
  case "$1" in
  --stat) stat=1 ;;
  --hash-type) shift; hashtypes="$hashtypes $1" ;;
  --include) shift; includes="$includes $1" ;;
  --checkers) shift ;;
  -R) ;;
  --*) ;;
  *) if [ -z "$a1" ]; then a1="$1"; else a2="$1"; fi ;;
  esac
  shift
done
hashes_json() {
  [ -n "$RCLONE_FAKE_NO_HASHES" ] && return
  v="$RCLONE_FAKE_HASH_VALUE"
  [ -z "$v" ] && v="${RCLONE_FAKE_HASH_PREFIX}$(cksum < "$1" | cut -d' ' -f1)"
  printf ',"Hashes":{'
  sep=""
  for t in ${hashtypes:-md5 sha1}; do
    printf '%s"%s":"%s"' "$sep" "$t" "$v"
    sep=","
  done
  printf '}'
}
entry_json() {
  size=$(wc -c < "$1" | tr -d '[:space:]')
  printf '{"Path":"%s","Name":"%s","Size":%s,"IsDir":false' "$(basename "$1")" "$(basename "$1")" "$size"
  hashes_json "$1"
  printf '}'
}
case "$cmd" in
copyto)
  case "$a2" in
  ${RCLONE_FAKE_FAIL_GLOB:-//none//}) echo "fake copyto failure for $a2" >&2; exit 1 ;;
  esac
  dst=$(resolve "$a2")
  mkdir -p "$(dirname "$dst")" && cp "$(resolve "$a1")" "$dst"
  ;;
lsjson)
  if [ "$stat" = 1 ]; then
    f=$(resolve "$a1")
    if [ ! -f "$f" ]; then echo "object not found: $a1" >&2; exit 3; fi
    entry_json "$f"; printf '\n'
  else
    dir=$(resolve "$a1")
    if [ ! -d "$dir" ]; then echo "directory not found: $a1" >&2; exit 3; fi
    if [ -n "$RCLONE_FAKE_EMPTY_LISTING" ]; then printf '[]\n'; exit 0; fi
    printf '['
    sep=""
    for f in "$dir"/*; do
      [ -f "$f" ] || continue
      name=$(basename "$f")
      if [ -n "$includes" ]; then
        m=0
        for inc in $includes; do [ "$name" = "$inc" ] && m=1; done
        [ "$m" = 1 ] || continue
      fi
      printf '%s' "$sep"; entry_json "$f"; sep=","
    done
    printf ']\n'
  fi
  ;;
lsf)
  # Directory listing for the layout guard's emptiness probe. Resolve the
  # base directory ignoring any crypt filename suffix — we only need to
  # know whether any file exists beneath the root — then list recursively.
  case "$a1" in
  *:*)
    p="${a1#*:}"
    case "$p" in
    "${RCLONE_FAKE_STRIP:-//none//}"/*) p="${p#"${RCLONE_FAKE_STRIP}"/}" ;;
    esac
    dir="$RCLONE_FAKE_ROOT/$p" ;;
  *) dir="$a1" ;;
  esac
  [ -d "$dir" ] && find "$dir" -type f
  exit 0
  ;;
*) echo "unexpected rclone subcommand: $cmd $*" >&2; exit 64 ;;
esac
`

// caFixture is the content-addressed analogue of syncFixture: a store,
// a fake-rclone wrapper, and one volume syncing to one crypt sftp
// destination with layout = "content-addressed". fakeRoot is the local
// directory the shim materialises the remote into.
type caFixture struct {
	store    *store.Store
	rcl      *Rclone
	cfg      *config.Config
	pair     Pair
	fakeRoot string
	logPath  string
}

func setupContentAddressedFixture(t *testing.T) *caFixture {
	t.Helper()
	f := setupCAFixture(t, `[destinations.offsite]
type   = "sftp"
host   = "remote.invalid"
user   = "u"
root   = "/data"
layout = "content-addressed"

[destinations.offsite.crypt]
password = "obscured-pw"
`, "/data")
	// Crypt reality: files written through the overlay carry rclone
	// crypt's data suffix on the underlying remote, which is where
	// scan-back and verify list from. The shim appends the suffix on
	// overlay writes so those listings see what real rclone would.
	t.Setenv("RCLONE_FAKE_CRYPT_SUFFIX", cryptDataSuffix)
	return f
}

// setupCAFixture is the destination-configurable body of
// setupContentAddressedFixture. destBlock declares the `offsite`
// destination; strip is the destination root the shim removes from
// underlying-remote URIs so they land in the same fake tree as the
// crypt overlay's root-relative paths.
func setupCAFixture(t *testing.T, destBlock, strip string) *caFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake rclone shim is a POSIX shell script")
	}
	dir := t.TempDir()
	binPath := filepath.Join(dir, "rclone")
	if err := os.WriteFile(binPath, []byte(fakeRcloneScript), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	fakeRoot := filepath.Join(dir, "remote")
	logPath := filepath.Join(dir, "calls.log")
	t.Setenv("RCLONE_FAKE_LOG", logPath)
	t.Setenv("RCLONE_FAKE_ROOT", fakeRoot)
	t.Setenv("RCLONE_FAKE_FAIL_GLOB", "")
	t.Setenv("RCLONE_FAKE_STRIP", strip)
	t.Setenv("RCLONE_FAKE_NO_HASHES", "")
	t.Setenv("RCLONE_FAKE_HASH_VALUE", "")
	t.Setenv("RCLONE_FAKE_HASH_PREFIX", "")
	t.Setenv("RCLONE_FAKE_CRYPT_SUFFIX", "")

	root := t.TempDir()
	volPath := filepath.Join(root, "src")
	docsPath := filepath.Join(root, "docs-src")
	for _, p := range []string{volPath, docsPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	s, err := store.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	cfgPath := filepath.Join(root, "config.toml")
	cfgBody := destBlock + `
[volumes.pics]
path    = "` + volPath + `"
sync_to = ["offsite"]

[volumes.docs]
path    = "` + docsPath + `"
sync_to = ["offsite"]
`
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	pairs, err := PairsFor(cfg, "pics", "")
	if err != nil {
		t.Fatalf("PairsFor: %v", err)
	}
	rcl := &Rclone{Binary: binPath, Config: filepath.Join(root, "rclone.conf")}
	if err := os.WriteFile(rcl.Config, []byte{}, 0o600); err != nil {
		t.Fatalf("seed rclone.conf: %v", err)
	}
	return &caFixture{store: s, rcl: rcl, cfg: cfg, pair: pairs[0], fakeRoot: fakeRoot, logPath: logPath}
}

func (f *caFixture) write(t *testing.T, name, content string) {
	t.Helper()
	p := filepath.Join(f.pair.Volume.Path, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *caFixture) mtimeNs(t *testing.T, name string) int64 {
	t.Helper()
	fi, err := os.Stat(filepath.Join(f.pair.Volume.Path, name))
	if err != nil {
		t.Fatalf("stat %s: %v", name, err)
	}
	return fi.ModTime().UnixNano()
}

func (f *caFixture) index(t *testing.T) {
	t.Helper()
	if _, err := index.Index(context.Background(), f.store, f.pair.Volume.Path, index.Options{Name: "pics"}); err != nil {
		t.Fatalf("index.Index: %v", err)
	}
}

func (f *caFixture) sync(t *testing.T) (Report, error) {
	t.Helper()
	return RunPair(context.Background(), f.store, Tools{Rclone: f.rcl}, f.pair, Options{})
}

func (f *caFixture) volumeID(t *testing.T) int64 {
	t.Helper()
	v, err := f.store.GetVolumeByName(context.Background(), "pics")
	if err != nil {
		t.Fatalf("GetVolumeByName: %v", err)
	}
	return v.ID
}

// remotePath maps a destination subpath to where the shim materialised
// it: objects/ lives at the root, manifest segments per volume.
func (f *caFixture) remotePath(parts ...string) string {
	return filepath.Join(append([]string{f.fakeRoot}, parts...)...)
}

// remoteBlob is remotePath for file artifacts written through the crypt
// overlay: when the fixture simulates crypt's data suffix, on-disk file
// names carry it. Directory paths keep using remotePath.
func (f *caFixture) remoteBlob(parts ...string) string {
	return f.remotePath(parts...) + os.Getenv("RCLONE_FAKE_CRYPT_SUFFIX")
}

func (f *caFixture) readSegment(t *testing.T, runID int64) []ManifestEntry {
	t.Helper()
	data, err := os.ReadFile(f.remoteBlob("pics", ManifestDirName, fmt.Sprintf("run-%d", runID)))
	if err != nil {
		t.Fatalf("read manifest segment: %v", err)
	}
	var entries []ManifestEntry
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var e ManifestEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parse manifest line %q: %v", line, err)
		}
		entries = append(entries, e)
	}
	return entries
}

func blake3Hex(content string) string {
	sum := blake3.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestContentAddressedPushHappyPath(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.write(t, "b.txt", "beta")
	f.index(t)

	rep, err := f.sync(t)
	if err != nil {
		t.Fatalf("sync: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success", rep.Status)
	}
	if rep.Verification.Verified() || rep.Verification.Method != VerifyMethodPresenceSize {
		t.Fatalf("Verification = %+v, want unverified %q (presence is weaker than a content check)", rep.Verification, VerifyMethodPresenceSize)
	}
	if rep.Verification.Files != 2 || rep.RcloneResult.Transferred != 2 || rep.RcloneResult.Checked != 0 {
		t.Fatalf("counts = files=%d transferred=%d checked=%d, want 2/2/0", rep.Verification.Files, rep.RcloneResult.Transferred, rep.RcloneResult.Checked)
	}
	if rep.RcloneResult.Bytes != int64(len("alpha")+len("beta")) {
		t.Fatalf("Bytes = %d, want %d", rep.RcloneResult.Bytes, len("alpha")+len("beta"))
	}

	for name, content := range map[string]string{"a.txt": "alpha", "b.txt": "beta"} {
		obj := f.remoteBlob(ObjectsDirName, blake3Hex(content))
		got, err := os.ReadFile(obj)
		if err != nil {
			t.Fatalf("object for %s missing at %s: %v", name, obj, err)
		}
		if string(got) != content {
			t.Fatalf("object for %s = %q, want %q", name, got, content)
		}
	}

	entries := f.readSegment(t, rep.RunID)
	if len(entries) != 2 || entries[0].Path != "a.txt" || entries[1].Path != "b.txt" {
		t.Fatalf("segment entries = %+v, want a.txt and b.txt", entries)
	}
	if entries[0].Blake3 != blake3Hex("alpha") || entries[0].Status != store.StatusPresent || entries[0].SizeBytes != 5 {
		t.Fatalf("segment entry = %+v, want present alpha", entries[0])
	}

	run, err := f.store.GetRun(context.Background(), rep.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunStatusSuccess || run.FileCount != 2 || !run.Shallow.Valid || !run.Shallow.Bool {
		t.Fatalf("run = %+v, want success file_count=2 shallow=true", run)
	}

	row, err := f.store.GetByPath(context.Background(), f.volumeID(t), "a.txt")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	obj, err := f.store.GetRemoteObject(context.Background(), row.ContentID, "offsite")
	if err != nil {
		t.Fatalf("GetRemoteObject: %v", err)
	}
	if obj.UploadedRunID != rep.RunID || obj.ChecksumAlgo.String != "sha256" || !obj.Checksum.Valid {
		t.Fatalf("remote object = %+v, want a sha256-fingerprinted record for run %d", obj, rep.RunID)
	}
	if !obj.VerifiedAtNs.Valid {
		t.Fatalf("scan-back capture did not stamp verification: %+v", obj)
	}
	if rep.Fingerprints != 2 {
		t.Fatalf("Fingerprints = %d, want 2", rep.Fingerprints)
	}

	vector, err := f.store.ListDestinationRunIDs(context.Background(), f.volumeID(t), "offsite")
	if err != nil {
		t.Fatalf("ListDestinationRunIDs: %v", err)
	}
	if len(vector) != 1 {
		t.Fatalf("vector = %+v, want one self component", vector)
	}
	self, err := f.store.GetSelfNode(context.Background())
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	if vector[0].OriginNodeID != self.ID || vector[0].OriginRunID == 0 {
		t.Fatalf("vector component = %+v, want self node at the introduction run", vector[0])
	}
	// The content-addressed advance records presence+size, not a
	// content-verified method: the offload gate holds this target out
	// until a verified fingerprint backs the object (#109).
	if vector[0].VerifyMethod != VerifyMethodPresenceSize {
		t.Fatalf("verify method = %q, want %q", vector[0].VerifyMethod, VerifyMethodPresenceSize)
	}
	if store.ContentVerifiedMethod(vector[0].VerifyMethod) {
		t.Fatalf("presence+size must not count as content-verified")
	}

	// Transfers and confirmations address the crypt overlay remote.
	log, err := os.ReadFile(f.logPath)
	if err != nil {
		t.Fatalf("read shim log: %v", err)
	}
	if !strings.Contains(string(log), "copyto") || !strings.Contains(string(log), "offsite-crypt:"+ObjectsDirName+"/") {
		t.Fatalf("shim log lacks crypt-addressed copyto lines:\n%s", log)
	}
}

// TestContentAddressedUploadOnce: a second run uploads only hashes the
// destination has no record of — a new path carrying already-recorded
// content transfers nothing and still lands in the manifest.
func TestContentAddressedUploadOnce(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	f.write(t, "copy.txt", "alpha")
	f.index(t)
	rep, err := f.sync(t)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if rep.RcloneResult.Transferred != 0 || rep.RcloneResult.Checked != 1 {
		t.Fatalf("transferred=%d checked=%d, want 0/1 (hash already recorded)", rep.RcloneResult.Transferred, rep.RcloneResult.Checked)
	}
	entries := f.readSegment(t, rep.RunID)
	if len(entries) != 1 || entries[0].Path != "copy.txt" || entries[0].Blake3 != blake3Hex("alpha") {
		t.Fatalf("segment = %+v, want one copy.txt line", entries)
	}
}

// TestContentAddressedManifestSegmentGolden pins the documented segment
// format byte-for-byte across an add + supersede + missing delta.
func TestContentAddressedManifestSegmentGolden(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "v1")
	f.write(t, "c.txt", "cc")
	aOldMtime := f.mtimeNs(t, "a.txt")
	cMtime := f.mtimeNs(t, "c.txt")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	f.write(t, "a.txt", "v2-longer")
	if err := os.Remove(filepath.Join(f.pair.Volume.Path, "c.txt")); err != nil {
		t.Fatal(err)
	}
	f.write(t, "d.txt", "dd")
	f.index(t)
	rep, err := f.sync(t)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}

	line := func(path, content, status string, mtime int64) string {
		return fmt.Sprintf(`{"path":%q,"blake3":%q,"status":%q,"size_bytes":%d,"mtime_ns":%d}`,
			path, blake3Hex(content), status, len(content), mtime)
	}
	want := strings.Join([]string{
		line("a.txt", "v2-longer", store.StatusPresent, f.mtimeNs(t, "a.txt")),
		line("a.txt", "v1", store.StatusSuperseded, aOldMtime),
		line("c.txt", "cc", store.StatusMissing, cMtime),
		line("d.txt", "dd", store.StatusPresent, f.mtimeNs(t, "d.txt")),
	}, "\n") + "\n"

	got, err := os.ReadFile(f.remoteBlob("pics", ManifestDirName, fmt.Sprintf("run-%d", rep.RunID)))
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	if string(got) != want {
		t.Fatalf("segment = \n%s\nwant\n%s", got, want)
	}
}

// TestContentAddressedObjectFailureIsTransactional: when an object
// fails to land, the manifest segment is never written, the runs row is
// failed, no upload is recorded, and the durability vector stays put.
func TestContentAddressedObjectFailureIsTransactional(t *testing.T) {
	f := setupContentAddressedFixture(t)
	t.Setenv("RCLONE_FAKE_FAIL_GLOB", "*"+ObjectsDirName+"*")
	f.write(t, "a.txt", "alpha")
	f.index(t)

	rep, err := f.sync(t)
	if err == nil || !strings.Contains(err.Error(), "did not advance") {
		t.Fatalf("expected transactional-landing failure, got %v", err)
	}
	if rep.Status != store.RunStatusFailed {
		t.Fatalf("Status = %q, want failed", rep.Status)
	}
	if _, statErr := os.Stat(f.remoteBlob("pics", ManifestDirName, fmt.Sprintf("run-%d", rep.RunID))); statErr == nil {
		t.Fatalf("manifest segment written despite object failure")
	}
	run, err := f.store.GetRun(context.Background(), rep.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunStatusFailed || !run.Error.Valid {
		t.Fatalf("run = %+v, want failed with an error message", run)
	}
	row, err := f.store.GetByPath(context.Background(), f.volumeID(t), "a.txt")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if has, _ := f.store.HasRemoteObject(context.Background(), row.ContentID, "offsite"); has {
		t.Fatalf("failed upload was recorded")
	}
	vector, err := f.store.ListDestinationRunIDs(context.Background(), f.volumeID(t), "offsite")
	if err != nil || len(vector) != 0 {
		t.Fatalf("vector = %+v (err=%v), want empty", vector, err)
	}
}

// TestContentAddressedSegmentFailureThenRecovery: a failed segment
// upload keeps the vector put even though every object landed, and the
// retry transfers nothing — the recorded objects are skipped and only
// the segment's missing piece is re-pushed.
func TestContentAddressedSegmentFailureThenRecovery(t *testing.T) {
	f := setupContentAddressedFixture(t)
	t.Setenv("RCLONE_FAKE_FAIL_GLOB", "*pics/"+ManifestDirName+"/*")
	f.write(t, "a.txt", "alpha")
	f.index(t)

	rep, err := f.sync(t)
	if err == nil || !strings.Contains(err.Error(), "manifest segment") {
		t.Fatalf("expected segment-upload failure, got %v", err)
	}
	if rep.Status != store.RunStatusFailed {
		t.Fatalf("Status = %q, want failed", rep.Status)
	}
	if _, err := os.Stat(f.remoteBlob(ObjectsDirName, blake3Hex("alpha"))); err != nil {
		t.Fatalf("object should have landed before the segment failed: %v", err)
	}
	vector, err := f.store.ListDestinationRunIDs(context.Background(), f.volumeID(t), "offsite")
	if err != nil || len(vector) != 0 {
		t.Fatalf("vector = %+v (err=%v), want empty after segment failure", vector, err)
	}

	t.Setenv("RCLONE_FAKE_FAIL_GLOB", "")
	rep2, err := f.sync(t)
	if err != nil {
		t.Fatalf("retry sync: %v", err)
	}
	if rep2.RcloneResult.Transferred != 0 || rep2.RcloneResult.Checked != 1 {
		t.Fatalf("retry transferred=%d checked=%d, want 0/1 (object already recorded)", rep2.RcloneResult.Transferred, rep2.RcloneResult.Checked)
	}
	entries := f.readSegment(t, rep2.RunID)
	if len(entries) != 1 || entries[0].Path != "a.txt" {
		t.Fatalf("retry segment = %+v, want the a.txt line", entries)
	}
	vector, err = f.store.ListDestinationRunIDs(context.Background(), f.volumeID(t), "offsite")
	if err != nil || len(vector) != 1 {
		t.Fatalf("vector = %+v (err=%v), want one component after recovery", vector, err)
	}
}

// TestContentAddressedEmptyDeltaStillLandsSegment: an unchanged volume
// produces an empty segment — uploaded anyway so the run leaves the
// landing evidence the next watermark check looks for.
func TestContentAddressedEmptyDeltaStillLandsSegment(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	rep, err := f.sync(t)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if rep.Status != store.RunStatusSuccess || rep.Verification.Files != 0 {
		t.Fatalf("rep = status=%q files=%d, want success with an empty delta", rep.Status, rep.Verification.Files)
	}
	data, err := os.ReadFile(f.remoteBlob("pics", ManifestDirName, fmt.Sprintf("run-%d", rep.RunID)))
	if err != nil {
		t.Fatalf("empty segment missing: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("empty delta produced segment content: %q", data)
	}
}

// TestContentAddressedReservedDirsStayHome: indexed rows under the
// reserved sync subtrees never reach the destination — no object, no
// manifest line.
func TestContentAddressedReservedDirsStayHome(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.write(t, HistoryDirName+"/secret.txt", "do-not-upload")
	f.index(t)

	rep, err := f.sync(t)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	entries := f.readSegment(t, rep.RunID)
	if len(entries) != 1 || entries[0].Path != "a.txt" {
		t.Fatalf("segment = %+v, want only a.txt", entries)
	}
	if _, err := os.Stat(f.remoteBlob(ObjectsDirName, blake3Hex("do-not-upload"))); err == nil {
		t.Fatalf("reserved-subtree content was uploaded as an object")
	}
}

// TestContentAddressedWatermarkGuard: a destination whose recorded
// last success left no manifest segment (a mirror-era run) is refused
// rather than silently diffed against the wrong baseline.
func TestContentAddressedWatermarkGuard(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)

	ctx := context.Background()
	mirrorRun, err := f.store.BeginRun(ctx, store.RunKindSync, f.volumeID(t), "offsite", false)
	if err != nil {
		t.Fatalf("seed mirror-era run: %v", err)
	}
	if err := f.store.FinishRun(ctx, mirrorRun, store.RunStatusSuccess, "", 1); err != nil {
		t.Fatalf("finish mirror-era run: %v", err)
	}
	// A real mirror era leaves the volume's files mirrored under the root,
	// so the root is non-empty: a genuine layout conflict, distinct from a
	// wiped destination (which the fresh-start recognition below handles).
	seedRemoteFile(t, f, "pics", "a.txt")

	rep, err := f.sync(t)
	if err == nil || !strings.Contains(err.Error(), "does not look content-addressed") {
		t.Fatalf("expected layout-flip refusal, got %v", err)
	}
	if rep.Status != store.RunStatusFailed {
		t.Fatalf("Status = %q, want failed", rep.Status)
	}
}

// seedRemoteFile writes one file under the fixture's remote root, standing
// in for the bytes a prior (mirror) era left there so the root is non-empty.
func seedRemoteFile(t *testing.T, f *caFixture, parts ...string) {
	t.Helper()
	p := f.remotePath(parts...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir remote file dir: %v", err)
	}
	if err := os.WriteFile(p, []byte("mirror-era-bytes"), 0o644); err != nil {
		t.Fatalf("seed remote file: %v", err)
	}
}

// TestContentAddressedFreshStartOnEmptyRoot is the F20 fix: when name-keyed
// run history records a prior success but the configured root is empty on the
// remote (a wiped or repointed destination, or one cleared by
// `squirrel destination reset`), the guard treats it as a fresh start and
// re-uploads instead of refusing.
func TestContentAddressedFreshStartOnEmptyRoot(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)

	ctx := context.Background()
	staleRun, err := f.store.BeginRun(ctx, store.RunKindSync, f.volumeID(t), "offsite", false)
	if err != nil {
		t.Fatalf("seed stale run: %v", err)
	}
	if err := f.store.FinishRun(ctx, staleRun, store.RunStatusSuccess, "", 1); err != nil {
		t.Fatalf("finish stale run: %v", err)
	}
	// Remote root left empty — no seedRemoteFile — so the guard reads it as
	// a fresh start.

	rep, err := f.sync(t)
	if err != nil {
		t.Fatalf("fresh-start sync failed: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success", rep.Status)
	}
	if _, err := os.Stat(f.remoteBlob(ObjectsDirName, blake3Hex("alpha"))); err != nil {
		t.Fatalf("fresh start did not re-upload the object: %v", err)
	}
}

func TestContentAddressedDryRunRefused(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)

	_, err := RunPair(context.Background(), f.store, Tools{Rclone: f.rcl}, f.pair, Options{DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "dry-run") {
		t.Fatalf("expected dry-run refusal, got %v", err)
	}
	runs, _ := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	for _, r := range runs {
		if r.Kind == store.RunKindSync {
			t.Fatalf("dry-run wrote a sync runs row: %+v", r)
		}
	}
}

func TestRestoreRefusesContentAddressedDestination(t *testing.T) {
	f := setupContentAddressedFixture(t)
	_, err := Restore(context.Background(), f.store, f.rcl, f.pair.Volume, f.pair.Destination, RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "content-addressed") {
		t.Fatalf("expected content-addressed restore refusal, got %v", err)
	}
}

// TestContentAddressedCrossVolumeDedup: objects/ is destination-global,
// matching remote_objects' (content, destination) key — a second volume
// carrying already-recorded content uploads nothing, and its manifest
// still maps the path onto the shared object.
func TestContentAddressedCrossVolumeDedup(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "shared-bytes")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("pics sync: %v", err)
	}

	docs := f.cfg.Volumes["docs"]
	if err := os.WriteFile(filepath.Join(docs.Path, "report.txt"), []byte("shared-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Index(context.Background(), f.store, docs.Path, index.Options{Name: "docs"}); err != nil {
		t.Fatalf("index docs: %v", err)
	}
	docsPairs, err := PairsFor(f.cfg, "docs", "")
	if err != nil {
		t.Fatalf("PairsFor docs: %v", err)
	}
	rep, err := RunPair(context.Background(), f.store, Tools{Rclone: f.rcl}, docsPairs[0], Options{})
	if err != nil {
		t.Fatalf("docs sync: %v", err)
	}
	if rep.RcloneResult.Transferred != 0 || rep.RcloneResult.Checked != 1 {
		t.Fatalf("docs transferred=%d checked=%d, want 0/1 (object shared across volumes)", rep.RcloneResult.Transferred, rep.RcloneResult.Checked)
	}
	data, err := os.ReadFile(f.remoteBlob("docs", ManifestDirName, fmt.Sprintf("run-%d", rep.RunID)))
	if err != nil {
		t.Fatalf("docs segment missing: %v", err)
	}
	if !strings.Contains(string(data), blake3Hex("shared-bytes")) || !strings.Contains(string(data), "report.txt") {
		t.Fatalf("docs segment = %q, want report.txt mapped onto the shared object", data)
	}
	if _, err := os.Stat(f.remoteBlob(ObjectsDirName, blake3Hex("shared-bytes"))); err != nil {
		t.Fatalf("shared object missing at the destination root: %v", err)
	}
}

// TestContentAddressedDriftRefusesObject: a source file whose on-disk
// bytes drift from the indexed hash (here a same-length, mtime-preserving
// in-place edit the metadata stat alone cannot catch) is never recorded in
// remote_objects. The run is refused and warned, the watermark holds, and
// once the honest bytes are restored the next run lands and records the
// object normally — the drifted bytes never bound to the hash.
func TestContentAddressedDriftRefusesObject(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)

	indexedMtime := f.mtimeNs(t, "a.txt")
	src := filepath.Join(f.pair.Volume.Path, "a.txt")
	if err := os.WriteFile(src, []byte("ALPHA"), 0o644); err != nil {
		t.Fatalf("in-place edit: %v", err)
	}
	restoreMtime := time.Unix(0, indexedMtime)
	if err := os.Chtimes(src, restoreMtime, restoreMtime); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}

	rep, err := f.sync(t)
	if err == nil || !strings.Contains(err.Error(), "drifting from their indexed hash") {
		t.Fatalf("expected a drift refusal, got %v", err)
	}
	if rep.Status != store.RunStatusFailed {
		t.Fatalf("Status = %q, want failed", rep.Status)
	}
	if rep.RcloneResult.Transferred != 0 || rep.RcloneResult.Errors != 0 {
		t.Fatalf("counts = transferred=%d errors=%d, want 0/0 (drift is a refusal, not a transfer error)",
			rep.RcloneResult.Transferred, rep.RcloneResult.Errors)
	}
	if len(rep.Warnings) == 0 || !strings.Contains(strings.Join(rep.Warnings, "\n"), "drifted") {
		t.Fatalf("Warnings = %+v, want a drift advisory", rep.Warnings)
	}
	if _, statErr := os.Stat(f.remoteBlob(ObjectsDirName, blake3Hex("alpha"))); statErr == nil {
		t.Fatalf("drifted source uploaded an object under the indexed hash")
	}

	row, err := f.store.GetByPath(context.Background(), f.volumeID(t), "a.txt")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if has, _ := f.store.HasRemoteObject(context.Background(), row.ContentID, "offsite"); has {
		t.Fatalf("drifted object was recorded in remote_objects")
	}
	if _, statErr := os.Stat(f.remoteBlob("pics", ManifestDirName, fmt.Sprintf("run-%d", rep.RunID))); statErr == nil {
		t.Fatalf("manifest segment written despite a refused object")
	}
	if vector, err := f.store.ListDestinationRunIDs(context.Background(), f.volumeID(t), "offsite"); err != nil || len(vector) != 0 {
		t.Fatalf("vector = %+v (err=%v), want empty after a refused object", vector, err)
	}

	if err := os.WriteFile(src, []byte("alpha"), 0o644); err != nil {
		t.Fatalf("restore honest bytes: %v", err)
	}
	if err := os.Chtimes(src, restoreMtime, restoreMtime); err != nil {
		t.Fatalf("restore mtime again: %v", err)
	}
	rep2, err := f.sync(t)
	if err != nil {
		t.Fatalf("retry sync: %v", err)
	}
	if rep2.RcloneResult.Transferred != 1 {
		t.Fatalf("retry transferred = %d, want 1 (honest bytes upload after drift cleared)", rep2.RcloneResult.Transferred)
	}
	if has, _ := f.store.HasRemoteObject(context.Background(), row.ContentID, "offsite"); !has {
		t.Fatalf("honest re-upload was not recorded")
	}
	got, err := os.ReadFile(f.remoteBlob(ObjectsDirName, blake3Hex("alpha")))
	if err != nil {
		t.Fatalf("object missing after honest re-upload: %v", err)
	}
	if string(got) != "alpha" {
		t.Fatalf("recorded object = %q, want the honest bytes %q", got, "alpha")
	}
}

// TestContentAddressedRefusesVolumeNamedObjects: a volume named like
// the destination-root objects/ directory would collide with it.
func TestContentAddressedRefusesVolumeNamedObjects(t *testing.T) {
	f := setupContentAddressedFixture(t)
	pair := Pair{Volume: &config.Volume{Name: ObjectsDirName, Path: t.TempDir()}, Destination: f.pair.Destination}
	_, err := RunPair(context.Background(), f.store, Tools{Rclone: f.rcl}, pair, Options{})
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("expected volume-name collision refusal, got %v", err)
	}
}

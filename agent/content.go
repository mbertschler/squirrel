package agent

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/syncproto"
)

// receivedFileMode is the permission applied to a file materialised from
// an upload. The wire carries (path, blake3, size, mtime) and no mode, so
// the receiver picks the conventional non-executable default; the next
// `squirrel index` records whatever is on disk either way.
const receivedFileMode = 0o644

// handlePutContent implements PUT /v1/sync/content/{run}/{blake3} — the
// transfer phase of a v4 session. The body is one content object's bytes;
// the receiver streams them to a temp file while hashing, refuses the
// upload unless the digest matches the {blake3} it was addressed to, and
// only then materialises it at every path in the session that wants that
// content.
//
// The URL carries a digest and no path: {blake3} is validated against a
// fixed 64-character hex alphabet before anything touches the filesystem,
// and the destination paths are the session's own /plan verdicts, each
// checked by validateRelPath when /plan accepted it.
func (r *peerSyncRouter) handlePutContent(w http.ResponseWriter, req *http.Request) {
	runID, digest, err := parseContentRoute(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sess, ok, err := r.enterUpload(runID, callerNodeName(req))
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "no session for receiver_run_id")
		return
	}
	defer sess.uploads.Done()
	targets, size := pathsAwaitingContent(sess, digest)
	if len(targets) == 0 {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("no path in this session awaits content %s", hex.EncodeToString(digest)))
		return
	}
	body := http.MaxBytesReader(w, req.Body, size)
	if err := r.materializeContent(sess, digest, targets, size, body); err != nil {
		writeError(w, contentErrorStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, syncproto.ContentResponse{Paths: targets})
}

// parseContentRoute extracts and validates the two route wildcards. The
// digest is required to be exactly 64 lowercase hex characters — the
// canonical form every other squirrel surface writes — so a caller
// cannot smuggle a path separator, a traversal segment, or an
// uppercase-variant duplicate through the one peer-supplied component of
// the URL.
func parseContentRoute(req *http.Request) (runID int64, digest []byte, err error) {
	runID, err = strconv.ParseInt(req.PathValue("run"), 10, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("invalid receiver run id in path: %w", err)
	}
	hexDigest := req.PathValue("blake3")
	if len(hexDigest) != 64 {
		return 0, nil, fmt.Errorf("blake3 must be 64 hex characters, got %d", len(hexDigest))
	}
	digest, err = hex.DecodeString(hexDigest)
	if err != nil {
		return 0, nil, fmt.Errorf("blake3 is not hex: %w", err)
	}
	if hex.EncodeToString(digest) != hexDigest {
		return 0, nil, errors.New("blake3 must be lowercase hex")
	}
	return runID, digest, nil
}

// pathsAwaitingContent returns every volume-relative path in the session
// whose /plan verdict expects these exact bytes, sorted so a multi-path
// upload materialises in a deterministic order, along with their size —
// one size, since /plan refuses a digest declared at two sizes.
func pathsAwaitingContent(sess *peerSession, digest []byte) (paths []string, size int64) {
	for path, entry := range sess.dispositions {
		if materializesAtPath(entry.disposition) && bytesEqual(entry.blake3, digest) {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil, 0
	}
	slices.Sort(paths)
	return paths, sess.dispositions[paths[0]].size
}

// enterUpload resolves the session for an upload and counts the upload
// in flight under the same lock takeSession removes the session with, so
// /close either sees the upload and waits for it, or the upload finds no
// session. The caller must call sess.uploads.Done.
func (r *peerSyncRouter) enterUpload(receiverRunID int64, callerNode string) (sess *peerSession, ok bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok = r.sessions[receiverRunID]
	if !ok {
		return nil, false, nil
	}
	if callerNode != "" && callerNode != sess.initiatorNodeName {
		return nil, true, errSessionCallerMismatch
	}
	sess.uploads.Add(1)
	return sess, true, nil
}

func (sess *peerSession) markLanded(rel string) {
	sess.landedMu.Lock()
	defer sess.landedMu.Unlock()
	if sess.landed == nil {
		sess.landed = make(map[string]struct{})
	}
	sess.landed[rel] = struct{}{}
}

// commits reports whether /close records a live row for path. A failed
// close comes from an initiator that gave up mid-flight and cannot say
// what landed, so it commits only the paths an upload materialised with
// verified bytes; any other close commits every path expecting bytes and
// relies on failedPaths for the ones that did not verify.
func (sess *peerSession) commits(path string, entry *sessionEntry, status string) bool {
	if !materializesAtPath(entry.disposition) {
		return false
	}
	if status != store.RunStatusFailed {
		return true
	}
	sess.landedMu.Lock()
	defer sess.landedMu.Unlock()
	_, ok := sess.landed[path]
	return ok
}

// errRejected marks a failure the *caller's bytes* caused — a body that is
// the wrong length or hashes to the wrong digest. Everything else that can
// fail here is the receiver's own filesystem (no space, a permission, a
// failed fsync), and telling a peer its request was bad when the receiver's
// disk is full sends the operator looking on the wrong machine.
var errRejected = errors.New("upload rejected")

// contentErrorStatus maps a materialise failure onto the status that names
// which side has to act: 400 for bytes the receiver refused, 500 for a
// receiver-side failure the initiator can only retry.
func contentErrorStatus(err error) int {
	if errors.Is(err, errRejected) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// materializeContent lands the streamed body at the first target and
// copies it to any remaining ones. The stream is written once and hashed
// in the same pass, so the digest covers exactly the bytes that reached
// the disk rather than a later re-read of them.
func (r *peerSyncRouter) materializeContent(sess *peerSession, digest []byte, targets []string, size int64, body io.Reader) error {
	primary := filepath.Join(sess.volume.Path, targets[0])
	if err := streamToPath(primary, body, digest, size, sess.dispositions[targets[0]].mtimeNs); err != nil {
		return fmt.Errorf("receive %s: %w", targets[0], err)
	}
	sess.markLanded(targets[0])
	for _, rel := range targets[1:] {
		dst := filepath.Join(sess.volume.Path, rel)
		if err := copyFileToPath(primary, dst, sess.dispositions[rel].mtimeNs); err != nil {
			return fmt.Errorf("materialise %s from %s: %w", rel, targets[0], err)
		}
		sess.markLanded(rel)
	}
	return nil
}

// streamToPath writes body to a temp file beside dstAbs, verifies the
// bytes hash to want and are exactly size long, and only then renames the
// temp into place. A body that is short, long, or hashes differently
// leaves the destination untouched — the refuse-over-wrong-write
// invariant, applied to the one place a peer's bytes enter the volume.
//
// The durability discipline (chmod and fsync on the open fd, mtime after
// close, rename last) mirrors copyFileToPath, for the same reasons
// documented there.
func streamToPath(dstAbs string, body io.Reader, want []byte, size, mtimeNs int64) error {
	if err := os.MkdirAll(filepath.Dir(dstAbs), 0o755); err != nil {
		return fmt.Errorf("mkdir dest dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dstAbs), ".squirrel-incoming-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	hasher := blake3.New()
	written, err := io.Copy(io.MultiWriter(tmp, hasher), body)
	if err != nil {
		_ = tmp.Close()
		cleanup()
		var overrun *http.MaxBytesError
		if errors.As(err, &overrun) {
			return fmt.Errorf("%w: body exceeds the %d bytes plan declared", errRejected, size)
		}
		return fmt.Errorf("write incoming bytes: %w", err)
	}
	if err := verifyReceived(hasher.Sum(nil), want, written, size); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := finalizeTemp(tmp, tmpPath, dstAbs, mtimeNs); err != nil {
		cleanup()
		return err
	}
	return nil
}

// verifyReceived is the accept/refuse decision for one upload: the bytes
// must be the declared length and must hash to the digest the URL
// addressed them to.
func verifyReceived(got, want []byte, written, size int64) error {
	if written != size {
		return fmt.Errorf("%w: body is %d bytes, plan declared %d", errRejected, written, size)
	}
	if !bytesEqual(got, want) {
		return fmt.Errorf("%w: body hashes to %s, addressed as %s", errRejected,
			hex.EncodeToString(got), hex.EncodeToString(want))
	}
	return nil
}

// finalizeTemp applies mode, flushes, stamps mtime, and renames the temp
// file over its destination. It takes ownership of closing tmp; the
// caller removes tmpPath on any returned error.
func finalizeTemp(tmp *os.File, tmpPath, dstAbs string, mtimeNs int64) error {
	if err := tmp.Chmod(receivedFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if mtimeNs != 0 {
		t := time.Unix(0, mtimeNs)
		if err := os.Chtimes(tmpPath, t, t); err != nil {
			return fmt.Errorf("set mtime: %w", err)
		}
	}
	if err := os.Rename(tmpPath, dstAbs); err != nil {
		return fmt.Errorf("rename temp: %w", err)
	}
	return nil
}

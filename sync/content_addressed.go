package sync

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// Content-addressed destination layout: an append-only store of content
// objects plus the manifest segments that map paths onto them. The
// layout has no mirrored user tree — every byte under the destination
// root is squirrel-written.
const (
	// ObjectsDirName holds one immutable object per BLAKE3 content
	// hash at the destination root: objects/<lowercase hex>, raw file
	// bytes (encrypted by the crypt overlay when the destination has
	// one). The directory is destination-global — shared by every
	// volume, matching remote_objects' (content, destination) key —
	// so duplicated content across volumes uploads once. An object is
	// uploaded once and never moved, overwritten, or deleted.
	ObjectsDirName = "objects"
	// ManifestDirName holds one immutable manifest segment per sync
	// run, per volume: <volume>/index/run-<run id>, the JSONL
	// path-level delta of that run (see ManifestEntry). Replaying a
	// volume's segments in run-id order reconstructs its full
	// path→content mapping with no SQLite required. Distinct from
	// IndexDirName, the dot-directory the snapshot ride-along writes.
	ManifestDirName = "index"
)

// ManifestEntry is one line of a manifest segment: a single path-level
// state change, JSON-encoded with exactly these fields in this order,
// one object per line (JSONL), lines sorted by (path, status).
//
//	{"path":"2024/cat.jpg","blake3":"<64 hex chars>","status":"present","size_bytes":123,"mtime_ns":456}
//
// status is one of present, superseded, missing, offloaded. To replay a
// segment log: process segments in ascending run id; for each line with
// status present, missing, or offloaded, set the path's current
// (content, status) — the bytes for a present or offloaded path live at
// objects/<blake3>; a missing path's content is known but was lost at
// the origin. Lines with status superseded are history only (the
// outgoing content of a path that changed) and update no mapping. The
// format is stable so a small external script can recover data from the
// destination without squirrel.
type ManifestEntry struct {
	Path      string `json:"path"`
	Blake3    string `json:"blake3"`
	Status    string `json:"status"`
	SizeBytes int64  `json:"size_bytes"`
	MtimeNs   int64  `json:"mtime_ns"`
}

// encodeManifestSegment renders the delta as JSONL. The input order
// (path, status — as ListPathDeltaSince returns it) is preserved, so
// identical deltas encode byte-for-byte identically. An empty delta
// encodes to an empty segment; the segment still uploads so every
// successful run leaves its landing evidence.
func encodeManifestSegment(delta []store.PathDelta) ([]byte, error) {
	var out []byte
	for _, d := range delta {
		line, err := json.Marshal(ManifestEntry{
			Path:      d.Path,
			Blake3:    hex.EncodeToString(d.Blake3),
			Status:    d.Status,
			SizeBytes: d.SizeBytes,
			MtimeNs:   d.MtimeNs,
		})
		if err != nil {
			return nil, fmt.Errorf("encode manifest entry for %s: %w", d.Path, err)
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out, nil
}

// contentPusher drives the object and manifest-segment transfers that
// the content-addressed and packed layouts share. Both address content
// objects at objects/<hash> on the destination-global root, write one
// per-run manifest segment under <volume>/index/run-<id>, and fingerprint
// freshly landed objects the same way. The content-addressed layout uses
// this base directly; the packed layout embeds it and adds pack assembly
// for content below its threshold.
type contentPusher struct {
	store *store.Store
	rcl   *Rclone
	vol   *config.Volume
	dest  *config.Destination
}

// ensureMarker gates a remote content-layout push on the destination's
// per-volume .squirrel-volume marker, exactly as the mirror layout does
// (the marker sits at the volume root regardless of layout). Local
// content-addressed and packed destinations are intentionally left
// ungated here: they carry no such gate today, so extending it to them
// is a separate parity concern — this closes only the remote gap (#150).
func (h *contentPusher) ensureMarker(ctx context.Context, init bool) error {
	if h.dest.Type == "local" {
		return nil
	}
	return ensureRemoteDestinationMarker(ctx, h.store, h.rcl, h.dest, h.vol.Name, init)
}

// contentAddressedHandler pushes a volume to a content-addressed rclone
// destination: per-hash `rclone copyto` for each content object the
// destination lacks, then the run's manifest segment. The landing is
// transactional from the durability gate's point of view — the runs row
// reaches success and the destination vector advances only once both
// the objects and the segment are confirmed present at the expected
// size; any earlier failure leaves orphaned objects that are recorded
// (or re-uploaded idempotently) and harmless without a segment mapping
// them.
type contentAddressedHandler struct {
	contentPusher
}

func (h *contentAddressedHandler) TargetName() string { return h.dest.Name }

func (h *contentAddressedHandler) Push(ctx context.Context, opts Options) (Report, error) {
	rep := Report{Volume: h.vol.Name, Destination: h.dest.Name}
	// Stamped up front so output renderers key content-addressed
	// formatting off the method even when the push fails early.
	rep.Verification.Method = VerifyMethodPresenceSize
	if h.vol.Name == ObjectsDirName {
		return rep, fmt.Errorf("volume %q: the name collides with the destination-root %s/ directory of content-addressed destination %q — rename the volume or use a mirrored destination", h.vol.Name, ObjectsDirName, h.dest.Name)
	}
	volID, err := requireIndexedVolume(ctx, h.store, h.vol)
	if err != nil {
		return rep, err
	}
	if opts.DryRun {
		return rep, h.previewDryRun(ctx, &rep, volID)
	}
	if err := h.ensureMarker(ctx, opts.Init); err != nil {
		return rep, err
	}
	// shallow=true on the runs row: the per-object transfers carry no
	// BLAKE3 end-to-end check (crypt remotes expose no hashes), and the
	// audit trail stays honest about that.
	runID, err := beginSyncRunGuarded(ctx, h.store, false, store.SyncRunSpec{
		VolumeID:    volID,
		Destination: h.dest.Name,
		Shallow:     true,
	}, h.vol.Name)
	if err != nil {
		return rep, err
	}
	rep.RunID = runID
	if opts.OnRunID != nil {
		opts.OnRunID(runID)
	}

	err = h.push(ctx, &rep, volID, runID)
	finishHandlerRun(ctx, h.store, &rep, err)
	opts.Snapshot.afterSync(ctx, &rep, h.vol, h.dest)
	return rep, err
}

func (h *contentAddressedHandler) sealed() {}

// previewDryRun reports what a real push would upload without transferring
// anything or writing a runs row: rep.RunID stays 0, no objects land, and
// no remote_objects or manifest-segment rows are written. It computes the
// same delta and new-content selection the real push does (watermark →
// delta → plannedUploads), so the preview matches what the next real run
// would land. The watermark check still reads the destination, so a
// layout-flip is refused here too — a dry-run is an honest rehearsal.
func (h *contentAddressedHandler) previewDryRun(ctx context.Context, rep *Report, volID int64) error {
	watermark, err := h.watermark(ctx, volID)
	if err != nil {
		return err
	}
	delta, err := h.store.ListPathDeltaSince(ctx, volID, watermark)
	if err != nil {
		return fmt.Errorf("compute path delta since run %d: %w", watermark, err)
	}
	rep.Verification.Files = int64(len(delta))
	if err := h.previewObjectUploads(ctx, rep, plannedUploads(delta)); err != nil {
		return err
	}
	rep.Status = store.RunStatusSuccess
	return nil
}

// push runs the transactional landing: delta → objects → segment →
// vector. rep.Status starts failed and is promoted to success only at
// the end, after the destination vector advanced — a confirmed landing
// whose evidence failed to record must not present as success, or the
// next run's watermark would skip past it.
func (h *contentAddressedHandler) push(ctx context.Context, rep *Report, volID, runID int64) error {
	rep.Status = store.RunStatusFailed
	watermark, err := h.watermark(ctx, volID)
	if err != nil {
		return err
	}
	advance, err := captureDurabilityAdvance(ctx, h.store, volID)
	if err != nil {
		return err
	}
	delta, err := h.store.ListPathDeltaSince(ctx, volID, watermark)
	if err != nil {
		return fmt.Errorf("compute path delta since run %d: %w", watermark, err)
	}
	rep.Verification.Files = int64(len(delta))
	if err := h.uploadObjects(ctx, rep, runID, plannedUploads(delta)); err != nil {
		return err
	}
	if err := h.uploadSegment(ctx, delta, runID); err != nil {
		return err
	}
	// presence+size is not a content-verified method (crypt remotes expose
	// no hashes): the component advances so the offload gate's per-object
	// scan-back can back it locally. When this run's capture leaves the
	// whole (volume, destination) pair with a verified fingerprint behind
	// every present content, the advance is instead stamped
	// fingerprint-verified — the content-verified method that also relays
	// over the durability pull (see the durability-vector upgrade in
	// store). A single still-pending object holds it at presence+size.
	method, err := h.advanceMethod(ctx, volID, len(advance))
	if err != nil {
		return err
	}
	if err := h.store.AdvanceDestinationVectorTo(ctx, volID, h.dest.Name, method, advance); err != nil {
		return fmt.Errorf("advance destination vector for %s: %w", h.dest.Name, err)
	}
	rep.Status = store.RunStatusSuccess
	rep.Verification.Bytes = rep.RcloneResult.Bytes
	return nil
}

// advanceMethod chooses the method the content-addressed push advances its
// durability vector with: fingerprint-verified when every present content
// of the volume already carries a verified provider fingerprint on this
// destination (the whole-state check, not just this run's uploads), and
// presence+size otherwise. advanceLen is the size of the advance snapshot;
// an empty snapshot advances nothing, so the method is immaterial and the
// pending query is skipped.
func (h *contentPusher) advanceMethod(ctx context.Context, volID int64, advanceLen int) (string, error) {
	if advanceLen == 0 {
		return store.VerifyMethodPresenceSize, nil
	}
	pending, err := h.store.CountVolumeContentsPendingFingerprint(ctx, volID, h.dest.Name)
	if err != nil {
		return "", err
	}
	if pending == 0 {
		return store.VerifyMethodFingerprint, nil
	}
	return store.VerifyMethodPresenceSize, nil
}

// watermark resolves the run id the delta starts after: the last
// successful sync of this (volume, destination), or 0 for a fresh
// destination. The last success must still have its manifest segment at
// the destination — every successful content-addressed run uploads one,
// so its absence means the recorded history belongs to a different
// layout (a destination flipped from mirror) and a delta computed
// against it would silently skip everything the mirror era covered.
func (h *contentAddressedHandler) watermark(ctx context.Context, volID int64) (int64, error) {
	last, err := h.store.LatestSuccessfulSyncRun(ctx, volID, h.dest.Name)
	if store.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("lookup last successful sync of %s: %w", h.dest.Name, err)
	}
	segURI := h.segmentURI(last.ID)
	if _, err := h.rcl.statRemote(ctx, segURI, checkersArgs(h.dest)...); err != nil {
		if freshStartOnEmptyRoot(ctx, h.rcl, h.dest) {
			return 0, nil
		}
		return 0, fmt.Errorf("destination %q: the last successful sync (run %d) left no manifest segment at %s — its history does not look content-addressed; point the layout at a fresh destination or root, or (after wiping the remote root) run `squirrel destination reset %s`, instead of switching an existing one: %w", h.dest.Name, last.ID, segURI, h.dest.Name, err)
	}
	return last.ID, nil
}

// previewObjectUploads counts, without uploading, the split uploadObjects
// would record: planned content the destination has no record of lands on
// rep.RcloneResult as Transferred + Bytes, content already offsite (as a
// per-hash object or a pack member) as Checked. It writes nothing and
// touches no object. Both layouts' dry-run preview use it — the packed
// layout passes only its object-routed (large) content.
func (h *contentPusher) previewObjectUploads(ctx context.Context, rep *Report, planned []store.PathDelta) error {
	for _, d := range planned {
		recorded, err := h.store.ContentPresentOnDestination(ctx, d.ContentID, h.dest.Name)
		if err != nil {
			return fmt.Errorf("lookup upload record for %s: %w", d.Path, err)
		}
		if recorded {
			rep.RcloneResult.Checked++
			continue
		}
		rep.RcloneResult.Transferred++
		rep.RcloneResult.Bytes += d.SizeBytes
	}
	return nil
}

// uploadObjects lands every planned content object that the destination
// has no upload record for. planned is the deduplicated, present-only
// upload set (plannedUploads) — the content-addressed layout passes its
// whole delta's uploads, the packed layout passes only the files at or
// above its pack threshold. Counters land on
// rep.RcloneResult so the run report reads like the other rclone
// flows: Transferred = objects uploaded, Checked = objects skipped as
// already recorded, Errors/FailedFiles = per-object failures. Per-object
// failures don't stop the loop — every object that lands now is recorded
// and saves work on the retry — but any failure fails the run before
// the segment is written.
//
// A source whose bytes drifted from the indexed hash is refused without a
// remote_objects row (errContentDrift): it is surfaced as a warning and
// fails the run, so the segment is not written and the watermark does not
// advance. The next run recomputes the same delta and re-offers the
// object, letting the honest bytes land once they are restored — without
// the drifted bytes ever being recorded under the hash.
func (h *contentPusher) uploadObjects(ctx context.Context, rep *Report, runID int64, planned []store.PathDelta) error {
	var confirmed []store.PathDelta
	var drifted int
	for _, d := range planned {
		// Two-source presence: skip content already offsite as a per-hash
		// object or bundled in a pack already uploaded to this destination,
		// so a threshold change that reclassifies content never re-uploads
		// bytes the destination already holds in the other form.
		recorded, err := h.store.ContentPresentOnDestination(ctx, d.ContentID, h.dest.Name)
		if err != nil {
			return fmt.Errorf("lookup upload record for %s: %w", d.Path, err)
		}
		if recorded {
			rep.RcloneResult.Checked++
			continue
		}
		if err := h.uploadOneObject(ctx, runID, d); err != nil {
			if errors.Is(err, errContentDrift) {
				drifted++
				rep.Warnings = append(rep.Warnings, err.Error())
				continue
			}
			rep.RcloneResult.Errors++
			if int64(len(rep.RcloneResult.FailedFiles)) < maxFailedFiles {
				rep.RcloneResult.FailedFiles = append(rep.RcloneResult.FailedFiles,
					FailedFile{Object: d.Path, Message: err.Error()})
			}
			continue
		}
		confirmed = append(confirmed, d)
		rep.RcloneResult.Transferred++
		rep.RcloneResult.Bytes += d.SizeBytes
	}
	h.captureFingerprints(ctx, rep, confirmed)
	if rep.RcloneResult.Errors > 0 {
		return fmt.Errorf("%d object(s) failed to land on %q; the manifest segment for run %d was not written and the durability vector did not advance", rep.RcloneResult.Errors, h.dest.Name, runID)
	}
	if drifted > 0 {
		return fmt.Errorf("%d object(s) on %q were refused for drifting from their indexed hash; re-index the volume and sync again — the manifest segment for run %d was not written and the durability vector did not advance", drifted, h.dest.Name, runID)
	}
	return nil
}

// captureFingerprints fills the pending checksum pair of every object
// confirmed during this run with the provider checksum read back from the
// underlying remote's objects/ directory, over the shared scan-back capture
// surface. That read-back is the first verification, so each object records
// and stamps verified_at_ns in one write (SetRemoteObjectFingerprint) —
// matching the packed layout; the later `squirrel verify` re-read re-stamps
// it on each re-confirmation.
func (h *contentPusher) captureFingerprints(ctx context.Context, rep *Report, confirmed []store.PathDelta) {
	targets := make([]captureTarget, 0, len(confirmed))
	for _, d := range confirmed {
		d := d
		targets = append(targets, captureTarget{
			name:  hex.EncodeToString(d.Blake3),
			label: "object",
			record: func(ctx context.Context, algo, value string) error {
				return h.store.SetRemoteObjectFingerprint(ctx, d.ContentID, h.dest.Name, algo, value, store.NowNs())
			},
		})
	}
	h.captureScanBackFingerprints(ctx, rep, ObjectsDirName, targets)
}

// plannedUploads selects the delta rows that need a content object —
// status present, the bytes are on local disk — deduplicated to one
// source path per content hash. Delta order is deterministic, so so is
// the chosen source path.
func plannedUploads(delta []store.PathDelta) []store.PathDelta {
	seen := make(map[int64]bool, len(delta))
	var out []store.PathDelta
	for _, d := range delta {
		if d.Status != store.StatusPresent || seen[d.ContentID] {
			continue
		}
		seen[d.ContentID] = true
		out = append(out, d)
	}
	return out
}

// errContentDrift marks a source file whose bytes no longer match the
// content hash the index bound them to. The upload path raises it instead
// of recording an object; uploadObjects turns it into a warning and fails
// the run so the watermark holds and the object is re-offered next run.
var errContentDrift = errors.New("source content drifted from its indexed hash")

// uploadOneObject lands one content object and records the upload. It
// guards the content-addressed invariant — the bytes stored under a hash
// must be the bytes that produced it — by re-hashing the source file
// immediately before the transfer and refusing (errContentDrift) when the
// digest no longer matches the indexed hash, catching a
// size+mtime-preserving in-place edit that a metadata stat would pass.
// The post-transfer stat confirms presence and size on the remote, and the
// upload record is written only after that confirmation, so a recorded
// hash is always a confirmed one; a crash in between re-uploads the same
// bytes idempotently on the next run.
//
// Residual: rclone reads the file in a separate child process after the
// re-hash, so a writer that edits the file in the window between the hash
// and rclone's read could still upload drifted bytes. The window is the
// fork/exec of one rclone invocation rather than the whole walk-to-push
// span, and the scan-back fingerprint pass (#109) re-reads the landed
// object to upgrade the durability vector, catching any byte that slipped
// through before the object is treated as content-verified.
func (h *contentPusher) uploadOneObject(ctx context.Context, runID int64, d store.PathDelta) error {
	src := filepath.Join(h.vol.Path, filepath.FromSlash(d.Path))
	digest, err := hashLocalFile(src)
	if err != nil {
		return fmt.Errorf("re-hash %s before upload: %w", src, err)
	}
	if !bytes.Equal(digest, d.Blake3) {
		return fmt.Errorf("%w: %s now hashes to %s, indexed as %s — run `squirrel index %s` and sync again",
			errContentDrift, d.Path, hex.EncodeToString(digest), hex.EncodeToString(d.Blake3), h.vol.Name)
	}
	hash := hex.EncodeToString(d.Blake3)
	uri := h.objectURI(hash)
	if err := h.rcl.copyTo(ctx, src, uri, checkersArgs(h.dest)...); err != nil {
		return err
	}
	size, err := h.rcl.statRemote(ctx, uri, checkersArgs(h.dest)...)
	if err != nil {
		return fmt.Errorf("confirm object %s after upload: %w", hash, err)
	}
	if size != d.SizeBytes {
		return fmt.Errorf("object %s landed with size %d, want %d", hash, size, d.SizeBytes)
	}
	if err := h.store.InsertRemoteObject(ctx, store.RemoteObject{
		ContentID:     d.ContentID,
		Destination:   h.dest.Name,
		UploadedRunID: runID,
	}); err != nil {
		return fmt.Errorf("record upload of %s: %w", hash, err)
	}
	return nil
}

// hashLocalFile streams the file at path through BLAKE3 and returns the
// raw 32-byte digest, the same hash the indexer binds content under.
func hashLocalFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := blake3.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// uploadSegment writes the run's manifest segment and confirms it
// landed at the expected size. Every run uploads one — an unchanged
// volume yields an empty segment — so each successful run leaves the
// landing evidence the next watermark check looks for.
func (h *contentPusher) uploadSegment(ctx context.Context, delta []store.PathDelta, runID int64) error {
	body, err := encodeManifestSegment(delta)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "squirrel-manifest-*")
	if err != nil {
		return fmt.Errorf("stage manifest segment: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write manifest segment: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close manifest segment: %w", err)
	}

	uri := h.segmentURI(runID)
	if err := h.rcl.copyTo(ctx, tmp.Name(), uri, checkersArgs(h.dest)...); err != nil {
		return fmt.Errorf("upload manifest segment to %s: %w", uri, err)
	}
	size, err := h.rcl.statRemote(ctx, uri, checkersArgs(h.dest)...)
	if err != nil {
		return fmt.Errorf("confirm manifest segment at %s: %w", uri, err)
	}
	if size != int64(len(body)) {
		return fmt.Errorf("manifest segment at %s landed with size %d, want %d", uri, size, len(body))
	}
	return nil
}

// objectURI addresses one content object under the destination-root
// objects/ directory, through the crypt overlay when the destination
// has one.
func (h *contentPusher) objectURI(hash string) string {
	return remoteSubpathURI(h.dest, path.Join(ObjectsDirName, hash))
}

// segmentURI addresses one run's manifest segment under the
// destination's per-volume index/ directory.
func (h *contentPusher) segmentURI(runID int64) string {
	return remoteSubpathURI(h.dest, path.Join(h.vol.Name, ManifestDirName, "run-"+strconv.FormatInt(runID, 10)))
}

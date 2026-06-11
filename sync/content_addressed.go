package sync

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"

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
	store *store.Store
	rcl   *Rclone
	vol   *config.Volume
	dest  *config.Destination
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
		return rep, fmt.Errorf("destination %q: the content-addressed push has no dry-run mode yet — run without --dry-run", h.dest.Name)
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
	delta, err := h.store.ListPathDeltaSince(ctx, volID, watermark)
	if err != nil {
		return fmt.Errorf("compute path delta since run %d: %w", watermark, err)
	}
	rep.Verification.Files = int64(len(delta))
	if err := h.uploadObjects(ctx, rep, runID, delta); err != nil {
		return err
	}
	if err := h.uploadSegment(ctx, delta, runID); err != nil {
		return err
	}
	if err := h.store.AdvanceDestinationVector(ctx, volID, h.dest.Name); err != nil {
		return fmt.Errorf("advance destination vector for %s: %w", h.dest.Name, err)
	}
	rep.Status = store.RunStatusSuccess
	rep.Verification.Bytes = rep.RcloneResult.Bytes
	return nil
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
		return 0, fmt.Errorf("destination %q: the last successful sync (run %d) left no manifest segment at %s — its history does not look content-addressed; point the layout at a fresh destination or root instead of switching an existing one: %w", h.dest.Name, last.ID, segURI, err)
	}
	return last.ID, nil
}

// uploadObjects lands every content object the delta needs that the
// destination has no upload record for. Counters land on
// rep.RcloneResult so the run report reads like the other rclone
// flows: Transferred = objects uploaded, Checked = objects skipped as
// already recorded, Errors/FailedFiles = per-object failures. Per-object
// failures don't stop the loop — every object that lands now is recorded
// and saves work on the retry — but any failure fails the run before
// the segment is written.
func (h *contentAddressedHandler) uploadObjects(ctx context.Context, rep *Report, runID int64, delta []store.PathDelta) error {
	var confirmed []store.PathDelta
	for _, d := range plannedUploads(delta) {
		recorded, err := h.store.HasRemoteObject(ctx, d.ContentID, h.dest.Name)
		if err != nil {
			return fmt.Errorf("lookup upload record for %s: %w", d.Path, err)
		}
		if recorded {
			rep.RcloneResult.Checked++
			continue
		}
		if err := h.uploadOneObject(ctx, runID, d); err != nil {
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
	return nil
}

// fingerprintBatchSize caps how many freshly confirmed objects one
// lsjson invocation covers, bounding argv growth from the per-object
// --include filters.
const fingerprintBatchSize = 200

// captureFingerprints fills the pending checksum pair of every object
// confirmed during this run with the provider checksum read back from
// the underlying remote — batched into one `lsjson --hash` per chunk,
// scoped by --include filters so the backend hashes only this run's
// uploads. Capture problems are warnings, not failures: the upload is
// already confirmed and recorded, and `squirrel verify` fills any
// fingerprint left pending.
func (h *contentAddressedHandler) captureFingerprints(ctx context.Context, rep *Report, confirmed []store.PathDelta) {
	dirURI := underlyingObjectsURI(h.dest)
	types := captureHashTypes(h.dest)
	for batch := range slices.Chunk(confirmed, fingerprintBatchSize) {
		extra := checkersArgs(h.dest)
		for _, d := range batch {
			extra = append(extra, "--include", hex.EncodeToString(d.Blake3))
		}
		entries, err := h.rcl.listHashes(ctx, dirURI, types, extra...)
		if err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("fingerprint capture on %q failed: %v — checksums stay pending until `squirrel verify`", h.dest.Name, err))
			return
		}
		byName := make(map[string]map[string]string, len(entries))
		present := make(map[string]bool, len(entries))
		for _, e := range entries {
			byName[e.Name] = e.Hashes
			present[e.Name] = true
		}
		for _, d := range batch {
			hash := hex.EncodeToString(d.Blake3)
			if !present[hash] {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("object %s on %q: not yet returned by the remote listing; fingerprint stays pending", hash, h.dest.Name))
				continue
			}
			cs, ok := extractChecksum(h.dest, byName[hash])
			if !ok {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("object %s on %q: remote exposes no usable checksum (e.g. a multipart object whose ETag rclone does not surface as a hash); fingerprint stays pending", hash, h.dest.Name))
				continue
			}
			if err := h.store.SetRemoteObjectChecksum(ctx, d.ContentID, h.dest.Name, cs.Algo, cs.Value); err != nil {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("record fingerprint for %s: %v", hash, err))
				continue
			}
			rep.Fingerprints++
		}
	}
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

// uploadOneObject lands one content object and records the upload. The
// pre-transfer stat guards the content-addressed invariant — the bytes
// stored under a hash must be the bytes that produced it — by refusing
// a source file whose size or mtime drifted from the indexed row; the
// post-transfer stat confirms presence and size on the remote. The
// upload record is written only after that confirmation, so a recorded
// hash is always a confirmed one; a crash in between re-uploads the
// same bytes idempotently on the next run.
func (h *contentAddressedHandler) uploadOneObject(ctx context.Context, runID int64, d store.PathDelta) error {
	src := filepath.Join(h.vol.Path, filepath.FromSlash(d.Path))
	fi, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if fi.Size() != d.SizeBytes || fi.ModTime().UnixNano() != d.MtimeNs {
		return fmt.Errorf("%s changed on disk since it was indexed (size %d→%d, mtime %d→%d) — run `squirrel index %s` and sync again", d.Path, d.SizeBytes, fi.Size(), d.MtimeNs, fi.ModTime().UnixNano(), h.vol.Name)
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

// uploadSegment writes the run's manifest segment and confirms it
// landed at the expected size. Every run uploads one — an unchanged
// volume yields an empty segment — so each successful run leaves the
// landing evidence the next watermark check looks for.
func (h *contentAddressedHandler) uploadSegment(ctx context.Context, delta []store.PathDelta, runID int64) error {
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
func (h *contentAddressedHandler) objectURI(hash string) string {
	return remoteSubpathURI(h.dest, path.Join(ObjectsDirName, hash))
}

// segmentURI addresses one run's manifest segment under the
// destination's per-volume index/ directory.
func (h *contentAddressedHandler) segmentURI(runID int64) string {
	return remoteSubpathURI(h.dest, path.Join(h.vol.Name, ManifestDirName, "run-"+strconv.FormatInt(runID, 10)))
}

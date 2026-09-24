package sync

import (
	"context"
	"database/sql"
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
	// one). On an encrypted destination the basename is the keyed
	// name namer.object derives, so the remote discloses no content
	// hash. The directory is destination-global — shared by every
	// volume, matching remote_objects' (content, destination) key —
	// so duplicated content across volumes uploads once. An object is
	// uploaded once and never moved, overwritten, or deleted.
	ObjectsDirName = "objects"
	// ManifestDirName holds one immutable manifest segment per sync
	// run, per volume: <volume>/index/run-<run id> (the volume
	// directory keyed by namer.volumeDir on an encrypted destination),
	// the JSONL path-level delta of that run (see ManifestEntry).
	// Replaying a volume's segments in run-id order reconstructs its
	// full path→content mapping with no SQLite required. Distinct from
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
// for content below its threshold. art is where the artifacts land.
type contentPusher struct {
	store *store.Store
	vol   *config.Volume
	dest  *config.Destination
	art   artifactStore
}

// names is how this push names the artifacts it writes.
func (h *contentPusher) names() namer { return namerFor(h.dest) }

func (h *contentPusher) shelf(runID int64) snapshotShelf { return h.art.shelf(runID) }

func (h *contentPusher) markers(ctx context.Context, _ *Report, _ int64, opts Options) error {
	return h.art.markers(ctx, opts)
}

func (h *contentPusher) rootEmpty(ctx context.Context) (bool, error) { return h.art.rootEmpty(ctx) }

func (h *contentPusher) reconcile(ctx context.Context, rep *Report, _, runID int64) error {
	return h.art.reconcile(ctx, rep, runID)
}

func (h *contentPusher) target() pushTarget {
	return pushTarget{store: h.store, vol: h.vol, dest: h.dest}
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
	defer h.art.close()
	if h.vol.Name == ObjectsDirName {
		rep := Report{Volume: h.vol.Name, Destination: h.dest.Name}
		rep.Verification.Method = VerifyMethodPresenceSize
		return rep, fmt.Errorf("volume %q: the name collides with the destination-root %s/ directory of content-addressed destination %q — rename the volume or use a mirrored destination", h.vol.Name, ObjectsDirName, h.dest.Name)
	}
	return pushThrough(ctx, h.target(), h, opts)
}

func (h *contentAddressedHandler) sealed() {}

// translate selects the delta's present content, one source path per hash,
// and splits it by whether the destination already records it.
func (h *contentAddressedHandler) translate(ctx context.Context, p pushPlan) (objectUploads, error) {
	return h.splitRecorded(ctx, plannedUploads(p.delta))
}

// execute lands every object the destination has no record of.
func (h *contentAddressedHandler) execute(ctx context.Context, rep *Report, runID int64, ops objectUploads) error {
	return h.uploadObjects(ctx, rep, runID, ops)
}

// seal writes the run's manifest segment, the layout's landing evidence.
func (h *contentAddressedHandler) seal(ctx context.Context, _ *Report, runID int64, p pushPlan, _ objectUploads) error {
	return h.uploadSegment(ctx, p.delta, runID)
}

// advanceMethod chooses fingerprint-verified when every present content of
// the volume already carries a verified provider fingerprint on this
// destination (the whole-state check, not just this run's uploads), and
// presence+size otherwise: presence+size is not content-verified (crypt
// remotes expose no hashes), so the offload gate's per-object scan-back
// backs it locally, and a single still-pending object holds the upgrade.
// An empty advance advances nothing, so the pending query is skipped.
func (h *contentAddressedHandler) advanceMethod(ctx context.Context, _ *Report, p pushPlan) (string, error) {
	if len(p.advance) == 0 {
		return store.VerifyMethodPresenceSize, nil
	}
	pending, err := h.store.CountVolumeContentsPendingFingerprint(ctx, p.volumeID, h.dest.Name)
	if err != nil {
		return "", err
	}
	if pending == 0 {
		return store.VerifyMethodFingerprint, nil
	}
	return store.VerifyMethodPresenceSize, nil
}

// landed reports whether runID's manifest segment is at the destination.
// Every successful content-addressed run uploads one, so its absence means
// the recorded history belongs to a different layout or a wiped root.
func (h *contentAddressedHandler) landed(ctx context.Context, runID int64) (bool, error) {
	return h.art.exists(ctx, h.segmentName(runID))
}

func (h *contentAddressedHandler) foreignHistory(runID int64) error {
	return fmt.Errorf("destination %q: the last successful sync (run %d) left no manifest segment at %s — its history does not look content-addressed; point the layout at a fresh destination or root, or (after wiping the remote root) run `squirrel destination reset %s`, instead of switching an existing one: %w", h.dest.Name, runID, h.art.where(h.segmentName(runID)), h.dest.Name, ErrRefused)
}

// objectUploads is the object side of a content push: the planned content
// the destination has no upload record for, and how many planned contents
// it already records (as a per-hash object or a pack member).
type objectUploads struct {
	needed   []store.PathDelta
	recorded int64
}

// preview counts the split uploadObjects would record: needed content as
// Transferred + Bytes, recorded content as Checked.
func (o objectUploads) preview(rep *Report) {
	rep.RcloneResult.Checked += o.recorded
	for _, d := range o.needed {
		rep.RcloneResult.Transferred++
		rep.RcloneResult.Bytes += d.SizeBytes
	}
}

// splitRecorded splits planned content by whether the destination already
// records it offsite, as a per-hash object or a pack member, so a threshold
// change that reclassifies content never re-uploads bytes the destination
// already holds in the other form. Both content layouts translate their
// object side through it.
func (h *contentPusher) splitRecorded(ctx context.Context, planned []store.PathDelta) (objectUploads, error) {
	var out objectUploads
	for _, d := range planned {
		recorded, err := h.store.ContentPresentOnDestination(ctx, d.ContentID, h.dest.Name)
		if err != nil {
			return objectUploads{}, fmt.Errorf("lookup upload record for %s: %w", d.Path, err)
		}
		if recorded {
			out.recorded++
			continue
		}
		out.needed = append(out.needed, d)
	}
	return out, nil
}

// uploadObjects lands every planned content object the destination has
// no upload record for. The content-addressed layout passes its whole
// delta's uploads, the packed layout only the files at or above its pack
// threshold. Counters land on rep.RcloneResult so the run report reads
// like the other rclone flows: Transferred = objects uploaded, Checked =
// objects skipped as already recorded, Errors/FailedFiles = per-object
// failures. Per-object failures don't stop the loop — every object that
// lands now is recorded and saves work on the retry — but any failure
// fails the run before the segment is written.
//
// A source whose bytes drifted from the indexed hash is refused without a
// remote_objects row (errContentDrift): it is surfaced as a warning and
// fails the run, so the segment is not written and the watermark does not
// advance. The next run recomputes the same delta and re-offers the
// object, letting the honest bytes land once they are restored — without
// the drifted bytes ever being recorded under the hash.
func (h *contentPusher) uploadObjects(ctx context.Context, rep *Report, runID int64, ops objectUploads) error {
	rep.RcloneResult.Checked += ops.recorded
	var pending []store.PathDelta
	var drifted int
	for _, d := range ops.needed {
		fingerprinted, err := h.uploadOneObject(ctx, runID, d)
		if err != nil {
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
		if fingerprinted {
			rep.Fingerprints++
		} else {
			pending = append(pending, d)
		}
		rep.RcloneResult.Transferred++
		rep.RcloneResult.Bytes += d.SizeBytes
	}
	h.captureFingerprints(ctx, rep, pending)
	if rep.RcloneResult.Errors > 0 {
		return fmt.Errorf("%d object(s) failed to land on %q; the manifest segment for run %d was not written and the durability vector did not advance", rep.RcloneResult.Errors, h.dest.Name, runID)
	}
	if drifted > 0 {
		return fmt.Errorf("%d object(s) on %q were refused for drifting from their indexed hash; re-index the volume and sync again — the manifest segment for run %d was not written and the durability vector did not advance", drifted, h.dest.Name, runID)
	}
	return nil
}

// captureFingerprints fills the pending checksum pair of every object
// confirmed during this run without one, read back from the destination's
// objects/ directory (the scan-back capture over rclone). That read-back is
// the first verification, so each object records and stamps verified_at_ns
// in one write (SetRemoteObjectFingerprint) — matching the packed layout;
// the later `squirrel verify` re-read re-stamps it on each re-confirmation.
func (h *contentPusher) captureFingerprints(ctx context.Context, rep *Report, pending []store.PathDelta) {
	if len(pending) == 0 {
		return
	}
	targets := make([]captureTarget, 0, len(pending))
	for _, d := range pending {
		d := d
		targets = append(targets, captureTarget{
			name:  h.names().object(d.Blake3),
			label: "object",
			record: func(ctx context.Context, algo, value string) error {
				return h.store.SetRemoteObjectFingerprint(ctx, d.ContentID, h.dest.Name, algo, value, store.NowNs())
			},
		})
	}
	h.art.capture(ctx, rep, ObjectsDirName, targets)
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

// uploadOneObject lands one content object and records the upload, with
// its fingerprint when the landing already confirmed one. It guards the
// content-addressed invariant — the bytes stored under a hash must be the
// bytes that produced it: the artifact store refuses a source that no
// longer hashes to the indexed hash (errContentDrift), catching a
// size+mtime-preserving in-place edit that a metadata stat would pass. The
// upload record is written only after the landing was confirmed, so a
// recorded hash is always a confirmed one; a crash in between lands the
// same bytes again on the next run.
func (h *contentPusher) uploadOneObject(ctx context.Context, runID int64, d store.PathDelta) (bool, error) {
	src := filepath.Join(h.vol.Path, filepath.FromSlash(d.Path))
	cs, err := h.art.put(ctx, runID, h.objectName(d.Blake3), src, d.SizeBytes, d.Blake3)
	if errors.Is(err, errContentDrift) {
		return false, fmt.Errorf("%s: %w — run `squirrel index %s` and sync again", d.Path, err, h.vol.Name)
	}
	if err != nil {
		return false, err
	}
	obj := store.RemoteObject{ContentID: d.ContentID, Destination: h.dest.Name, UploadedRunID: runID}
	if cs != nil {
		obj.ChecksumAlgo = sql.NullString{String: cs.Algo, Valid: true}
		obj.Checksum = sql.NullString{String: cs.Value, Valid: true}
		obj.VerifiedAtNs = sql.NullInt64{Int64: store.NowNs(), Valid: true}
	}
	if err := h.store.InsertRemoteObject(ctx, obj); err != nil {
		return false, fmt.Errorf("record upload of %s: %w", hex.EncodeToString(d.Blake3), err)
	}
	return cs != nil, nil
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
	return putBytes(ctx, h.art, runID, h.segmentName(runID), body, "manifest segment")
}

// objectName is one content object under the destination-root objects/
// directory. The basename is namer.object's.
func (h *contentPusher) objectName(contentHash []byte) string {
	return path.Join(ObjectsDirName, h.names().object(contentHash))
}

// segmentName is one run's manifest segment under the destination's
// per-volume index/ directory. The run id stays in clear: replaying the
// segments in run order is what recovers a volume without squirrel, and
// that ordering has to survive without the naming key.
func (h *contentPusher) segmentName(runID int64) string {
	return path.Join(h.names().volumeDir(h.vol.Name), ManifestDirName, "run-"+strconv.FormatInt(runID, 10))
}

package sync

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/store"
)

// PacksDirName holds the packed layout's tar.zst packs and the per-run
// placement maps at the destination root: packs/<pack-key hex> is one
// immutable pack (raw compressed bytes, encrypted by the crypt overlay
// when the destination has one), packs/map-<run id> the run's placement
// map (see PlacementEntry). Like objects/, the directory is
// destination-global — packs are addressed by the BLAKE3 of their
// compressed bytes, so an identical pack assembled from an identical
// content set names the same file — and run ids are globally unique, so a
// map name never collides across volumes.
const PacksDirName = "packs"

// packMapPrefix names a run's placement map under PacksDirName.
const packMapPrefix = "map-"

// PlacementEntry is one line of a placement map: where a single newly
// packed content lives inside its pack's uncompressed tar. JSON-encoded
// with exactly these fields in this order, one object per line (JSONL).
//
//	{"blake3":"<64 hex>","pack":"<64 hex>","offset":512,"length":123}
//
// Replaying a volume's manifest segments (path→blake3) together with every
// placement map (blake3→pack, offset, length) reconstructs path → hash →
// (pack, offset, length): decompress packs/<pack> with stock zstd, then the
// member's bytes are the offset..offset+length slice of the uncompressed
// tar (equivalently, the tar member named <blake3>). The format is stable
// so a small external script can recover data from the destination without
// squirrel.
type PlacementEntry struct {
	Blake3 string `json:"blake3"`
	Pack   string `json:"pack"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
}

// encodePlacementMap renders placements as JSONL in the given order, which
// is deterministic (packs are built over hash-sorted content, members
// stay in that order). An empty run encodes to an empty map; the map still
// uploads so every successful run leaves the landing evidence the next
// watermark check looks for.
func encodePlacementMap(placements []PlacementEntry) ([]byte, error) {
	var out []byte
	for _, p := range placements {
		line, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("encode placement entry for %s: %w", p.Blake3, err)
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out, nil
}

// PackPreview reports the pack side of a packed --dry-run push: the
// content that would be bundled into packs rather than uploaded as
// per-hash objects. It is populated only by a packed dry-run; zero-valued
// for every other push (a real packed push and a content-addressed
// dry-run both leave it zero, reporting only through RcloneResult).
//
// Bytes is the uncompressed member total — the compressed pack size is
// unknown until a pack is assembled, which a dry-run must not do — and
// Packs is an estimate of the resulting pack count from the same size-band
// walk buildOnePack uses, run over uncompressed sizes. It is an estimate,
// not a bound, and can be wrong in either direction: compression shrinks
// the packed output (fewer packs), while the PAX tar headers, block
// padding, and zstd framing a pack adds over many small members can push
// the real output past the uncompressed total (more packs). SizeBand
// echoes the destination's pack_size, the target compressed pack size.
type PackPreview struct {
	Contents int64 // members that would be newly packed
	Bytes    int64 // their total uncompressed bytes
	Packs    int64 // estimated pack count from the uncompressed size-band walk
	SizeBand int64 // destination pack_size (target compressed pack size)
}

// packedHandler pushes a volume to a packed rclone destination. Content at
// or above dest.PackThreshold lands as a per-hash object exactly as the
// content-addressed layout does (reusing contentPusher's object path);
// content below the threshold is bundled into immutable tar.zst packs. Each
// run writes a placement map locating its newly packed content plus the
// same per-volume manifest segment the content-addressed layout writes, so
// the two together recover path → hash → bytes with no SQLite.
//
// Durability: this handler uploads packs, records them in the local
// packs/pack_members tables, reads each pack's scan-back fingerprint into
// remote_packs, and advances the destination durability vector at
// presence+size only once every pack this run assembled is
// fingerprint-verified alongside the placement map and manifest segment
// (see certifyPacked). The offload gate then certifies a packed content
// through its pack's verified remote_packs row. Large files reuse the
// content-addressed object path and keep its per-object remote_objects
// record and fingerprint capture.
type packedHandler struct {
	contentPusher
}

func (h *packedHandler) TargetName() string { return h.dest.Name }

func (h *packedHandler) sealed() {}

func (h *packedHandler) Push(ctx context.Context, opts Options) (Report, error) {
	rep := Report{Volume: h.vol.Name, Destination: h.dest.Name}
	// Presence+size is the strongest check the packed push claims; it is
	// not content-verified (crypt remotes expose no hashes, and packs carry
	// no fingerprint until PR 3), so results stay unverified.
	rep.Verification.Method = VerifyMethodPresenceSize
	if h.vol.Name == ObjectsDirName || h.vol.Name == PacksDirName {
		return rep, fmt.Errorf("volume %q: the name collides with the destination-root %s/ directory of packed destination %q — rename the volume or use a mirrored destination", h.vol.Name, h.vol.Name, h.dest.Name)
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
	// shallow=true: neither the per-object copyto nor the pack copyto
	// carries a BLAKE3 end-to-end check, and the audit trail stays honest.
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

// previewDryRun reports what a real packed push would do — which content
// would upload as per-hash objects vs. be bundled into packs — without
// assembling or uploading any pack and without writing a runs row:
// rep.RunID stays 0 and no packs, pack_members, remote_*, objects, or
// pack artifacts are written. It computes the same delta and routing the
// real push does (watermark → delta → routeBySize); the object side lands
// on rep.RcloneResult exactly as the content-addressed preview reports it,
// the pack side on rep.PackPreview. The watermark check still reads the
// destination, so a layout-flip is refused here too.
func (h *packedHandler) previewDryRun(ctx context.Context, rep *Report, volID int64) error {
	watermark, err := h.watermark(ctx, volID)
	if err != nil {
		return err
	}
	delta, err := h.store.ListPathDeltaSince(ctx, volID, watermark)
	if err != nil {
		return fmt.Errorf("compute path delta since run %d: %w", watermark, err)
	}
	rep.Verification.Files = int64(len(delta))
	large, small, err := h.routeBySize(ctx, rep, plannedUploads(delta))
	if err != nil {
		return err
	}
	if err := h.previewObjectUploads(ctx, rep, large); err != nil {
		return err
	}
	rep.PackPreview = h.previewPacks(small)
	rep.Status = store.RunStatusSuccess
	return nil
}

// previewPacks summarises the small (pack-routed) content routeBySize
// selected, without assembling a pack. small is already hash-sorted, so
// the size-band walk mirrors buildOnePack's — accumulate members until the
// running total reaches PackSize, then close a pack — but over uncompressed
// sizes, yielding an estimated (not bounded) pack count (see PackPreview).
func (h *packedHandler) previewPacks(small []store.PathDelta) PackPreview {
	p := PackPreview{SizeBand: h.dest.PackSize}
	for i := 0; i < len(small); {
		var packBytes int64
		for i < len(small) {
			packBytes += small[i].SizeBytes
			p.Contents++
			p.Bytes += small[i].SizeBytes
			i++
			if packBytes >= h.dest.PackSize {
				break
			}
		}
		p.Packs++
	}
	return p
}

// push runs the transactional landing: delta → objects (large) + packs
// (small) → placement map → manifest segment → local pack rows → per-pack
// fingerprints → vector advance. rep.Status starts failed and is promoted
// only after every piece is confirmed present, so a partial landing never
// presents as success.
func (h *packedHandler) push(ctx context.Context, rep *Report, volID, runID int64) error {
	rep.Status = store.RunStatusFailed
	watermark, err := h.watermark(ctx, volID)
	if err != nil {
		return err
	}
	// Snapshot the advance before any transfer so a row committed mid-push
	// is never folded into it — the same discipline the content-addressed
	// handler uses.
	advance, err := captureDurabilityAdvance(ctx, h.store, volID)
	if err != nil {
		return err
	}
	delta, err := h.store.ListPathDeltaSince(ctx, volID, watermark)
	if err != nil {
		return fmt.Errorf("compute path delta since run %d: %w", watermark, err)
	}
	rep.Verification.Files = int64(len(delta))
	// Same reading as the content-addressed push: the delta is what this
	// run changes at the destination, so an empty one folds as a no-op.
	rep.Changed = knownChanged(int64(len(delta)))
	large, small, err := h.routeBySize(ctx, rep, plannedUploads(delta))
	if err != nil {
		return err
	}
	if err := h.uploadObjects(ctx, rep, runID, large); err != nil {
		return err
	}
	writes, placements, err := h.assembleAndUploadPacks(ctx, rep, runID, small)
	if err != nil {
		return err
	}
	if err := h.uploadPlacementMap(ctx, placements, runID); err != nil {
		return err
	}
	if err := h.uploadSegment(ctx, delta, runID); err != nil {
		return err
	}
	// Recorded only after every pack, the map, and the segment confirmed
	// landing: a pack_members row always has its bytes (and their location)
	// offsite, and a run that failed earlier left nothing here to re-pack.
	if err := h.store.InsertPacks(ctx, writes); err != nil {
		return fmt.Errorf("record packs for run %d: %w", runID, err)
	}
	rep.Status = store.RunStatusSuccess
	rep.Verification.Bytes = rep.RcloneResult.Bytes
	return h.certifyPacked(ctx, rep, volID, runID, writes, advance)
}

// certifyPacked closes the durability seam: it records a remote_packs row
// per landed pack, reads each pack's scan-back fingerprint, and advances
// the destination vector to fingerprint-verified only once the whole
// (volume, destination) pair carries a verified fingerprint behind every
// present file content. That whole-state check is
// CountVolumeContentsPendingFingerprint, which counts present contents
// lacking a verified remote_objects (per-hash object) or remote_packs (pack
// member) fingerprint on this destination — so it covers packs and per-hash
// objects, the two artifacts that carry content bytes.
//
// The placement map and manifest segment are deliberately NOT in that
// pending set: they are re-derivable squirrel-written metadata that carry
// no scan-back fingerprint. Their durability before this advance is handled
// upstream in push, which uploads and confirms both landed at their
// expected size before promoting the run to success — a run whose map or
// segment failed to land returns failed and never reaches certifyPacked, so
// the vector is untouched.
//
// Gating on the whole pending set rather than this run's writes is the
// friction-log F13 fix: a run that packs nothing no longer advances
// vacuously past an earlier still-pending pack, and a still-pending artifact
// anywhere in the pair holds the whole advance. `squirrel verify` fills a
// pending fingerprint later and re-attempts this advance itself (see the
// store upgrade), so the vector no longer stalls until the next
// content-writing sync. rep.Verification stays unverified (presence+size is
// not content-verified), so RunPair does not advance the vector a second
// time.
func (h *packedHandler) certifyPacked(ctx context.Context, rep *Report, volID, runID int64, writes []store.PackWrite, advance []store.OriginComponent) error {
	h.capturePackFingerprints(ctx, rep, runID, writes)
	pending, err := h.store.CountVolumeContentsPendingFingerprint(ctx, volID, h.dest.Name)
	if err != nil {
		return fmt.Errorf("count pending fingerprints for %s: %w", h.dest.Name, err)
	}
	if pending > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("destination %q: %d content(s) are not yet fingerprint-verified; the durability vector was not advanced — run `squirrel verify` to certify them", h.dest.Name, pending))
		return nil
	}
	if len(advance) == 0 {
		return nil
	}
	if err := h.store.AdvanceDestinationVectorTo(ctx, volID, h.dest.Name, store.VerifyMethodFingerprint, advance); err != nil {
		return fmt.Errorf("advance destination vector for %s: %w", h.dest.Name, err)
	}
	return nil
}

// capturePackFingerprints records a per-destination upload row for each
// landed pack and fills its scan-back fingerprint, reusing the shared
// capture surface over the packs/ directory. Every pack exceeds the
// multipart threshold, so on s3 the composite ETag is read from the S3 API
// (never rclone's md5 slot, which is blank for a multipart object); other
// backends read `rclone lsjson --hash`. A pack whose fingerprint could not
// be read stays pending (checksum NULL) with a warning — never a fabricated
// value. The whole-pair pending tally (CountVolumeContentsPendingFingerprint)
// is what certifyPacked gates the vector advance on, so this returns nothing.
func (h *packedHandler) capturePackFingerprints(ctx context.Context, rep *Report, runID int64, writes []store.PackWrite) {
	targets := make([]captureTarget, 0, len(writes))
	for _, w := range writes {
		pack, err := h.store.GetPackByKey(ctx, w.Pack.PackKey)
		if err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("look up recorded pack %s: %v", hex.EncodeToString(w.Pack.PackKey), err))
			continue
		}
		if err := h.store.InsertRemotePack(ctx, store.RemotePack{
			PackID: pack.ID, Destination: h.dest.Name, UploadedRunID: runID,
		}); err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("record pack upload %s: %v", hex.EncodeToString(w.Pack.PackKey), err))
			continue
		}
		packID := pack.ID
		targets = append(targets, captureTarget{
			name:  hex.EncodeToString(w.Pack.PackKey),
			label: "pack",
			record: func(ctx context.Context, algo, value string) error {
				return h.store.SetRemotePackFingerprint(ctx, packID, h.dest.Name, algo, value, store.NowNs())
			},
		})
	}
	h.captureScanBackFingerprints(ctx, rep, PacksDirName, targets)
}

// watermark resolves the run id the delta starts after: the last
// successful sync of this (volume, destination), or 0 for a fresh
// destination. The last success must still have its placement map at the
// destination — every successful packed run uploads one (empty when no
// small content was new) — so its absence means the recorded history
// belongs to a different layout (a mirror leaves no map, a
// content-addressed root leaves objects and segments but no packs/ map) and
// a delta computed against it would silently skip everything that era
// covered. This is the packed analogue of the content-addressed watermark
// guard, strengthened to also refuse an object-per-hash history.
func (h *packedHandler) watermark(ctx context.Context, volID int64) (int64, error) {
	last, err := h.store.LatestSuccessfulSyncRun(ctx, volID, h.dest.Name)
	if store.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("lookup last successful sync of %s: %w", h.dest.Name, err)
	}
	mapURI := h.mapURI(last.ID)
	if _, err := h.rcl.statRemote(ctx, mapURI, checkersArgs(h.dest)...); err != nil {
		if freshStartOnEmptyRoot(ctx, h.rcl, h.dest) {
			return 0, nil
		}
		return 0, fmt.Errorf("destination %q: the last successful sync (run %d) left no pack placement map at %s — its history is not packed (a mirror or content-addressed root); point the layout at a fresh destination or root, or (after wiping the remote root) run `squirrel destination reset %s`, instead of switching an existing one: %w: %w", h.dest.Name, last.ID, mapURI, h.dest.Name, err, ErrRefused)
	}
	return last.ID, nil
}

// routeBySize splits the planned uploads by dest.PackThreshold: content at
// or above it becomes a per-hash object (large), content below it becomes a
// pack member (small) unless it is already packed — a pack is
// content-global and assembled once, so already-packed content is skipped
// and counted as checked. small is returned sorted by hash so pack
// assembly is deterministic.
func (h *packedHandler) routeBySize(ctx context.Context, rep *Report, planned []store.PathDelta) (large, small []store.PathDelta, err error) {
	for _, d := range planned {
		if d.SizeBytes >= h.dest.PackThreshold {
			large = append(large, d)
			continue
		}
		packed, err := h.store.HasPackMember(ctx, d.ContentID)
		if err != nil {
			return nil, nil, fmt.Errorf("lookup pack member for %s: %w", d.Path, err)
		}
		if packed {
			rep.RcloneResult.Checked++
			continue
		}
		small = append(small, d)
	}
	slices.SortFunc(small, func(a, b store.PathDelta) int {
		return bytes.Compare(a.Blake3, b.Blake3)
	})
	return large, small, nil
}

// assembleAndUploadPacks bundles the sorted small content into tar.zst
// packs, staging and uploading one pack at a time so memory and disk stay
// bounded regardless of corpus size. It returns the pack rows to record
// locally and the placement entries for the run's map; nothing is recorded
// in the store here (push does that only after the map and segment land).
func (h *packedHandler) assembleAndUploadPacks(ctx context.Context, rep *Report, runID int64, small []store.PathDelta) ([]store.PackWrite, []PlacementEntry, error) {
	level := zstdEncoderLevel(h.dest.ZstdLevel)
	var writes []store.PackWrite
	var placements []PlacementEntry
	for i := 0; i < len(small); {
		pack, next, err := h.buildOnePack(small, i, level)
		if err != nil {
			return nil, nil, err
		}
		i = next
		if err := h.uploadPack(ctx, pack); err != nil {
			return nil, nil, err
		}
		rep.RcloneResult.Transferred++
		rep.RcloneResult.Bytes += pack.compressedSize
		writes = append(writes, pack.toWrite(runID))
		placements = append(placements, pack.placements()...)
	}
	return writes, placements, nil
}

// buildOnePack assembles content from srcs starting at index start into a
// single staged pack, closing it when the compressed output reaches the
// dest.PackSize band (or srcs is exhausted). It returns the staged pack and
// the index of the next unpacked source. The caller uploads the pack and
// removes its temp file (uploadPack does).
func (h *packedHandler) buildOnePack(srcs []store.PathDelta, start int, level zstd.EncoderLevel) (assembledPack, int, error) {
	tmp, err := os.CreateTemp("", "squirrel-pack-*")
	if err != nil {
		return assembledPack{}, 0, fmt.Errorf("stage pack: %w", err)
	}
	discard := func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }
	pw, err := newPackWriter(tmp, level)
	if err != nil {
		discard()
		return assembledPack{}, 0, err
	}
	i := start
	for i < len(srcs) {
		d := srcs[i]
		src := packSource{
			contentID: d.ContentID,
			blake3:    d.Blake3,
			size:      d.SizeBytes,
			srcPath:   filepath.Join(h.vol.Path, filepath.FromSlash(d.Path)),
		}
		if err := pw.add(src); err != nil {
			pw.close() // release the encoder before dropping the staged pack
			discard()
			return assembledPack{}, 0, fmt.Errorf("pack %s: %w", d.Path, err)
		}
		i++
		if pw.compressedSize() >= h.dest.PackSize {
			break
		}
	}
	key, size, members, err := pw.finish()
	if err != nil {
		discard()
		return assembledPack{}, 0, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return assembledPack{}, 0, fmt.Errorf("close staged pack: %w", err)
	}
	return assembledPack{tmpPath: tmp.Name(), key: key, compressedSize: size, members: members}, i, nil
}

// uploadPack copies one staged pack to packs/<pack-key hex> through the
// crypt overlay and confirms it landed at the compressed size, then removes
// the temp file. Through a crypt overlay the reported size is the decrypted
// length, which is the compressed pack, so it compares directly.
func (h *packedHandler) uploadPack(ctx context.Context, pack assembledPack) error {
	defer func() { _ = os.Remove(pack.tmpPath) }()
	hexKey := hex.EncodeToString(pack.key)
	uri := h.packURI(hexKey)
	if err := h.rcl.copyTo(ctx, pack.tmpPath, uri, checkersArgs(h.dest)...); err != nil {
		return fmt.Errorf("upload pack %s: %w", hexKey, err)
	}
	size, err := h.rcl.statRemote(ctx, uri, checkersArgs(h.dest)...)
	if err != nil {
		return fmt.Errorf("confirm pack %s after upload: %w", hexKey, err)
	}
	if size != pack.compressedSize {
		return fmt.Errorf("pack %s landed with size %d, want %d", hexKey, size, pack.compressedSize)
	}
	return nil
}

// uploadPlacementMap writes the run's placement map and confirms it landed
// at the expected size. Every run uploads one — an empty run yields an
// empty map — so each success leaves the landing evidence the watermark
// check looks for.
func (h *packedHandler) uploadPlacementMap(ctx context.Context, placements []PlacementEntry, runID int64) error {
	body, err := encodePlacementMap(placements)
	if err != nil {
		return err
	}
	return h.uploadBytes(ctx, body, h.mapURI(runID), "placement map")
}

// uploadBytes stages body in a temp file, copies it to uri through the
// crypt overlay, and confirms it landed at len(body). what names the
// artifact in error messages.
func (h *packedHandler) uploadBytes(ctx context.Context, body []byte, uri, what string) error {
	tmp, err := os.CreateTemp("", "squirrel-packmeta-*")
	if err != nil {
		return fmt.Errorf("stage %s: %w", what, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", what, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", what, err)
	}
	if err := h.rcl.copyTo(ctx, tmp.Name(), uri, checkersArgs(h.dest)...); err != nil {
		return fmt.Errorf("upload %s to %s: %w", what, uri, err)
	}
	size, err := h.rcl.statRemote(ctx, uri, checkersArgs(h.dest)...)
	if err != nil {
		return fmt.Errorf("confirm %s at %s: %w", what, uri, err)
	}
	if size != int64(len(body)) {
		return fmt.Errorf("%s at %s landed with size %d, want %d", what, uri, size, len(body))
	}
	return nil
}

// packURI addresses one pack under the destination-root packs/ directory,
// through the crypt overlay when the destination has one. The pack name is
// its content-addressed key, opaque already, so no filename encryption is
// needed.
func (h *packedHandler) packURI(hexKey string) string {
	return remoteSubpathURI(h.dest, path.Join(PacksDirName, hexKey))
}

// mapURI addresses one run's placement map under the destination-root
// packs/ directory.
func (h *packedHandler) mapURI(runID int64) string {
	return remoteSubpathURI(h.dest, path.Join(PacksDirName, packMapPrefix+strconv.FormatInt(runID, 10)))
}

// zstdEncoderLevel maps the config's 1..4 zstd level onto klauspost's
// fastest..best encoder levels (validated to this range at config load).
func zstdEncoderLevel(level int) zstd.EncoderLevel {
	switch level {
	case 1:
		return zstd.SpeedFastest
	case 2:
		return zstd.SpeedDefault
	case 4:
		return zstd.SpeedBestCompression
	default:
		return zstd.SpeedBetterCompression
	}
}

// packSource is one content to add to a pack: its identity, indexed size,
// and the local file the bytes are read from.
type packSource struct {
	contentID int64
	blake3    []byte
	size      int64
	srcPath   string
}

// packedMember is a content's placement inside a pack under assembly: the
// data offset and length in the uncompressed tar, plus the identity needed
// for the local row and the placement map.
type packedMember struct {
	contentID int64
	blake3    []byte
	offset    int64
	length    int64
}

// assembledPack is one staged, uploaded-pending pack: the temp file holding
// its compressed bytes, the pack key (BLAKE3 of those bytes), the
// compressed size, and its members.
type assembledPack struct {
	tmpPath        string
	key            []byte
	compressedSize int64
	members        []packedMember
}

func (p assembledPack) toWrite(runID int64) store.PackWrite {
	members := make([]store.PackMember, len(p.members))
	for i, m := range p.members {
		members[i] = store.PackMember{
			ContentID:  m.contentID,
			ByteOffset: m.offset,
			ByteLength: m.length,
		}
	}
	return store.PackWrite{
		Pack: store.Pack{
			PackKey:      p.key,
			SizeBytes:    p.compressedSize,
			MemberCount:  int64(len(p.members)),
			CreatedRunID: runID,
		},
		Members: members,
	}
}

func (p assembledPack) placements() []PlacementEntry {
	hexKey := hex.EncodeToString(p.key)
	out := make([]PlacementEntry, len(p.members))
	for i, m := range p.members {
		out[i] = PlacementEntry{
			Blake3: hex.EncodeToString(m.blake3),
			Pack:   hexKey,
			Offset: m.offset,
			Length: m.length,
		}
	}
	return out
}

// countWriter counts the bytes written through it, so pack assembly can
// track both the uncompressed tar offset of each member and the compressed
// output size for the PackSize band.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// packWriter assembles one deterministic pack: a normalized PAX tar of
// members named by BLAKE3 hex, solid-compressed with a single zstd stream
// whose bytes are hashed to yield the pack key. ucount tracks the
// uncompressed tar position (for member offsets); ccount tracks the
// compressed output (for the size band and the final size); the hasher runs
// over the compressed bytes.
type packWriter struct {
	hasher  *blake3.Hasher
	ccount  *countWriter
	zw      *zstd.Encoder
	ucount  *countWriter
	tw      *tar.Writer
	members []packedMember
}

// newPackWriter wires tar → uncompressed counter → zstd → compressed
// counter → (dst, hasher). Concurrency is pinned to one so the compressed
// bytes — and thus the pack key — are reproducible for a given input and
// level, making retry within a run idempotent.
func newPackWriter(dst io.Writer, level zstd.EncoderLevel) (*packWriter, error) {
	hasher := blake3.New()
	ccount := &countWriter{w: io.MultiWriter(dst, hasher)}
	zw, err := zstd.NewWriter(ccount, zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, fmt.Errorf("new zstd writer: %w", err)
	}
	ucount := &countWriter{w: zw}
	return &packWriter{
		hasher: hasher,
		ccount: ccount,
		zw:     zw,
		ucount: ucount,
		tw:     tar.NewWriter(ucount),
	}, nil
}

// add streams one content into the pack under a normalized header (zeroed
// mtime/uid/gid, fixed read-only mode, PAX format) and records its data
// span in the uncompressed tar. The bytes are re-hashed as they are read
// and a drift from the indexed hash fails the build (errContentDrift), so a
// size+mtime-preserving in-place edit never lands in a pack under the wrong
// hash — the content-addressed invariant, applied to packed content.
func (p *packWriter) add(src packSource) error {
	f, err := os.Open(src.srcPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", src.srcPath, err)
	}
	defer f.Close()
	hdr := &tar.Header{
		Name:     hex.EncodeToString(src.blake3),
		Mode:     0o444,
		Size:     src.size,
		ModTime:  time.Unix(0, 0),
		Typeflag: tar.TypeReg,
		Format:   tar.FormatPAX,
	}
	if err := p.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write tar header: %w", err)
	}
	offset := p.ucount.n
	digest := blake3.New()
	n, err := io.Copy(p.tw, io.TeeReader(f, digest))
	if err != nil {
		return fmt.Errorf("write %s into pack: %w", src.srcPath, err)
	}
	if n != src.size {
		return fmt.Errorf("%s changed size to %d, indexed as %d", src.srcPath, n, src.size)
	}
	if !bytes.Equal(digest.Sum(nil), src.blake3) {
		return fmt.Errorf("%w: %s no longer matches %s", errContentDrift, src.srcPath, hex.EncodeToString(src.blake3))
	}
	p.members = append(p.members, packedMember{
		contentID: src.contentID,
		blake3:    src.blake3,
		offset:    offset,
		length:    src.size,
	})
	return nil
}

// compressedSize reports the compressed bytes emitted so far. zstd buffers
// internally, so this lags the true output until finish flushes — good
// enough for a target band, and it never over-reports.
func (p *packWriter) compressedSize() int64 { return p.ccount.n }

// finish flushes the tar and zstd streams and returns the pack key (BLAKE3
// of the compressed bytes), the compressed size, and the members.
func (p *packWriter) finish() ([]byte, int64, []packedMember, error) {
	if err := p.tw.Close(); err != nil {
		_ = p.zw.Close() // release the encoder even when the tar close fails
		return nil, 0, nil, fmt.Errorf("close tar: %w", err)
	}
	if err := p.zw.Close(); err != nil {
		return nil, 0, nil, fmt.Errorf("close zstd: %w", err)
	}
	return p.hasher.Sum(nil), p.ccount.n, p.members, nil
}

// close releases the tar writer and zstd encoder on the abandon path — a
// pack dropped before finish() (e.g. an add failure). klauspost's zstd
// encoder holds a background goroutine and buffers, so an unclosed writer
// leaks across a long run. Best-effort; only call when finish() did not run.
func (p *packWriter) close() {
	_ = p.tw.Close()
	_ = p.zw.Close()
}

// packedHandler satisfies the sealed handler interface.
var _ Handler = (*packedHandler)(nil)

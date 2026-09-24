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
// map name never collides across volumes. An encrypted destination keys
// the pack basename (namer.pack); the map keeps its run id, which recovery
// needs in order to replay in order.
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
// remote_packs, and advances the destination durability vector — as
// fingerprint-verified, never presence+size — only once every present
// content of the pair is fingerprint-verified (see advanceMethod). The
// offload gate then certifies a packed content through its pack's verified
// remote_packs row. Large files reuse the content-addressed object path and
// keep its per-object remote_objects record and fingerprint capture.
type packedHandler struct {
	contentPusher
}

func (h *packedHandler) TargetName() string { return h.dest.Name }

func (h *packedHandler) sealed() {}

func (h *packedHandler) Push(ctx context.Context, opts Options) (Report, error) {
	defer h.art.close()
	if h.vol.Name == ObjectsDirName || h.vol.Name == PacksDirName {
		rep := Report{Volume: h.vol.Name, Destination: h.dest.Name}
		rep.Verification.Method = VerifyMethodPresenceSize
		return rep, fmt.Errorf("volume %q: the name collides with the destination-root %s/ directory of packed destination %q — rename the volume or use a mirrored destination", h.vol.Name, h.vol.Name, h.dest.Name)
	}
	return pushThrough(ctx, h.target(), h, opts)
}

// packedOps is the packed translation of a plan: the large content routed
// to per-hash objects, the small content still to be packed (hash-sorted,
// so assembly is deterministic), and how much small content an earlier
// pack already holds. execute fills in the packs it landed, which seal
// records.
type packedOps struct {
	objects       objectUploads
	members       []store.PathDelta
	alreadyInPack int64
	packSize      int64

	packs      []landedPack
	placements []PlacementEntry
}

// landedPack is one pack execute landed: the rows to record, and the
// fingerprint the landing confirmed, nil while capture must read one.
type landedPack struct {
	write       store.PackWrite
	fingerprint *remoteChecksum
}

// preview reports the object side on rep.RcloneResult, exactly as the
// content-addressed preview does, and the pack side on rep.PackPreview.
func (o *packedOps) preview(rep *Report) {
	o.objects.preview(rep)
	rep.RcloneResult.Checked += o.alreadyInPack
	rep.PackPreview = previewPacks(o.members, o.packSize)
}

// previewPacks summarises the small (pack-routed) content without
// assembling a pack. small is already hash-sorted, so the size-band walk
// mirrors buildOnePack's — accumulate members until the running total
// reaches packSize, then close a pack — but over uncompressed sizes,
// yielding an estimated (not bounded) pack count (see PackPreview).
func previewPacks(small []store.PathDelta, packSize int64) PackPreview {
	p := PackPreview{SizeBand: packSize}
	for i := 0; i < len(small); {
		var packBytes int64
		for i < len(small) {
			packBytes += small[i].SizeBytes
			p.Contents++
			p.Bytes += small[i].SizeBytes
			i++
			if packBytes >= packSize {
				break
			}
		}
		p.Packs++
	}
	return p
}

// translate routes the delta's present content by dest.PackThreshold (see
// routeBySize) and splits the large side by upload record.
func (h *packedHandler) translate(ctx context.Context, p pushPlan) (*packedOps, error) {
	large, small, alreadyInPack, err := h.routeBySize(ctx, plannedUploads(p.delta))
	if err != nil {
		return nil, err
	}
	objects, err := h.splitRecorded(ctx, large)
	if err != nil {
		return nil, err
	}
	return &packedOps{objects: objects, members: small, alreadyInPack: alreadyInPack, packSize: h.dest.PackSize}, nil
}

// execute lands the large content as objects, then assembles and uploads
// the small content's packs. Nothing about the packs is recorded in the
// store yet: seal does that once the map and segment landed.
func (h *packedHandler) execute(ctx context.Context, rep *Report, runID int64, ops *packedOps) error {
	rep.RcloneResult.Checked += ops.alreadyInPack
	if err := h.uploadObjects(ctx, rep, runID, ops.objects); err != nil {
		return err
	}
	packs, placements, err := h.assembleAndUploadPacks(ctx, rep, runID, ops.members)
	if err != nil {
		return err
	}
	ops.packs, ops.placements = packs, placements
	return nil
}

// seal writes the placement map (the layout's landing evidence) and the
// manifest segment, then records the packs and reads their fingerprints.
// A pack_members row therefore always has its bytes, and their location,
// offsite, and a run that failed earlier left nothing here to re-pack.
func (h *packedHandler) seal(ctx context.Context, rep *Report, runID int64, p pushPlan, ops *packedOps) error {
	if err := h.uploadPlacementMap(ctx, ops.placements, runID); err != nil {
		return err
	}
	if err := h.uploadSegment(ctx, p.delta, runID); err != nil {
		return err
	}
	writes := make([]store.PackWrite, len(ops.packs))
	for i, lp := range ops.packs {
		writes[i] = lp.write
	}
	if err := h.store.InsertPacks(ctx, writes); err != nil {
		return fmt.Errorf("record packs for run %d: %w", runID, err)
	}
	h.capturePackFingerprints(ctx, rep, runID, ops.packs)
	return nil
}

// advanceMethod closes the durability seam: the vector advances as
// fingerprint-verified only once the whole (volume, destination) pair
// carries a verified fingerprint behind every present file content, and is
// held otherwise. That whole-state check is
// CountVolumeContentsPendingFingerprint, which counts present contents
// lacking a verified remote_objects (per-hash object) or remote_packs (pack
// member) fingerprint on this destination — so it covers packs and per-hash
// objects, the two artifacts that carry content bytes.
//
// The placement map and manifest segment are deliberately NOT in that
// pending set: they are re-derivable squirrel-written metadata that carry
// no scan-back fingerprint, and seal confirmed both landed at their
// expected size before this runs.
//
// Gating on the whole pending set rather than this run's writes is the
// friction-log F13 fix: a run that packs nothing no longer advances
// vacuously past an earlier still-pending pack, and a still-pending artifact
// anywhere in the pair holds the whole advance. `squirrel verify` fills a
// pending fingerprint later and re-attempts this advance itself (see the
// store upgrade), so the vector no longer stalls until the next
// content-writing sync.
func (h *packedHandler) advanceMethod(ctx context.Context, rep *Report, p pushPlan) (string, error) {
	pending, err := h.store.CountVolumeContentsPendingFingerprint(ctx, p.volumeID, h.dest.Name)
	if err != nil {
		return "", fmt.Errorf("count pending fingerprints for %s: %w", h.dest.Name, err)
	}
	if pending > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("destination %q: %d content(s) are not yet fingerprint-verified; the durability vector was not advanced — run `squirrel verify` to certify them", h.dest.Name, pending))
		return "", nil
	}
	return store.VerifyMethodFingerprint, nil
}

// capturePackFingerprints records a per-destination upload row for each
// landed pack, with the fingerprint its landing confirmed or, failing
// that, one read back over the shared capture surface on the packs/
// directory. Every pack exceeds the multipart threshold, so on s3 the
// composite ETag is read from the S3 API (never rclone's md5 slot, which
// is blank for a multipart object); other rclone backends read `rclone
// lsjson --hash`. A pack whose fingerprint could not be read stays pending
// (checksum NULL) with a warning — never a fabricated value. The whole-pair
// pending tally (CountVolumeContentsPendingFingerprint) is what
// advanceMethod gates the vector advance on, so this returns nothing.
func (h *packedHandler) capturePackFingerprints(ctx context.Context, rep *Report, runID int64, packs []landedPack) {
	targets := make([]captureTarget, 0, len(packs))
	for _, lp := range packs {
		w := lp.write
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
		if cs := lp.fingerprint; cs != nil {
			if err := h.store.SetRemotePackFingerprint(ctx, packID, h.dest.Name, cs.Algo, cs.Value, store.NowNs()); err != nil {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("record fingerprint for pack %s: %v", hex.EncodeToString(w.Pack.PackKey), err))
				continue
			}
			rep.Fingerprints++
			continue
		}
		targets = append(targets, captureTarget{
			name:  h.names().pack(w.Pack.PackKey),
			label: "pack",
			record: func(ctx context.Context, algo, value string) error {
				return h.store.SetRemotePackFingerprint(ctx, packID, h.dest.Name, algo, value, store.NowNs())
			},
		})
	}
	if len(targets) > 0 {
		h.art.capture(ctx, rep, PacksDirName, targets)
	}
}

// landed reports whether runID's placement map is at the destination.
// Every successful packed run uploads one (empty when no small content was
// new), so its absence means the recorded history belongs to a different
// layout — a mirror leaves no map, a content-addressed root leaves objects
// and segments but no packs/ map — or a wiped root.
func (h *packedHandler) landed(ctx context.Context, runID int64) (bool, error) {
	return h.art.exists(ctx, mapName(runID))
}

func (h *packedHandler) foreignHistory(runID int64) error {
	return fmt.Errorf("destination %q: the last successful sync (run %d) left no pack placement map at %s — its history is not packed (a mirror or content-addressed root); point the layout at a fresh destination or root, or (after wiping the remote root) run `squirrel destination reset %s`, instead of switching an existing one: %w", h.dest.Name, runID, h.art.where(mapName(runID)), h.dest.Name, ErrRefused)
}

// routeBySize splits the planned uploads by dest.PackThreshold: content at
// or above it becomes a per-hash object (large), content below it becomes a
// pack member (small) unless it is already packed — a pack is
// content-global and assembled once, so already-packed content is skipped
// and counted. small is returned sorted by hash so pack assembly is
// deterministic.
func (h *packedHandler) routeBySize(ctx context.Context, planned []store.PathDelta) (large, small []store.PathDelta, alreadyInPack int64, err error) {
	for _, d := range planned {
		if d.SizeBytes >= h.dest.PackThreshold {
			large = append(large, d)
			continue
		}
		packed, err := h.store.HasPackMember(ctx, d.ContentID)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("lookup pack member for %s: %w", d.Path, err)
		}
		if packed {
			alreadyInPack++
			continue
		}
		small = append(small, d)
	}
	slices.SortFunc(small, func(a, b store.PathDelta) int {
		return bytes.Compare(a.Blake3, b.Blake3)
	})
	return large, small, alreadyInPack, nil
}

// assembleAndUploadPacks bundles the sorted small content into tar.zst
// packs, staging and uploading one pack at a time so memory and disk stay
// bounded regardless of corpus size. It returns the packs to record
// locally and the placement entries for the run's map; nothing is recorded
// in the store here (push does that only after the map and segment land).
func (h *packedHandler) assembleAndUploadPacks(ctx context.Context, rep *Report, runID int64, small []store.PathDelta) ([]landedPack, []PlacementEntry, error) {
	level := zstdEncoderLevel(h.dest.ZstdLevel)
	var packs []landedPack
	var placements []PlacementEntry
	for i := 0; i < len(small); {
		pack, next, err := h.buildOnePack(small, i, level)
		if err != nil {
			return nil, nil, err
		}
		i = next
		fingerprint, err := h.uploadPack(ctx, runID, pack)
		if err != nil {
			return nil, nil, err
		}
		rep.RcloneResult.Transferred++
		rep.RcloneResult.Bytes += pack.compressedSize
		packs = append(packs, landedPack{write: pack.toWrite(runID), fingerprint: fingerprint})
		placements = append(placements, pack.placements()...)
	}
	return packs, placements, nil
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

// uploadPack lands one staged pack at packs/<pack-key hex>, confirmed at
// its compressed size and its key — the BLAKE3 of those bytes — then
// removes the temp file. Through a crypt overlay the reported size is the
// decrypted length, which is the compressed pack, so it compares directly.
func (h *packedHandler) uploadPack(ctx context.Context, runID int64, pack assembledPack) (*remoteChecksum, error) {
	defer func() { _ = os.Remove(pack.tmpPath) }()
	fingerprint, err := h.art.put(ctx, runID, h.packName(pack.key), pack.tmpPath, pack.compressedSize, pack.key)
	if err != nil {
		return nil, fmt.Errorf("upload pack %s: %w", hex.EncodeToString(pack.key), err)
	}
	return fingerprint, nil
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
	return putBytes(ctx, h.art, runID, mapName(runID), body, "placement map")
}

// packName is one pack under the destination-root packs/ directory. The
// basename is namer.pack's.
func (h *packedHandler) packName(packKey []byte) string {
	return path.Join(PacksDirName, h.names().pack(packKey))
}

// mapName is one run's placement map under the destination-root packs/
// directory.
func mapName(runID int64) string {
	return path.Join(PacksDirName, packMapPrefix+strconv.FormatInt(runID, 10))
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

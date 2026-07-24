package sync

import (
	"context"
	"fmt"
	"slices"
)

// fingerprintBatchSize caps how many freshly confirmed artifacts one
// lsjson invocation covers, bounding argv growth from the per-artifact
// --include filters.
const fingerprintBatchSize = 200

// captureTarget is one uploaded artifact (a content object or a pack)
// whose scan-back fingerprint this run records: name is its remote
// basename — the lowercase hex key the provider listing is keyed by —
// label names the artifact in warnings, and record persists a captured
// provider checksum against the artifact's upload row.
type captureTarget struct {
	name   string
	label  string
	record func(ctx context.Context, algo, value string) error
}

// captureScanBackFingerprints fills the pending provider checksum of every
// target uploaded during this run, read back from dest's underlying remote
// under dirName (ObjectsDirName or PacksDirName). s3 destinations read the
// ETag straight from the S3 API — the one surface exposing a multipart
// composite ETag, and every pack is multipart; every other backend reads
// `rclone lsjson --hash`. Capture problems are warnings, not failures: the
// upload is already confirmed and recorded, and `squirrel verify` fills any
// fingerprint left pending. The content-addressed object path and the
// packed pack path share this surface, differing only in the dirName they
// scan and the record closure each target carries.
func (h *contentPusher) captureScanBackFingerprints(ctx context.Context, rep *Report, dirName string, targets []captureTarget) {
	// A run that uploaded nothing new has nothing to fingerprint; return
	// before the s3 path would otherwise list the whole prefix (and possibly
	// warn on a transient failure) for an empty batch.
	if len(targets) == 0 {
		return
	}
	if h.dest.Type == "s3" {
		h.captureScanBackS3(ctx, rep, dirName, targets)
		return
	}
	h.captureScanBackRclone(ctx, rep, dirName, targets)
}

// captureScanBackRclone reads provider checksums via `rclone lsjson
// --hash`, batched into one invocation per chunk and scoped by --include
// filters so the backend hashes only this run's uploads.
func (h *contentPusher) captureScanBackRclone(ctx context.Context, rep *Report, dirName string, targets []captureTarget) {
	dirURI := underlyingDirURI(h.dest, dirName)
	types := captureHashTypes(h.dest)
	for batch := range slices.Chunk(targets, fingerprintBatchSize) {
		extra := checkersArgs(h.dest)
		for _, t := range batch {
			extra = append(extra, "--include", listedRemoteName(h.dest, t.name))
		}
		entries, err := h.rcl.listHashes(ctx, dirURI, types, extra...)
		if err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("fingerprint capture on %q failed: %v — checksums stay pending until `squirrel verify`", h.dest.Name, err))
			return
		}
		byName := make(map[string]map[string]string, len(entries))
		present := make(map[string]bool, len(entries))
		for _, e := range entries {
			name := listedPlainName(h.dest, e.Name)
			byName[name] = e.Hashes
			present[name] = true
		}
		h.recordScanBack(ctx, rep, batch, byName, present)
	}
}

// captureScanBackS3 reads every ETag under the destination's dirName prefix
// in one ListObjectsV2, presenting each ETag under the "md5" slot so the
// shared recording path classifies it via etagFlavor. A read failure leaves
// every fingerprint pending with a warning — capture is not on the
// durability critical path (the bytes are already confirmed), so `squirrel
// verify` is the backstop.
func (h *contentPusher) captureScanBackS3(ctx context.Context, rep *Report, dirName string, targets []captureTarget) {
	reader, err := newS3ETagReader(h.dest, dirName)
	if err == nil {
		var etags map[string]string
		etags, err = reader.objectETags(ctx)
		if err == nil {
			byName := make(map[string]map[string]string, len(etags))
			present := make(map[string]bool, len(etags))
			for name, etag := range etags {
				name = listedPlainName(h.dest, name)
				byName[name] = map[string]string{"md5": etag}
				present[name] = true
			}
			h.recordScanBack(ctx, rep, targets, byName, present)
			return
		}
	}
	rep.Warnings = append(rep.Warnings, fmt.Sprintf("fingerprint capture on %q failed: %v — checksums stay pending until `squirrel verify`", h.dest.Name, err))
}

// recordScanBack records the provider checksum for each target from the
// listing (byName, present). A target absent from the listing or exposing
// no usable checksum stays pending with a warning; the fingerprint is never
// fabricated. Each recorded checksum counts toward rep.Fingerprints.
func (h *contentPusher) recordScanBack(ctx context.Context, rep *Report, targets []captureTarget, byName map[string]map[string]string, present map[string]bool) {
	for _, t := range targets {
		if !present[t.name] {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s %s on %q: not yet returned by the remote listing; fingerprint stays pending", t.label, t.name, h.dest.Name))
			continue
		}
		cs, ok := extractChecksum(h.dest, byName[t.name])
		if !ok {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s %s on %q: remote exposes no usable checksum for this %s; fingerprint stays pending", t.label, t.name, h.dest.Name, t.label))
			continue
		}
		if err := t.record(ctx, cs.Algo, cs.Value); err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("record fingerprint for %s: %v", t.name, err))
			continue
		}
		rep.Fingerprints++
	}
}

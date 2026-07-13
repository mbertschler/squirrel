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
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// setupPackedFixture reuses the content-addressed fake-rclone harness with
// a packed destination: a small pack_threshold routes tiny files into packs
// while a larger file still lands as an object. The shim's copyto/lsjson
// playback is layout-agnostic, so the caFixture helpers work unchanged.
func setupPackedFixture(t *testing.T, threshold string) *caFixture {
	t.Helper()
	return setupCAFixture(t, fmt.Sprintf(`[destinations.offsite]
type   = "sftp"
host   = "remote.invalid"
user   = "u"
root   = "/data"
layout = "packed"
pack_threshold = %q
pack_size      = "512MiB"
zstd_level     = 3

[destinations.offsite.crypt]
password = "obscured-pw"
`, threshold), "/data")
}

func (f *caFixture) readPlacementMap(t *testing.T, runID int64) []PlacementEntry {
	t.Helper()
	data, err := os.ReadFile(f.remotePath(PacksDirName, fmt.Sprintf("map-%d", runID)))
	if err != nil {
		t.Fatalf("read placement map: %v", err)
	}
	var out []PlacementEntry
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var p PlacementEntry
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			t.Fatalf("parse placement line %q: %v", line, err)
		}
		out = append(out, p)
	}
	return out
}

// TestPackedSizeRouting: a file at/above the threshold lands as an object,
// a file below it lands inside a pack (not as an object), and the pack, its
// local rows, and the placement map all reflect the small file.
func TestPackedSizeRouting(t *testing.T) {
	f := setupPackedFixture(t, "16B")
	f.write(t, "big.bin", strings.Repeat("B", 64)) // >= threshold -> object
	f.write(t, "small.txt", "tiny")                // < threshold -> pack
	f.index(t)

	rep, err := f.sync(t)
	if err != nil {
		t.Fatalf("sync: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success", rep.Status)
	}
	// The large file is a plain object; the small file is not.
	if _, err := os.Stat(f.remotePath(ObjectsDirName, blake3Hex(strings.Repeat("B", 64)))); err != nil {
		t.Fatalf("large file did not land as an object: %v", err)
	}
	if _, err := os.Stat(f.remotePath(ObjectsDirName, blake3Hex("tiny"))); err == nil {
		t.Fatalf("small file wrongly landed as an object")
	}

	// A pack exists and the small content has a pack_members row.
	smallRow, err := f.store.GetByPath(context.Background(), f.volumeID(t), "small.txt")
	if err != nil {
		t.Fatalf("GetByPath small: %v", err)
	}
	m, err := f.store.GetPackMember(context.Background(), smallRow.ContentID)
	if err != nil {
		t.Fatalf("GetPackMember small: %v", err)
	}
	pack, err := f.store.GetPackByKey(context.Background(), mustPackKeyOf(t, f))
	if err != nil {
		t.Fatalf("GetPackByKey: %v", err)
	}
	if m.PackID != pack.ID || m.ByteLength != 4 {
		t.Fatalf("pack member = %+v, want pack %d length 4", m, pack.ID)
	}

	// The placement map has exactly the small content.
	placements := f.readPlacementMap(t, rep.RunID)
	if len(placements) != 1 || placements[0].Blake3 != blake3Hex("tiny") || placements[0].Length != 4 {
		t.Fatalf("placement map = %+v, want one tiny entry", placements)
	}

	// The large file kept the content-addressed per-object record.
	bigRow, err := f.store.GetByPath(context.Background(), f.volumeID(t), "big.bin")
	if err != nil {
		t.Fatalf("GetByPath big: %v", err)
	}
	if has, _ := f.store.HasRemoteObject(context.Background(), bigRow.ContentID, "offsite"); !has {
		t.Fatalf("large file has no remote_objects record")
	}
	if has, _ := f.store.HasPackMember(context.Background(), bigRow.ContentID); has {
		t.Fatalf("large file wrongly recorded as a pack member")
	}
}

// mustPackKeyOf returns the single pack key present at the destination,
// derived from the one pack file the fixture produced.
func mustPackKeyOf(t *testing.T, f *caFixture) []byte {
	t.Helper()
	entries, err := os.ReadDir(f.remotePath(PacksDirName))
	if err != nil {
		t.Fatalf("read packs dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "map-") {
			continue
		}
		key, err := hex.DecodeString(e.Name())
		if err != nil {
			t.Fatalf("pack name %q not hex: %v", e.Name(), err)
		}
		return key
	}
	t.Fatalf("no pack file at destination")
	return nil
}

// TestPackedDurabilityNotCertified: a successful packed push leaves the
// destination durability vector empty — packed content is not certified
// durable until PR 3 fingerprints packs. This pins the PR-2/PR-3 seam.
func TestPackedDurabilityNotCertified(t *testing.T) {
	f := setupPackedFixture(t, "1MiB")
	f.write(t, "small.txt", "tiny")
	f.index(t)
	rep, err := f.sync(t)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if rep.Verification.Verified() {
		t.Fatalf("packed push reported verified; must stay uncertified")
	}
	vector, err := f.store.ListDestinationRunIDs(context.Background(), f.volumeID(t), "offsite")
	if err != nil {
		t.Fatalf("ListDestinationRunIDs: %v", err)
	}
	if len(vector) != 0 {
		t.Fatalf("durability vector = %+v, want empty (no auto-certify for packed)", vector)
	}
}

// TestPackedAssemblyDeterministic: the same input set produces byte-for-byte
// identical pack bytes and the same pack key.
func TestPackedAssemblyDeterministic(t *testing.T) {
	srcs := writePackSources(t, map[string]string{
		"one":   "first content",
		"two":   "second content",
		"three": "third content",
	})
	key1, bytes1 := buildPackBytes(t, srcs)
	key2, bytes2 := buildPackBytes(t, srcs)
	if !bytes.Equal(key1, key2) {
		t.Fatalf("pack keys differ across builds: %x vs %x", key1, key2)
	}
	if !bytes.Equal(bytes1, bytes2) {
		t.Fatalf("pack bytes differ across builds (len %d vs %d)", len(bytes1), len(bytes2))
	}
}

// TestPackedStockTarRecoverable: a produced pack decompresses with the
// stock zstd + archive/tar readers, and every member's bytes round-trip by
// hash — recoverable without squirrel.
func TestPackedStockTarRecoverable(t *testing.T) {
	contents := map[string]string{
		"alpha": "the quick brown fox",
		"beta":  "jumps over the lazy dog",
		"gamma": strings.Repeat("z", 500),
	}
	srcs := writePackSources(t, contents)
	_, packBytes := buildPackBytes(t, srcs)

	zr, err := zstd.NewReader(bytes.NewReader(packBytes))
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	seen := 0
	wantByHash := map[string]string{}
	for _, c := range contents {
		wantByHash[blake3Hex(c)] = c
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read member %s: %v", hdr.Name, err)
		}
		if blake3Hex(string(data)) != hdr.Name {
			t.Fatalf("member %s bytes do not hash to its name", hdr.Name)
		}
		if want, ok := wantByHash[hdr.Name]; !ok || string(data) != want {
			t.Fatalf("unexpected member %s = %q", hdr.Name, data)
		}
		// Normalized header fields.
		if hdr.ModTime.Unix() != 0 || hdr.Uid != 0 || hdr.Gid != 0 {
			t.Fatalf("member %s header not normalized: %+v", hdr.Name, hdr)
		}
		seen++
	}
	if seen != len(contents) {
		t.Fatalf("recovered %d members, want %d", seen, len(contents))
	}
}

// TestPackedDRReplay: from the placement map alone, the recorded
// offset/length slice the uncompressed tar at the right member bytes —
// the disaster-recovery path a small external script would take.
func TestPackedDRReplay(t *testing.T) {
	f := setupPackedFixture(t, "1MiB")
	files := map[string]string{
		"docs/a.txt": "content of a",
		"docs/b.txt": "content of b, longer",
		"c.txt":      "c",
	}
	for name, content := range files {
		f.write(t, name, content)
	}
	f.index(t)
	rep, err := f.sync(t)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Path manifest maps path -> hash.
	pathToHash := map[string]string{}
	for _, e := range f.readSegment(t, rep.RunID) {
		if e.Status == store.StatusPresent {
			pathToHash[e.Path] = e.Blake3
		}
	}
	// Placement map maps hash -> (pack, offset, length).
	placeByHash := map[string]PlacementEntry{}
	for _, p := range f.readPlacementMap(t, rep.RunID) {
		placeByHash[p.Blake3] = p
	}

	for name, content := range files {
		hash, ok := pathToHash[name]
		if !ok {
			t.Fatalf("no manifest entry for %s", name)
		}
		place, ok := placeByHash[hash]
		if !ok {
			t.Fatalf("no placement for %s (%s)", name, hash)
		}
		// Fetch and decompress the pack, then slice by offset/length.
		packBytes, err := os.ReadFile(f.remotePath(PacksDirName, place.Pack))
		if err != nil {
			t.Fatalf("read pack %s: %v", place.Pack, err)
		}
		tarBytes := decompress(t, packBytes)
		if place.Offset+place.Length > int64(len(tarBytes)) {
			t.Fatalf("placement for %s exceeds tar length", name)
		}
		got := tarBytes[place.Offset : place.Offset+place.Length]
		if string(got) != content {
			t.Fatalf("DR replay of %s = %q, want %q", name, got, content)
		}
		if blake3Hex(string(got)) != hash {
			t.Fatalf("DR replay of %s does not hash to %s", name, hash)
		}
	}
}

// TestPackedDedupAcrossRuns: content already packed is not re-packed on a
// later run — a new path carrying already-packed content adds no pack, and
// its manifest line still maps onto the existing pack via the earlier map.
func TestPackedDedupAcrossRuns(t *testing.T) {
	f := setupPackedFixture(t, "1MiB")
	f.write(t, "a.txt", "shared-small")
	f.index(t)
	rep1, err := f.sync(t)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	firstMap := f.readPlacementMap(t, rep1.RunID)
	if len(firstMap) != 1 {
		t.Fatalf("first map = %+v, want one entry", firstMap)
	}

	f.write(t, "copy.txt", "shared-small")
	f.index(t)
	rep2, err := f.sync(t)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if rep2.RcloneResult.Transferred != 0 || rep2.RcloneResult.Checked != 1 {
		t.Fatalf("second sync transferred=%d checked=%d, want 0/1 (content already packed)",
			rep2.RcloneResult.Transferred, rep2.RcloneResult.Checked)
	}
	secondMap := f.readPlacementMap(t, rep2.RunID)
	if len(secondMap) != 0 {
		t.Fatalf("second map = %+v, want empty (nothing newly packed)", secondMap)
	}
	// The path still maps onto the shared content in the segment.
	entries := f.readSegment(t, rep2.RunID)
	if len(entries) != 1 || entries[0].Path != "copy.txt" || entries[0].Blake3 != blake3Hex("shared-small") {
		t.Fatalf("second segment = %+v, want copy.txt onto the shared hash", entries)
	}
}

// TestPackedWatermarkGuardRefusesContentAddressed: a destination whose last
// success looks content-addressed (a segment but no placement map) is
// refused rather than diffed against the wrong baseline.
func TestPackedWatermarkGuardRefusesContentAddressed(t *testing.T) {
	f := setupPackedFixture(t, "1MiB")
	f.write(t, "a.txt", "tiny")
	f.index(t)

	ctx := context.Background()
	// Seed a successful sync run with a manifest segment but no placement
	// map — the shape a content-addressed era leaves behind.
	caRun, err := f.store.BeginRun(ctx, store.RunKindSync, f.volumeID(t), "offsite", false)
	if err != nil {
		t.Fatalf("seed ca-era run: %v", err)
	}
	if err := f.store.FinishRun(ctx, caRun, store.RunStatusSuccess, "", 1); err != nil {
		t.Fatalf("finish ca-era run: %v", err)
	}
	segPath := f.remotePath("pics", ManifestDirName, fmt.Sprintf("run-%d", caRun))
	if err := os.MkdirAll(filepath.Dir(segPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(segPath, []byte{}, 0o644); err != nil {
		t.Fatalf("seed segment: %v", err)
	}

	rep, err := f.sync(t)
	if err == nil || !strings.Contains(err.Error(), "not packed") {
		t.Fatalf("expected packed-history refusal, got %v", err)
	}
	if rep.Status != store.RunStatusFailed {
		t.Fatalf("Status = %q, want failed", rep.Status)
	}
}

// TestPackedWatermarkGuardRefusesMirror: a mirror-era success (no segment,
// no map) is refused too.
func TestPackedWatermarkGuardRefusesMirror(t *testing.T) {
	f := setupPackedFixture(t, "1MiB")
	f.write(t, "a.txt", "tiny")
	f.index(t)

	ctx := context.Background()
	mirrorRun, err := f.store.BeginRun(ctx, store.RunKindSync, f.volumeID(t), "offsite", false)
	if err != nil {
		t.Fatalf("seed mirror run: %v", err)
	}
	if err := f.store.FinishRun(ctx, mirrorRun, store.RunStatusSuccess, "", 1); err != nil {
		t.Fatalf("finish mirror run: %v", err)
	}
	rep, err := f.sync(t)
	if err == nil || !strings.Contains(err.Error(), "not packed") {
		t.Fatalf("expected packed-history refusal, got %v", err)
	}
	if rep.Status != store.RunStatusFailed {
		t.Fatalf("Status = %q, want failed", rep.Status)
	}
}

// TestPackedDryRunRefused: the packed push has no dry-run mode yet.
func TestPackedDryRunRefused(t *testing.T) {
	f := setupPackedFixture(t, "1MiB")
	f.write(t, "a.txt", "tiny")
	f.index(t)
	_, err := RunPair(context.Background(), f.store, Tools{Rclone: f.rcl}, f.pair, Options{DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "dry-run") {
		t.Fatalf("expected dry-run refusal, got %v", err)
	}
}

// TestPackedRefusesReservedVolumeName: a volume named like a destination
// root directory (objects/ or packs/) is refused.
func TestPackedRefusesReservedVolumeName(t *testing.T) {
	f := setupPackedFixture(t, "1MiB")
	for _, name := range []string{ObjectsDirName, PacksDirName} {
		pair := Pair{
			Volume:      &config.Volume{Name: name, Path: t.TempDir()},
			Destination: f.pair.Destination,
		}
		_, err := RunPair(context.Background(), f.store, Tools{Rclone: f.rcl}, pair, Options{})
		if err == nil || !strings.Contains(err.Error(), "collides") {
			t.Fatalf("volume %q: expected collision refusal, got %v", name, err)
		}
	}
}

// --- pack-assembly unit helpers ---

// writePackSources materialises name->content into a temp dir and returns
// packSource records sorted by hash (the order assembly expects).
func writePackSources(t *testing.T, contents map[string]string) []packSource {
	t.Helper()
	dir := t.TempDir()
	var srcs []packSource
	for name, content := range contents {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := blake3.Sum256([]byte(content))
		key := make([]byte, 32)
		copy(key, sum[:])
		srcs = append(srcs, packSource{blake3: key, size: int64(len(content)), srcPath: p})
	}
	bytesSortSources(srcs)
	return srcs
}

func bytesSortSources(srcs []packSource) {
	for i := 1; i < len(srcs); i++ {
		for j := i; j > 0 && bytes.Compare(srcs[j-1].blake3, srcs[j].blake3) > 0; j-- {
			srcs[j-1], srcs[j] = srcs[j], srcs[j-1]
		}
	}
}

// buildPackBytes assembles one pack from srcs at zstd level 3 into memory,
// returning the pack key and the compressed bytes.
func buildPackBytes(t *testing.T, srcs []packSource) (key, packBytes []byte) {
	t.Helper()
	var buf bytes.Buffer
	pw, err := newPackWriter(&buf, zstdEncoderLevel(3))
	if err != nil {
		t.Fatalf("newPackWriter: %v", err)
	}
	for _, s := range srcs {
		if err := pw.add(s); err != nil {
			t.Fatalf("add %s: %v", s.srcPath, err)
		}
	}
	k, _, _, err := pw.finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	return k, buf.Bytes()
}

func decompress(t *testing.T, compressed []byte) []byte {
	t.Helper()
	zr, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	return out
}

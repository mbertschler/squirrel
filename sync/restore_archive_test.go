package sync

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/store"
)

// restore runs a restore of the fixture's "pics" volume from its offsite
// destination with the given options.
func (f *caFixture) restore(t *testing.T, opts RestoreOptions) (Report, error) {
	t.Helper()
	return Restore(context.Background(), f.store, f.rcl, f.pair.Volume, f.pair.Destination, opts)
}

// resetLog truncates the fake-rclone call log so a later assertion counts
// only the invocations made after the reset.
func (f *caFixture) resetLog(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(f.logPath, nil, 0o644); err != nil {
		t.Fatalf("reset shim log: %v", err)
	}
}

// copytoCount returns how many logged rclone invocations are a `copyto`
// whose argv mentions substr (e.g. "packs/" or "objects/").
func (f *caFixture) copytoCount(t *testing.T, substr string) int {
	t.Helper()
	data, err := os.ReadFile(f.logPath)
	if err != nil {
		t.Fatalf("read shim log: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, " copyto ") && strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

func mustReadRestored(t *testing.T, dir, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read restored %s: %v", rel, err)
	}
	return string(body)
}

// TestContentAddressedRestoreRoundTrip syncs a content-addressed volume,
// restores it into a scratch directory, and confirms every path's bytes
// come back — resolved from the local index and fetched per hash through
// the crypt overlay. The run is recorded as kind='restore'.
func TestContentAddressedRestoreRoundTrip(t *testing.T) {
	f := setupContentAddressedFixture(t)
	files := map[string]string{"a.txt": "alpha", "dir/b.txt": "beta", "dir/sub/c.txt": "gamma"}
	for name, content := range files {
		f.write(t, name, content)
	}
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}

	target := t.TempDir()
	rep, err := f.restore(t, RestoreOptions{ToPath: target})
	if err != nil {
		t.Fatalf("restore: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success", rep.Status)
	}
	if rep.RcloneResult.Transferred != int64(len(files)) || rep.RcloneResult.Errors != 0 {
		t.Fatalf("counts = transferred=%d errors=%d, want %d/0", rep.RcloneResult.Transferred, rep.RcloneResult.Errors, len(files))
	}
	for name, content := range files {
		if got := mustReadRestored(t, target, name); got != content {
			t.Fatalf("restored %s = %q, want %q", name, got, content)
		}
	}
	run, err := f.store.GetRun(context.Background(), rep.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Kind != store.RunKindRestore || run.Status != store.RunStatusSuccess {
		t.Fatalf("run = %+v, want a successful restore row", run)
	}
}

// TestPackedRestoreRoundTrip restores a packed destination holding both a
// standalone object (a large file) and several pack members (small files),
// and confirms the pack is fetched exactly once for all its members
// (batch-by-pack) while the object is fetched on its own.
func TestPackedRestoreRoundTrip(t *testing.T) {
	f := setupPackedFixture(t, "16B")
	files := map[string]string{
		"big.bin":  strings.Repeat("B", 64), // >= threshold -> object
		"s1.txt":   "one",                   // < threshold -> pack
		"s2.txt":   "two",                   // < threshold -> pack
		"d/s3.txt": "three",                 // < threshold -> pack
	}
	for name, content := range files {
		f.write(t, name, content)
	}
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}

	f.resetLog(t)
	target := t.TempDir()
	rep, err := f.restore(t, RestoreOptions{ToPath: target})
	if err != nil {
		t.Fatalf("restore: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusSuccess || rep.RcloneResult.Transferred != int64(len(files)) {
		t.Fatalf("rep = status=%q transferred=%d, want success/%d", rep.Status, rep.RcloneResult.Transferred, len(files))
	}
	for name, content := range files {
		if got := mustReadRestored(t, target, name); got != content {
			t.Fatalf("restored %s = %q, want %q", name, got, content)
		}
	}
	// One pack fetch serves all three small members; the object is fetched
	// on its own. Never a fetch per member.
	if n := f.copytoCount(t, PacksDirName+"/"); n != 1 {
		t.Fatalf("pack copyto count = %d, want 1 (batch by pack)", n)
	}
	if n := f.copytoCount(t, ObjectsDirName+"/"); n != 1 {
		t.Fatalf("object copyto count = %d, want 1", n)
	}
}

// TestArchiveRestoreVerifyOnExtract corrupts a landed object at the
// destination and confirms restore re-hashes on the way down: the mismatch
// is refused, the run is not clean, and the corrupt bytes never reach the
// target.
func TestArchiveRestoreVerifyOnExtract(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// Tamper with the stored object: same length, different bytes.
	obj := f.remoteBlob(ObjectsDirName, blake3Hex("alpha"))
	if err := os.WriteFile(obj, []byte("BRAVO"), 0o644); err != nil {
		t.Fatalf("corrupt object: %v", err)
	}

	target := t.TempDir()
	rep, err := f.restore(t, RestoreOptions{ToPath: target})
	if err == nil {
		t.Fatalf("expected a verification failure, got nil (rep=%+v)", rep)
	}
	if rep.Status != store.RunStatusPartial || rep.RcloneResult.Errors != 1 {
		t.Fatalf("rep = status=%q errors=%d, want partial/1", rep.Status, rep.RcloneResult.Errors)
	}
	if _, statErr := os.Stat(filepath.Join(target, "a.txt")); statErr == nil {
		t.Fatalf("corrupt bytes were written to the target despite the hash mismatch")
	}
}

// TestPackedRestoreVerifyOnExtractMember corrupts a single member's bytes
// inside a pack — leaving the tar framing and every other member intact — and
// confirms the pack-extraction hash check refuses only that content: its
// target file is never written, while the pack's other members still restore
// correctly. This is the pack-side counterpart of
// TestArchiveRestoreVerifyOnExtract (which covers the standalone-object path),
// exercising the readMember/extractMembers verify-before-write branch and the
// mid-stream continue that lets later members survive a failed one.
func TestPackedRestoreVerifyOnExtractMember(t *testing.T) {
	f := setupPackedFixture(t, "16B")
	files := map[string]string{
		"s1.txt": "one",   // < threshold -> pack
		"s2.txt": "two",   // < threshold -> pack (the member we corrupt)
		"s3.txt": "three", // < threshold -> pack
	}
	for name, content := range files {
		f.write(t, name, content)
	}
	f.index(t)
	rep, err := f.sync(t)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	// All three tiny files must share one pack, so this genuinely tests
	// "other members in the same pack still restore" rather than trivially
	// separate packs.
	place := map[string]PlacementEntry{}
	for _, p := range f.readPlacementMap(t, rep.RunID) {
		place[p.Blake3] = p
	}
	corrupt := place[blake3Hex("two")]
	if corrupt.Pack == "" {
		t.Fatalf("no placement entry for the corrupted content")
	}
	for _, c := range []string{"one", "three"} {
		if place[blake3Hex(c)].Pack != corrupt.Pack {
			t.Fatalf("expected %q to share the corrupted pack %s; got %+v", c, corrupt.Pack, place[blake3Hex(c)])
		}
	}

	// Flip the "two" member's bytes in the uncompressed tar (same length, so
	// every other member's offset is unchanged), then recompress and write the
	// pack back. The pack key no longer matches the tampered bytes, but restore
	// verifies per member, not per pack — so only s2.txt's re-hash must fail.
	packPath := f.remoteBlob(PacksDirName, corrupt.Pack)
	packBytes, err := os.ReadFile(packPath)
	if err != nil {
		t.Fatalf("read pack: %v", err)
	}
	tarBytes := decompress(t, packBytes)
	if corrupt.Offset+corrupt.Length > int64(len(tarBytes)) {
		t.Fatalf("placement offset/length exceed the decompressed tar")
	}
	for i := corrupt.Offset; i < corrupt.Offset+corrupt.Length; i++ {
		tarBytes[i] ^= 0xff
	}
	if err := os.WriteFile(packPath, compress(t, tarBytes), 0o644); err != nil {
		t.Fatalf("rewrite tampered pack: %v", err)
	}

	target := t.TempDir()
	rrep, rerr := f.restore(t, RestoreOptions{ToPath: target})
	if rerr == nil {
		t.Fatalf("expected a pack-member verification failure, got nil (rep=%+v)", rrep)
	}
	if rrep.Status != store.RunStatusPartial || rrep.RcloneResult.Errors != 1 {
		t.Fatalf("rep = status=%q errors=%d, want partial/1", rrep.Status, rrep.RcloneResult.Errors)
	}
	// The corrupt member's bytes never reach the target...
	if _, statErr := os.Stat(filepath.Join(target, "s2.txt")); statErr == nil {
		t.Fatalf("corrupt pack member was written to the target despite the hash mismatch")
	}
	// ...while the pack's other members restore correctly.
	if rrep.RcloneResult.Transferred != 2 {
		t.Fatalf("transferred = %d, want 2 (the uncorrupted members)", rrep.RcloneResult.Transferred)
	}
	for _, name := range []string{"s1.txt", "s3.txt"} {
		if got := mustReadRestored(t, target, name); got != files[name] {
			t.Fatalf("restored %s = %q, want %q", name, got, files[name])
		}
	}
}

// TestArchiveRestoreDedup confirms one fetch serves every path that
// references the same content, and all of them are written.
func TestArchiveRestoreDedup(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "shared")
	f.write(t, "copy/b.txt", "shared")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}

	f.resetLog(t)
	target := t.TempDir()
	rep, err := f.restore(t, RestoreOptions{ToPath: target})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if rep.RcloneResult.Transferred != 2 {
		t.Fatalf("transferred = %d, want 2 files written", rep.RcloneResult.Transferred)
	}
	for _, name := range []string{"a.txt", "copy/b.txt"} {
		if got := mustReadRestored(t, target, name); got != "shared" {
			t.Fatalf("restored %s = %q, want shared", name, got)
		}
	}
	if n := f.copytoCount(t, ObjectsDirName+"/"); n != 1 {
		t.Fatalf("object copyto count = %d, want 1 (one fetch for the shared content)", n)
	}
}

// TestArchiveRestoreInPlacePreservesOverwritten confirms an in-place
// archive restore moves any file it would overwrite under
// .squirrel-restore-history/run-<id>/ before writing the restored bytes —
// the local-side counterpart of sync's --backup-dir. Nothing is destroyed.
func TestArchiveRestoreInPlacePreservesOverwritten(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t) // writes the .squirrel-volume marker in-place
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// Diverge the live copy without re-indexing, so the index still maps
	// a.txt onto the "alpha" content.
	live := filepath.Join(f.pair.Volume.Path, "a.txt")
	if err := os.WriteFile(live, []byte("local-edit"), 0o644); err != nil {
		t.Fatalf("edit live copy: %v", err)
	}

	rep, err := f.restore(t, RestoreOptions{InPlace: true})
	if err != nil {
		t.Fatalf("in-place restore: %v (rep=%+v)", err, rep)
	}
	if got, _ := os.ReadFile(live); string(got) != "alpha" {
		t.Fatalf("live a.txt = %q, want the restored alpha", got)
	}
	backup := filepath.Join(f.pair.Volume.Path, RestoreHistoryDirName, "run-"+strconv.FormatInt(rep.RunID, 10), "a.txt")
	if got, err := os.ReadFile(backup); err != nil || string(got) != "local-edit" {
		t.Fatalf("preserved copy = %q err=%v, want the overwritten local-edit", got, err)
	}
}

// TestArchiveRestoreDryRun previews the work without fetching, writing, or
// recording a runs row.
func TestArchiveRestoreDryRun(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.write(t, "b.txt", "beta")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}

	f.resetLog(t)
	target := t.TempDir()
	rep, err := f.restore(t, RestoreOptions{ToPath: target, DryRun: true})
	if err != nil {
		t.Fatalf("dry-run restore: %v", err)
	}
	if rep.Status != store.RunStatusSuccess || rep.RunID != 0 {
		t.Fatalf("rep = status=%q run=%d, want success with no runs row", rep.Status, rep.RunID)
	}
	if rep.RcloneResult.Transferred != 2 {
		t.Fatalf("preview transferred = %d, want 2", rep.RcloneResult.Transferred)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("dry-run wrote %d entries into the target", len(entries))
	}
	if n := f.copytoCount(t, ""); n != 0 {
		t.Fatalf("dry-run issued %d copyto calls, want 0", n)
	}
}

// TestArchiveRestoreRefusesSymlinkTarget confirms restore refuses a
// destination path that already exists as a symlink rather than following
// it — following would let a write clobber a path outside the restore
// target. The symlink and its target are both left untouched.
func TestArchiveRestoreRefusesSymlinkTarget(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}

	target := t.TempDir()
	outside := filepath.Join(t.TempDir(), "precious.txt")
	if err := os.WriteFile(outside, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(target, "a.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	rep, err := f.restore(t, RestoreOptions{ToPath: target})
	if err == nil {
		t.Fatalf("expected a symlink refusal, got nil (rep=%+v)", rep)
	}
	if rep.RcloneResult.Errors != 1 || rep.Status != store.RunStatusPartial {
		t.Fatalf("rep = status=%q errors=%d, want partial/1", rep.Status, rep.RcloneResult.Errors)
	}
	if got, _ := os.ReadFile(outside); string(got) != "precious" {
		t.Fatalf("outside file clobbered through the symlink: %q", got)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was replaced or removed: mode=%v err=%v", fi.Mode(), err)
	}
}

// TestArchiveRestorePreservesMtime confirms a restored file is stamped with
// the mtime the index recorded, matching the mirror pull, so a follow-up
// index sees no spurious drift.
func TestArchiveRestorePreservesMtime(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	want := time.Unix(1_600_000_000, 0)
	src := filepath.Join(f.pair.Volume.Path, "a.txt")
	if err := os.Chtimes(src, want, want); err != nil {
		t.Fatalf("set source mtime: %v", err)
	}
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}

	target := t.TempDir()
	if _, err := f.restore(t, RestoreOptions{ToPath: target}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	fi, err := os.Stat(filepath.Join(target, "a.txt"))
	if err != nil {
		t.Fatalf("stat restored: %v", err)
	}
	if fi.ModTime().UnixNano() != want.UnixNano() {
		t.Fatalf("restored mtime = %v, want the index-recorded %v", fi.ModTime(), want)
	}
}

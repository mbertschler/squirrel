package store

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

// TestApplyIndexBatchMatchesPerOpRecompute is the equivalence guarantee
// behind ApplyIndexBatch: a batched apply of N file ops must produce the
// same folder Merkle (shallow + deep) at every folder as N independent
// per-op Upsert / TouchSeen calls would. Without this, the dedup'd
// closure recompute could silently diverge from the per-op behaviour as
// the schema evolves.
//
// Both phases — first-write (Upsert) and steady-state observation
// (TouchSeen) — are exercised because they take different paths through
// ApplyIndexBatch (BatchOpUpsert vs BatchOpTouchSeen) but share the
// closure recompute.
func TestApplyIndexBatchMatchesPerOpRecompute(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	perOpID := makeVolume(t, s, "/per-op")
	batchID := makeVolume(t, s, "/batch")
	perOpRun := makeRun(t, s, perOpID)
	batchRun := makeRun(t, s, batchID)

	// Tree shared by both volumes — deliberately mixes:
	//   - a root-level file              (folder "")
	//   - a file in a single-depth folder (folder "a")
	//   - two siblings in a nested folder (folder "a/b", common parent)
	//   - a file in a separate subtree    (folder "c")
	// So the closure walk exercises shared ancestors ("", "a") and a
	// disjoint subtree ("c") at once.
	files := []struct {
		path string
		hash byte
	}{
		{"f0", 0x10},
		{"a/f1", 0x21},
		{"a/b/f2", 0x32},
		{"a/b/f3", 0x33},
		{"c/f4", 0x44},
	}

	mkRow := func(volumeID, runID int64, path string, hash byte) FileRow {
		return FileRow{
			VolumeID: volumeID, Path: path, Blake3: digest(hash),
			SizeBytes: 100, MtimeNs: 1, Status: StatusPresent,
			FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: 1,
		}
	}

	for _, f := range files {
		if err := s.Upsert(ctx, mkRow(perOpID, perOpRun, f.path, f.hash), nil); err != nil {
			t.Fatalf("per-op Upsert %s: %v", f.path, err)
		}
	}

	batch := make([]IndexBatchEntry, 0, len(files))
	for _, f := range files {
		batch = append(batch, IndexBatchEntry{
			Kind: BatchOpUpsert,
			Row:  mkRow(batchID, batchRun, f.path, f.hash),
		})
	}
	if err := s.ApplyIndexBatch(ctx, batchRun, batch); err != nil {
		t.Fatalf("ApplyIndexBatch upserts: %v", err)
	}

	folderPaths := []string{"", "a", "a/b", "c"}
	assertSameFolderHashes(t, s, ctx, perOpID, batchID, folderPaths, "after first-write")

	// Phase 2: a second run re-observes the same content. Per-op TouchSeen
	// vs batched TouchSeen must again converge on identical folder hashes.
	perOpRun2 := makeRun(t, s, perOpID)
	batchRun2 := makeRun(t, s, batchID)
	for _, f := range files {
		if err := s.TouchSeen(ctx, perOpID, f.path, perOpRun2); err != nil {
			t.Fatalf("per-op TouchSeen %s: %v", f.path, err)
		}
	}
	batch = batch[:0]
	for _, f := range files {
		row := mkRow(batchID, batchRun2, f.path, f.hash)
		batch = append(batch, IndexBatchEntry{Kind: BatchOpTouchSeen, Row: row})
	}
	if err := s.ApplyIndexBatch(ctx, batchRun2, batch); err != nil {
		t.Fatalf("ApplyIndexBatch touch_seens: %v", err)
	}
	assertSameFolderHashes(t, s, ctx, perOpID, batchID, folderPaths, "after re-observe")
}

// assertSameFolderHashes fails the test if any of the listed folder paths
// has divergent shallow_blake3 or deep_blake3 between the two volumes.
func assertSameFolderHashes(t *testing.T, s *Store, ctx context.Context, volA, volB int64, paths []string, phase string) {
	t.Helper()
	for _, p := range paths {
		fa, err := s.GetFolderByPath(ctx, volA, p)
		if err != nil {
			t.Fatalf("%s: GetFolderByPath A %q: %v", phase, p, err)
		}
		fb, err := s.GetFolderByPath(ctx, volB, p)
		if err != nil {
			t.Fatalf("%s: GetFolderByPath B %q: %v", phase, p, err)
		}
		if !bytes.Equal(fa.ShallowBlake3, fb.ShallowBlake3) {
			t.Errorf("%s: folder %q shallow mismatch: per-op=%x batch=%x", phase, p, fa.ShallowBlake3, fb.ShallowBlake3)
		}
		if !bytes.Equal(fa.DeepBlake3, fb.DeepBlake3) {
			t.Errorf("%s: folder %q deep mismatch: per-op=%x batch=%x", phase, p, fa.DeepBlake3, fb.DeepBlake3)
		}
	}
}

package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"path/filepath"
	"sort"
	"testing"

	"github.com/zeebo/blake3"
)

// TestComputeShallowHashEmpty pins the "well-defined empty input" rule:
// a folder with zero live files has a deterministic 32-byte digest, not
// NULL. This is what lets a freshly created (or just-emptied) folder
// still participate in the Merkle walk without a special case.
func TestComputeShallowHashEmpty(t *testing.T) {
	got := ComputeShallowHash(nil)
	if len(got) != 32 {
		t.Fatalf("empty shallow hash length = %d, want 32", len(got))
	}
	// Two calls must agree byte-for-byte — the empty hash is not
	// randomised, it's a function of the key alone.
	again := ComputeShallowHash(nil)
	if !bytes.Equal(got, again) {
		t.Fatalf("empty shallow hash unstable across calls")
	}
}

// TestComputeShallowHashIndependentReimpl is the hash-spec round-trip
// acceptance test. A small recomputation that follows the canonical
// input format (keyed BLAKE3 over big-endian length-prefixed names and
// raw 32-byte digests) must agree with ComputeShallowHash on the same
// inputs. If the in-store canon ever drifts, this test fails before
// any sync protocol drift can.
func TestComputeShallowHashIndependentReimpl(t *testing.T) {
	entries := []ShallowEntry{
		{Name: "alpha.jpg", Blake3: digest(0xaa)},
		{Name: "bravo.txt", Blake3: digest(0xbb)},
		{Name: "charlie.md", Blake3: digest(0xcc)},
	}
	got := ComputeShallowHash(entries)

	// Independent reimplementation: derive the key from the same
	// context string and feed the canonical bytes.
	var key [32]byte
	blake3.DeriveKey(folderHashContext, nil, key[:])
	h, err := blake3.NewKeyed(key[:])
	if err != nil {
		t.Fatalf("NewKeyed: %v", err)
	}
	var lenBuf [4]byte
	for _, e := range entries {
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(e.Name)))
		h.Write(lenBuf[:])
		h.Write([]byte(e.Name))
		h.Write(e.Blake3)
	}
	want := h.Sum(nil)

	if !bytes.Equal(got, want) {
		t.Fatalf("shallow hash diverges:\n got=%x\nwant=%x", got, want)
	}
}

// TestComputeDeepHashNameSensitive verifies that the deep hash folds the
// subfolder name in addition to its deep_blake3. Two volumes with the
// same direct files but a renamed subfolder must yield different deep
// hashes — otherwise a rename of a whole subtree would not propagate
// through plan negotiation and the rename would be silently dropped.
func TestComputeDeepHashNameSensitive(t *testing.T) {
	shallow := ComputeShallowHash(nil)
	childDeep := digest(0x77)
	one := ComputeDeepHash(shallow, []ChildFolder{{Name: "before", DeepBlake3: childDeep}})
	two := ComputeDeepHash(shallow, []ChildFolder{{Name: "after", DeepBlake3: childDeep}})
	if bytes.Equal(one, two) {
		t.Fatalf("deep hash is not subfolder-name sensitive — rename within a tree would not invalidate ancestors")
	}
}

// TestUpsertSeedsRootFolder verifies the basic plumbing: a fresh volume
// with one file upserted at the root produces a folder row with path=""
// whose shallow_blake3 agrees with a from-scratch recomputation over
// the live files.
func TestUpsertSeedsRootFolder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run := makeRun(t, s, vID)

	d := digest(0x42)
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "photo.jpg", Blake3: d,
		SizeBytes: 1, MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	root, err := s.GetFolderByPath(ctx, vID, "")
	if err != nil {
		t.Fatalf("GetFolderByPath root: %v", err)
	}
	want := ComputeShallowHash([]ShallowEntry{{Name: "photo.jpg", Blake3: d}})
	if !bytes.Equal(root.ShallowBlake3, want) {
		t.Fatalf("root shallow_blake3 mismatch:\n got=%x\nwant=%x", root.ShallowBlake3, want)
	}
	if len(root.DeepBlake3) != 32 {
		t.Fatalf("root deep_blake3 = %x, want 32 bytes", root.DeepBlake3)
	}
	if !root.LastChangedRunID.Valid || root.LastChangedRunID.Int64 != run {
		t.Fatalf("root last_changed_run_id = %+v, want %d", root.LastChangedRunID, run)
	}
}

// TestUpsertInvalidatesExactlyAncestors writes a file deep in a tree, then
// snapshots every folder digest. A second write under a sibling branch
// must change the touched branch's ancestors (its own folder + parents up
// to root) and leave the untouched branch's digests stable.
func TestUpsertInvalidatesExactlyAncestors(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run1 := makeRun(t, s, vID)

	// Seed three branches.
	seed := []struct {
		path  string
		hash  byte
	}{
		{"a/b/c/leaf-a.txt", 0x01},
		{"a/d/leaf-d.txt", 0x02},
		{"x/y/leaf-y.txt", 0x03},
	}
	for _, e := range seed {
		if err := s.Upsert(ctx, FileRow{
			VolumeID: vID, Path: e.path, Blake3: digest(e.hash),
			SizeBytes: 1, MtimeNs: 1, Status: StatusPresent,
			FirstSeenRunID: run1, LastSeenRunID: run1, IndexedAtNs: 1,
		}, nil); err != nil {
			t.Fatalf("seed %s: %v", e.path, err)
		}
	}

	beforeShallow := snapshotFolderHashes(t, s, vID, true)
	beforeDeep := snapshotFolderHashes(t, s, vID, false)

	// New file under x/y. Only x/y, x, and root should see a deep hash
	// change. x/y's shallow also changes; siblings under a/ stay put.
	run2 := makeRun(t, s, vID)
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "x/y/new.txt", Blake3: digest(0x04),
		SizeBytes: 1, MtimeNs: 2, Status: StatusPresent,
		FirstSeenRunID: run2, LastSeenRunID: run2, IndexedAtNs: 2,
	}, nil); err != nil {
		t.Fatalf("Upsert new: %v", err)
	}

	afterShallow := snapshotFolderHashes(t, s, vID, true)
	afterDeep := snapshotFolderHashes(t, s, vID, false)

	wantShallowChanged := map[string]bool{"x/y": true}
	wantDeepChanged := map[string]bool{"": true, "x": true, "x/y": true}

	for p, before := range beforeShallow {
		after := afterShallow[p]
		changed := !bytes.Equal(before, after)
		if changed != wantShallowChanged[p] {
			t.Errorf("shallow %q: changed=%v, want %v", p, changed, wantShallowChanged[p])
		}
	}
	for p, before := range beforeDeep {
		after := afterDeep[p]
		changed := !bytes.Equal(before, after)
		if changed != wantDeepChanged[p] {
			t.Errorf("deep %q: changed=%v, want %v", p, changed, wantDeepChanged[p])
		}
	}
}

// TestRenameInSameFolderInvalidates is the rename test from the issue's
// acceptance: a file's content stays the same but its name changes, so
// the file row's blake3 is identical but its (name, blake3) tuple is
// different. The shallow hash of the affected folder must change
// (otherwise the sync walk would skip the subtree and the rename would
// not propagate).
func TestRenameInSameFolderInvalidates(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run := makeRun(t, s, vID)
	d := digest(0x55)

	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "pictures/cat.jpg", Blake3: d,
		SizeBytes: 1, MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert original: %v", err)
	}

	pictures, err := s.GetFolderByPath(ctx, vID, "pictures")
	if err != nil {
		t.Fatalf("GetFolderByPath: %v", err)
	}
	beforeShallow := append([]byte(nil), pictures.ShallowBlake3...)
	beforeDeepRoot := snapshotFolderHashes(t, s, vID, false)[""]

	// Rename in place: same blake3, new name. Mark the old name as
	// missing (the indexer would do this on the next walk) and upsert
	// the new name.
	if err := upsertMissing(ctx, s, vID, "pictures/cat.jpg", run); err != nil {
		t.Fatalf("mark old name missing: %v", err)
	}
	run2 := makeRun(t, s, vID)
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "pictures/dog.jpg", Blake3: d,
		SizeBytes: 1, MtimeNs: 2, Status: StatusPresent,
		FirstSeenRunID: run2, LastSeenRunID: run2, IndexedAtNs: 2,
	}, nil); err != nil {
		t.Fatalf("Upsert renamed: %v", err)
	}

	afterFolder, err := s.GetFolderByPath(ctx, vID, "pictures")
	if err != nil {
		t.Fatalf("GetFolderByPath after: %v", err)
	}
	if bytes.Equal(beforeShallow, afterFolder.ShallowBlake3) {
		t.Fatalf("shallow_blake3 unchanged after rename — sync walk would skip the subtree")
	}
	afterDeepRoot := snapshotFolderHashes(t, s, vID, false)[""]
	if bytes.Equal(beforeDeepRoot, afterDeepRoot) {
		t.Fatalf("root deep_blake3 unchanged after rename — ancestor chain did not propagate")
	}
}

// TestEmptyFolderHasWellDefinedHash exercises the "empty folder is not
// NULL" rule: a folder created (via getOrCreateFolderTx ancestor chain)
// but containing no direct files still has a 32-byte shallow_blake3 set
// — specifically the keyed hash of the empty input.
func TestEmptyFolderHasWellDefinedHash(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run := makeRun(t, s, vID)

	// File at depth 2 — creates intermediate folder "a" with no direct files.
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "a/b/leaf.txt", Blake3: digest(0x77),
		SizeBytes: 1, MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	a, err := s.GetFolderByPath(ctx, vID, "a")
	if err != nil {
		t.Fatalf("GetFolderByPath a: %v", err)
	}
	want := ComputeShallowHash(nil)
	if !bytes.Equal(a.ShallowBlake3, want) {
		t.Fatalf("empty intermediate folder shallow_blake3:\n got=%x\nwant=%x", a.ShallowBlake3, want)
	}
	if len(a.DeepBlake3) != 32 {
		t.Fatalf("empty intermediate folder has no deep hash")
	}
}

// TestDriftDetectionFoundation simulates corruption on the receiver:
// directly poking one file row's content shows up as a deep_blake3 mismatch
// against an independent recomputation. This is the cheap full-volume
// integrity check #17 will later schedule.
func TestDriftDetectionFoundation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run := makeRun(t, s, vID)

	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "deep/nested/leaf.txt", Blake3: digest(0xaa),
		SizeBytes: 1, MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	rootBefore, err := s.GetFolderByPath(ctx, vID, "")
	if err != nil {
		t.Fatalf("GetFolderByPath root: %v", err)
	}
	deepBefore := append([]byte(nil), rootBefore.DeepBlake3...)

	// Corrupt the receiver-side stored hash directly (bypassing every
	// API guarantee — this mimics a disk-level flip rather than any
	// legitimate write path). Drift detection should notice that the
	// stored deep_blake3 no longer equals a fresh recomputation.
	corrupt := append([]byte(nil), deepBefore...)
	corrupt[0] ^= 0xff
	if _, err := s.db.ExecContext(ctx,
		`UPDATE folders SET deep_blake3 = ? WHERE volume_id = ? AND path = ''`,
		corrupt, vID); err != nil {
		t.Fatalf("corrupt root deep_blake3: %v", err)
	}

	fresh, err := freshRecomputeDeep(ctx, s, vID, "")
	if err != nil {
		t.Fatalf("freshRecomputeDeep: %v", err)
	}
	if bytes.Equal(corrupt, fresh) {
		t.Fatalf("recompute hashed to corrupted value — test fixture too weak")
	}
	if !bytes.Equal(fresh, deepBefore) {
		t.Fatalf("fresh recompute drifted from original — not a drift detector at all")
	}
}

// TestMigrateV7ToV8RoundTrip is the migration acceptance test: a v7
// fixture DB with files at several paths must end up at v8 with (a)
// every file row preserved, (b) folders populated for every distinct
// directory + ancestor + root, and (c) every folder's stored hashes
// agreeing with a from-scratch recompute over the live files.
func TestMigrateV7ToV8RoundTrip(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	for _, q := range v7Fixture() {
		if _, err := rawDB.Exec(q); err != nil {
			rawDB.Close()
			t.Fatalf("v7 DDL %q: %v", q, err)
		}
	}
	files := []struct {
		path string
		hash byte
	}{
		{"top.txt", 0x10},
		{"a/file1.txt", 0x11},
		{"a/file2.txt", 0x12},
		{"a/b/inner.txt", 0x13},
		{"x/y/z/deep.txt", 0x14},
	}
	for _, f := range files {
		if _, err := rawDB.Exec(
			`INSERT INTO files (volume_id, path, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns)
			 VALUES (1, ?, ?, 1, 1, 'present', 1, 1, 1)`, f.path, digest(f.hash),
		); err != nil {
			rawDB.Close()
			t.Fatalf("seed %s: %v", f.path, err)
		}
	}
	rawDB.Close()

	s, err := OpenWithOptions(dsn, OpenOptions{NodeName: "nas"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if v, _ := s.CurrentSchemaVersion(ctx); v != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", v, SchemaVersion)
	}

	// (a) every file row preserved — assert by counting and by spot-checking
	// a leaf path goes through GetByPath cleanly.
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&n); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if n != len(files) {
		t.Fatalf("files count = %d, want %d", n, len(files))
	}
	row, err := s.GetByPath(ctx, 1, "x/y/z/deep.txt")
	if err != nil {
		t.Fatalf("GetByPath deep file: %v", err)
	}
	if !bytes.Equal(row.Blake3, digest(0x14)) {
		t.Fatalf("deep file blake3 mismatch")
	}

	// (b) folders for every distinct directory + ancestor + root.
	wantFolders := []string{"", "a", "a/b", "x", "x/y", "x/y/z"}
	for _, p := range wantFolders {
		if _, err := s.GetFolderByPath(ctx, 1, p); err != nil {
			t.Fatalf("folder %q missing: %v", p, err)
		}
	}

	// (c) every folder's stored hashes agree with a from-scratch recompute.
	folders, err := listFolders(ctx, s, 1)
	if err != nil {
		t.Fatalf("listFolders: %v", err)
	}
	for _, f := range folders {
		shallow, err := freshRecomputeShallow(ctx, s, 1, f.Path)
		if err != nil {
			t.Fatalf("freshRecomputeShallow %q: %v", f.Path, err)
		}
		if !bytes.Equal(f.ShallowBlake3, shallow) {
			t.Fatalf("folder %q shallow mismatch:\n stored=%x\n fresh =%x", f.Path, f.ShallowBlake3, shallow)
		}
		deep, err := freshRecomputeDeep(ctx, s, 1, f.Path)
		if err != nil {
			t.Fatalf("freshRecomputeDeep %q: %v", f.Path, err)
		}
		if !bytes.Equal(f.DeepBlake3, deep) {
			t.Fatalf("folder %q deep mismatch:\n stored=%x\n fresh =%x", f.Path, f.DeepBlake3, deep)
		}
	}
}

// --- helpers ---

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// upsertMissing flips the live row at relPath to 'missing' via Upsert,
// preserving the same blake3. Simulates the indexer's MarkMissing for a
// single path. Used by the rename test to free the (folder, name) slot
// before reusing the same hash under a different name.
func upsertMissing(ctx context.Context, s *Store, volumeID int64, relPath string, runID int64) error {
	row, err := s.GetByPath(ctx, volumeID, relPath)
	if err != nil {
		return err
	}
	// MarkMissing won't help here (it acts on whole-run staleness). Use
	// a raw UPDATE inside the same supersede contract: blake3 stays, status
	// flips to missing. The folder hash recompute lives behind TouchSeen /
	// Upsert; we trigger one explicitly to mirror what the real indexer
	// would do after marking absent.
	folderPath, name := splitFilePath(relPath)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var folderID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM folders WHERE volume_id = ? AND path = ?`,
		volumeID, folderPath).Scan(&folderID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE files SET status = 'missing', last_seen_run_id = ?
		 WHERE folder_id = ? AND name = ? AND blake3 = ?`,
		runID, folderID, name, row.Blake3); err != nil {
		return err
	}
	if err := recomputeFolderAndAncestors(ctx, tx, folderID, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// snapshotFolderHashes returns a map from folder path to either its
// shallow_blake3 (when shallow=true) or deep_blake3.
func snapshotFolderHashes(t *testing.T, s *Store, volumeID int64, shallow bool) map[string][]byte {
	t.Helper()
	col := "deep_blake3"
	if shallow {
		col = "shallow_blake3"
	}
	rows, err := s.db.Query(
		`SELECT path, `+col+` FROM folders WHERE volume_id = ?`, volumeID)
	if err != nil {
		t.Fatalf("snapshot %s: %v", col, err)
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var p string
		var h []byte
		if err := rows.Scan(&p, &h); err != nil {
			t.Fatalf("scan snapshot row: %v", err)
		}
		out[p] = h
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	return out
}

// freshRecomputeShallow re-derives a folder's shallow_blake3 from the
// live files in the DB without consulting the stored value. Used by the
// migration round-trip test and drift test.
func freshRecomputeShallow(ctx context.Context, s *Store, volumeID int64, folderPath string) ([]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.name, f.blake3 FROM files f
		JOIN folders fo ON fo.id = f.folder_id
		WHERE fo.volume_id = ? AND fo.path = ? AND f.status = 'present'
		ORDER BY f.name`,
		volumeID, folderPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []ShallowEntry
	for rows.Next() {
		var e ShallowEntry
		if err := rows.Scan(&e.Name, &e.Blake3); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ComputeShallowHash(entries), nil
}

// freshRecomputeDeep re-derives a folder's deep_blake3 by recursing into
// each direct child folder. The recursion ignores stored deep_blake3
// values entirely so corruption surfaces here.
func freshRecomputeDeep(ctx context.Context, s *Store, volumeID int64, folderPath string) ([]byte, error) {
	shallow, err := freshRecomputeShallow(ctx, s, volumeID, folderPath)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT child.path FROM folders child
		JOIN folders parent ON parent.id = child.parent_id
		WHERE parent.volume_id = ? AND parent.path = ?
		ORDER BY child.path`, volumeID, folderPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var childPaths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		childPaths = append(childPaths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var children []ChildFolder
	for _, cp := range childPaths {
		childDeep, err := freshRecomputeDeep(ctx, s, volumeID, cp)
		if err != nil {
			return nil, err
		}
		children = append(children, ChildFolder{Name: folderName(cp), DeepBlake3: childDeep})
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	return ComputeDeepHash(shallow, children), nil
}

func listFolders(ctx context.Context, s *Store, volumeID int64) ([]Folder, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, volume_id, parent_id, path, shallow_blake3, deep_blake3, last_changed_run_id
		 FROM folders WHERE volume_id = ? ORDER BY path`, volumeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.VolumeID, &f.ParentID, &f.Path, &f.ShallowBlake3, &f.DeepBlake3, &f.LastChangedRunID); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// v7Fixture returns the DDL + seed for a v7 database used by the
// migration round-trip test. Kept inline rather than reusing the v6
// fixture because the v6 test seeds different rows and asserting on
// those would obscure the v7→v8 contract.
func v7Fixture() []string {
	return []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE nodes (
			id                     INTEGER PRIMARY KEY,
			name                   TEXT NOT NULL UNIQUE,
			endpoint               TEXT,
			public_key_fingerprint TEXT
		)`,
		`CREATE TABLE runs (
			id            INTEGER PRIMARY KEY,
			kind          TEXT NOT NULL CHECK (kind IN ('index','sync','restore','audit')),
			volume_id     INTEGER REFERENCES volumes(id),
			destination   TEXT,
			started_at_ns INTEGER NOT NULL,
			ended_at_ns   INTEGER,
			status        TEXT NOT NULL CHECK (status IN ('running','success','failed','partial')),
			error         TEXT,
			file_count    INTEGER NOT NULL DEFAULT 0,
			peer_node_id      INTEGER REFERENCES nodes(id),
			correlated_run_id INTEGER,
			CHECK (
				(kind IN ('index','audit') AND destination IS NULL) OR
				(kind IN ('sync','restore') AND destination IS NOT NULL AND destination != '')
			)
		)`,
		`CREATE TABLE peer_sync_state (
			volume_id          INTEGER NOT NULL REFERENCES volumes(id),
			peer_node_id       INTEGER NOT NULL REFERENCES nodes(id),
			last_shared_run_id INTEGER,
			last_synced_at     INTEGER NOT NULL,
			PRIMARY KEY (volume_id, peer_node_id)
		)`,
		`CREATE TABLE files (
			volume_id         INTEGER NOT NULL REFERENCES volumes(id),
			path              TEXT NOT NULL,
			blake3            BLOB NOT NULL CHECK (length(blake3) = 32),
			size_bytes        INTEGER NOT NULL,
			mtime_ns          INTEGER NOT NULL,
			status            TEXT NOT NULL CHECK (status IN ('present','missing','superseded')),
			first_seen_run_id INTEGER NOT NULL REFERENCES runs(id),
			last_seen_run_id  INTEGER NOT NULL REFERENCES runs(id),
			indexed_at_ns     INTEGER NOT NULL,
			source_node_id    INTEGER REFERENCES nodes(id),
			source_run_id     INTEGER REFERENCES runs(id),
			PRIMARY KEY (volume_id, path, blake3)
		)`,
		`INSERT INTO schema_version (version) VALUES (7)`,
		`INSERT INTO nodes (name, endpoint, public_key_fingerprint) VALUES ('nas', NULL, NULL)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'photos', '/photos')`,
		`INSERT INTO runs (id, kind, volume_id, started_at_ns, status, file_count)
		 VALUES (1, 'index', 1, 1, 'success', 5)`,
	}
}


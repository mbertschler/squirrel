package store

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func digest(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// writeSchemaVersion forces a schema version row. Used to construct DB
// states the migration code wouldn't normally produce — e.g., a "future"
// version we expect Open to refuse.
func writeSchemaVersion(t *testing.T, s *Store, v int) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `INSERT INTO schema_version (version) VALUES (?)`, v); err != nil {
		t.Fatalf("write schema_version %d: %v", v, err)
	}
}

// makeVolume is a test helper that creates a volume with the given absolute
// path and returns its id.
func makeVolume(t *testing.T, s *Store, path string) int64 {
	t.Helper()
	v, err := s.GetOrCreateVolume(context.Background(), path)
	if err != nil {
		t.Fatalf("GetOrCreateVolume(%q): %v", path, err)
	}
	return v.ID
}

func TestOpenCreatesSchema(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	v, err := s.CurrentSchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("CurrentSchemaVersion: %v", err)
	}
	if v != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", v, SchemaVersion)
	}
}

func TestOpenRoundTripVersion(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(dsn)
	if err != nil {
		t.Fatalf("Re-open: %v", err)
	}
	defer s2.Close()
	v, err := s2.CurrentSchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("CurrentSchemaVersion: %v", err)
	}
	if v != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", v, SchemaVersion)
	}
}

func TestOpenRejectsDSNInjection(t *testing.T) {
	cases := []string{
		"foo.db?_pragma=journal_mode(DELETE)",
		"foo.db#fragment",
		"file:foo.db",
		"sqlite://foo.db",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			if _, err := Open(p); err == nil {
				t.Fatalf("Open(%q) succeeded, want rejection", p)
			}
		})
	}
}

func TestRefuseFutureSchema(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	writeSchemaVersion(t, s, SchemaVersion+5)
	s.Close()

	_, err = Open(dsn)
	if err == nil {
		t.Fatalf("expected refusal to open future-version DB, got nil")
	}
	if !strings.Contains(err.Error(), "newer than binary") {
		t.Fatalf("error %q does not mention version mismatch", err)
	}
}

// TestRefuseV1Schema verifies that v1 databases are refused rather than
// silently auto-migrated. We build a v1-shape schema_version row by hand
// with a direct sql connection, then attempt to Open() it.
func TestRefuseV1Schema(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	if _, err := rawDB.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`); err != nil {
		t.Fatalf("create schema_version: %v", err)
	}
	if _, err := rawDB.Exec(`INSERT INTO schema_version (version) VALUES (1)`); err != nil {
		t.Fatalf("insert v1: %v", err)
	}
	rawDB.Close()

	_, err = Open(dsn)
	if err == nil {
		t.Fatalf("expected refusal to open v1 DB, got nil")
	}
	if !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("error %q does not mention deprecated schema", err)
	}
}

func TestGetOrCreateVolumeIdempotent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	v1, err := s.GetOrCreateVolume(ctx, "/photos/pictures")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	v2, err := s.GetOrCreateVolume(ctx, "/photos/pictures")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if v1.ID != v2.ID {
		t.Fatalf("ids differ: %d vs %d", v1.ID, v2.ID)
	}
	if v1.Name != "pictures" {
		t.Fatalf("name = %q, want pictures", v1.Name)
	}
}

func TestGetOrCreateVolumeBasenameCollision(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	a, err := s.GetOrCreateVolume(ctx, "/a/pictures")
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := s.GetOrCreateVolume(ctx, "/b/pictures")
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	c, err := s.GetOrCreateVolume(ctx, "/c/pictures")
	if err != nil {
		t.Fatalf("c: %v", err)
	}
	if a.Name != "pictures" {
		t.Fatalf("a.Name = %q, want pictures", a.Name)
	}
	if b.Name != "pictures-2" {
		t.Fatalf("b.Name = %q, want pictures-2", b.Name)
	}
	if c.Name != "pictures-3" {
		t.Fatalf("c.Name = %q, want pictures-3", c.Name)
	}
}

// Path is not UNIQUE: two volumes can share the same filesystem path. The
// first call wins for GetOrCreateVolume, but ListVolumes shows both if they
// were created via explicit insert. For now, just confirm the first call is
// idempotent and a second distinct call with the same path returns the same
// volume.
func TestVolumePathNotUnique(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	v1, _ := s.GetOrCreateVolume(ctx, "/shared")
	v2, _ := s.GetOrCreateVolume(ctx, "/shared")
	if v1.ID != v2.ID {
		t.Fatalf("expected same volume for same path")
	}
	// But direct inserts via raw SQL with a different name should succeed,
	// confirming the absence of a UNIQUE constraint on path.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO volumes (name, path) VALUES (?, ?)`, "shared-alt", "/shared"); err != nil {
		t.Fatalf("second insert with same path failed: %v", err)
	}
	vols, _ := s.ListVolumes(ctx)
	if len(vols) != 2 {
		t.Fatalf("ListVolumes = %d, want 2", len(vols))
	}
}

func TestUpsertAndGet(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	xID := makeVolume(t, s, "/x")
	yID := makeVolume(t, s, "/y")

	r := FileRow{
		VolumeID: xID, Path: "a", Blake3: digest(0xab), SizeBytes: 10, MtimeNs: 1, Status: StatusPresent,
		LastSeenAtNs: 100, IndexedAtNs: 100,
	}
	if err := s.Upsert(ctx, r); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := s.GetByPath(ctx, xID, "a")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if !bytes.Equal(got.Blake3, digest(0xab)) || got.SizeBytes != 10 {
		t.Fatalf("got %+v", got)
	}

	// Same relative path under a different volume should not collide.
	r2 := r
	r2.VolumeID = yID
	r2.Blake3 = digest(0xcd)
	if err := s.Upsert(ctx, r2); err != nil {
		t.Fatalf("Upsert /y/a: %v", err)
	}
	gotX, _ := s.GetByPath(ctx, xID, "a")
	gotY, _ := s.GetByPath(ctx, yID, "a")
	if !bytes.Equal(gotX.Blake3, digest(0xab)) || !bytes.Equal(gotY.Blake3, digest(0xcd)) {
		t.Fatalf("volume scoping broken: x=%x y=%x", gotX.Blake3, gotY.Blake3)
	}

	// Update same (volume, path).
	r.Blake3 = digest(0xef)
	r.SizeBytes = 20
	if err := s.Upsert(ctx, r); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err = s.GetByPath(ctx, xID, "a")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if !bytes.Equal(got.Blake3, digest(0xef)) || got.SizeBytes != 20 {
		t.Fatalf("after update got %+v", got)
	}

	// GetByAbsolutePath should find /x/a.
	fv, err := s.GetByAbsolutePath(ctx, "/x/a")
	if err != nil {
		t.Fatalf("GetByAbsolutePath: %v", err)
	}
	if fv.Volume.Path != "/x" || fv.File.Path != "a" {
		t.Fatalf("GetByAbsolutePath returned %+v", fv)
	}
}

func TestMarkMissingScopedToVolume(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	rootID := makeVolume(t, s, "/root")
	otherID := makeVolume(t, s, "/other")

	rows := []FileRow{
		{VolumeID: rootID, Path: "a", Blake3: digest(0x01), SizeBytes: 1, MtimeNs: 1, Status: StatusPresent, LastSeenAtNs: 50, IndexedAtNs: 50},
		{VolumeID: rootID, Path: "b", Blake3: digest(0x02), SizeBytes: 1, MtimeNs: 1, Status: StatusPresent, LastSeenAtNs: 200, IndexedAtNs: 200},
		{VolumeID: otherID, Path: "c", Blake3: digest(0x03), SizeBytes: 1, MtimeNs: 1, Status: StatusPresent, LastSeenAtNs: 50, IndexedAtNs: 50},
	}
	for _, r := range rows {
		if err := s.Upsert(ctx, r); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	n, err := s.MarkMissing(ctx, rootID, 100)
	if err != nil {
		t.Fatalf("MarkMissing: %v", err)
	}
	if n != 1 {
		t.Fatalf("MarkMissing affected %d, want 1", n)
	}

	a, _ := s.GetByPath(ctx, rootID, "a")
	if a.Status != StatusMissing {
		t.Fatalf("/root/a status = %s, want missing", a.Status)
	}
	b, _ := s.GetByPath(ctx, rootID, "b")
	if b.Status != StatusPresent {
		t.Fatalf("/root/b status = %s, want present", b.Status)
	}
	c, _ := s.GetByPath(ctx, otherID, "c")
	if c.Status != StatusPresent {
		t.Fatalf("/other/c status = %s, want present (different volume)", c.Status)
	}
}

func TestListDuplicates(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	rID := makeVolume(t, s, "/r")
	rows := []FileRow{
		{VolumeID: rID, Path: "a", Blake3: digest(0x11), Status: StatusPresent, LastSeenAtNs: 1, IndexedAtNs: 1},
		{VolumeID: rID, Path: "b", Blake3: digest(0x11), Status: StatusPresent, LastSeenAtNs: 1, IndexedAtNs: 1},
		{VolumeID: rID, Path: "c", Blake3: digest(0x22), Status: StatusPresent, LastSeenAtNs: 1, IndexedAtNs: 1},
	}
	for _, r := range rows {
		if err := s.Upsert(ctx, r); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	dups, err := s.ListDuplicates(ctx)
	if err != nil {
		t.Fatalf("ListDuplicates: %v", err)
	}
	if len(dups) != 2 {
		t.Fatalf("dups = %d, want 2", len(dups))
	}
	if !bytes.Equal(dups[0].File.Blake3, digest(0x11)) || !bytes.Equal(dups[1].File.Blake3, digest(0x11)) {
		t.Fatalf("unexpected dups: %+v", dups)
	}
}

func TestCheckConstraintsRejectBadRows(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	rID := makeVolume(t, s, "/r")
	cases := []struct {
		name string
		row  FileRow
	}{
		{"short blake3", FileRow{VolumeID: rID, Path: "a", Blake3: bytes.Repeat([]byte{1}, 31), Status: StatusPresent}},
		{"long blake3", FileRow{VolumeID: rID, Path: "b", Blake3: bytes.Repeat([]byte{1}, 33), Status: StatusPresent}},
		{"empty blake3", FileRow{VolumeID: rID, Path: "c", Blake3: nil, Status: StatusPresent}},
		{"invalid status", FileRow{VolumeID: rID, Path: "d", Blake3: digest(0x01), Status: "weird"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Upsert(ctx, tc.row); err == nil {
				t.Fatalf("Upsert(%+v) succeeded, want CHECK constraint failure", tc.row)
			}
		})
	}
}

func TestCrossVolumeDuplicates(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	aID := makeVolume(t, s, "/A")
	bID := makeVolume(t, s, "/B")
	shared := digest(0x42)
	rows := []FileRow{
		{VolumeID: aID, Path: "x", Blake3: shared, Status: StatusPresent, LastSeenAtNs: 1, IndexedAtNs: 1},
		{VolumeID: bID, Path: "y", Blake3: shared, Status: StatusPresent, LastSeenAtNs: 1, IndexedAtNs: 1},
		{VolumeID: aID, Path: "z", Blake3: digest(0x99), Status: StatusPresent, LastSeenAtNs: 1, IndexedAtNs: 1},
	}
	for _, r := range rows {
		if err := s.Upsert(ctx, r); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	dups, err := s.ListDuplicates(ctx)
	if err != nil {
		t.Fatalf("ListDuplicates: %v", err)
	}
	if len(dups) != 2 {
		t.Fatalf("dups = %d, want 2 (one per volume)", len(dups))
	}
	paths := map[string]bool{dups[0].Volume.Path: true, dups[1].Volume.Path: true}
	if !paths["/A"] || !paths["/B"] {
		t.Fatalf("expected duplicates across /A and /B, got %+v", dups)
	}
}

func TestTouchSeenUpdatesStatusAndTimestamp(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	rID := makeVolume(t, s, "/r")
	r := FileRow{VolumeID: rID, Path: "a", Blake3: digest(0x01), Status: StatusMissing, LastSeenAtNs: 100, IndexedAtNs: 100}
	if err := s.Upsert(ctx, r); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.TouchSeen(ctx, rID, "a", 500); err != nil {
		t.Fatalf("TouchSeen: %v", err)
	}
	got, err := s.GetByPath(ctx, rID, "a")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if got.Status != StatusPresent {
		t.Fatalf("Status = %s, want present", got.Status)
	}
	if got.LastSeenAtNs != 500 {
		t.Fatalf("LastSeenAtNs = %d, want 500", got.LastSeenAtNs)
	}
}

func TestGetByAbsolutePathNotUnderAnyVolume(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	rID := makeVolume(t, s, "/r")
	if err := s.Upsert(ctx, FileRow{VolumeID: rID, Path: "a", Blake3: digest(0x01), Status: StatusPresent, LastSeenAtNs: 1, IndexedAtNs: 1}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := s.GetByAbsolutePath(ctx, "/somewhere/else"); !IsNotFound(err) {
		t.Fatalf("got err=%v, want sql.ErrNoRows", err)
	}
}

// With overlapping volumes (option b from the design discussion), the
// longest-prefix match wins so a query against /a/sub/x lands in the more
// specific volume.
func TestGetByAbsolutePathLongestPrefixWins(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	outer := makeVolume(t, s, "/a")
	inner := makeVolume(t, s, "/a/sub")

	// Same file path "x" upserted under each volume with a distinguishing digest.
	if err := s.Upsert(ctx, FileRow{VolumeID: outer, Path: "sub/x", Blake3: digest(0x01), Status: StatusPresent, LastSeenAtNs: 1, IndexedAtNs: 1}); err != nil {
		t.Fatalf("upsert outer: %v", err)
	}
	if err := s.Upsert(ctx, FileRow{VolumeID: inner, Path: "x", Blake3: digest(0x02), Status: StatusPresent, LastSeenAtNs: 1, IndexedAtNs: 1}); err != nil {
		t.Fatalf("upsert inner: %v", err)
	}

	fv, err := s.GetByAbsolutePath(ctx, "/a/sub/x")
	if err != nil {
		t.Fatalf("GetByAbsolutePath: %v", err)
	}
	if fv.Volume.ID != inner {
		t.Fatalf("expected inner volume to win, got %+v", fv.Volume)
	}
	if !bytes.Equal(fv.File.Blake3, digest(0x02)) {
		t.Fatalf("expected inner digest, got %x", fv.File.Blake3)
	}
}

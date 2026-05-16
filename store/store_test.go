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

// makeRun is a test helper that opens an index run against the given volume
// and returns its id. Tests use it to satisfy the files table's FK to runs
// when constructing FileRow literals directly.
func makeRun(t *testing.T, s *Store, volumeID int64) int64 {
	t.Helper()
	id, err := s.BeginRun(context.Background(), RunKindIndex, volumeID)
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	return id
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
	xRun := makeRun(t, s, xID)
	yRun := makeRun(t, s, yID)

	r := FileRow{
		VolumeID: xID, Path: "a", Blake3: digest(0xab), SizeBytes: 10, MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: xRun, LastSeenRunID: xRun, IndexedAtNs: 100,
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
	r2.FirstSeenRunID = yRun
	r2.LastSeenRunID = yRun
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

	// Two runs against /root: an older one for the row that should age out,
	// then a newer one for the row that should stay present. The /other
	// volume gets its own run so MarkMissing on /root cannot touch it.
	rootOldRun := makeRun(t, s, rootID)
	rootCurRun := makeRun(t, s, rootID)
	otherRun := makeRun(t, s, otherID)

	rows := []FileRow{
		{VolumeID: rootID, Path: "a", Blake3: digest(0x01), SizeBytes: 1, MtimeNs: 1, Status: StatusPresent, FirstSeenRunID: rootOldRun, LastSeenRunID: rootOldRun, IndexedAtNs: 50},
		{VolumeID: rootID, Path: "b", Blake3: digest(0x02), SizeBytes: 1, MtimeNs: 1, Status: StatusPresent, FirstSeenRunID: rootCurRun, LastSeenRunID: rootCurRun, IndexedAtNs: 200},
		{VolumeID: otherID, Path: "c", Blake3: digest(0x03), SizeBytes: 1, MtimeNs: 1, Status: StatusPresent, FirstSeenRunID: otherRun, LastSeenRunID: otherRun, IndexedAtNs: 50},
	}
	for _, r := range rows {
		if err := s.Upsert(ctx, r); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	n, err := s.MarkMissing(ctx, rootID, rootCurRun)
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
	run := makeRun(t, s, rID)
	rows := []FileRow{
		{VolumeID: rID, Path: "a", Blake3: digest(0x11), Status: StatusPresent, FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1},
		{VolumeID: rID, Path: "b", Blake3: digest(0x11), Status: StatusPresent, FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1},
		{VolumeID: rID, Path: "c", Blake3: digest(0x22), Status: StatusPresent, FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1},
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
	run := makeRun(t, s, rID)
	cases := []struct {
		name string
		row  FileRow
	}{
		{"short blake3", FileRow{VolumeID: rID, Path: "a", Blake3: bytes.Repeat([]byte{1}, 31), Status: StatusPresent, FirstSeenRunID: run, LastSeenRunID: run}},
		{"long blake3", FileRow{VolumeID: rID, Path: "b", Blake3: bytes.Repeat([]byte{1}, 33), Status: StatusPresent, FirstSeenRunID: run, LastSeenRunID: run}},
		{"empty blake3", FileRow{VolumeID: rID, Path: "c", Blake3: nil, Status: StatusPresent, FirstSeenRunID: run, LastSeenRunID: run}},
		{"invalid status", FileRow{VolumeID: rID, Path: "d", Blake3: digest(0x01), Status: "weird", FirstSeenRunID: run, LastSeenRunID: run}},
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
	aRun := makeRun(t, s, aID)
	bRun := makeRun(t, s, bID)
	shared := digest(0x42)
	rows := []FileRow{
		{VolumeID: aID, Path: "x", Blake3: shared, Status: StatusPresent, FirstSeenRunID: aRun, LastSeenRunID: aRun, IndexedAtNs: 1},
		{VolumeID: bID, Path: "y", Blake3: shared, Status: StatusPresent, FirstSeenRunID: bRun, LastSeenRunID: bRun, IndexedAtNs: 1},
		{VolumeID: aID, Path: "z", Blake3: digest(0x99), Status: StatusPresent, FirstSeenRunID: aRun, LastSeenRunID: aRun, IndexedAtNs: 1},
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

func TestTouchSeenUpdatesStatusAndLastSeenRun(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	rID := makeVolume(t, s, "/r")
	oldRun := makeRun(t, s, rID)
	newRun := makeRun(t, s, rID)
	r := FileRow{VolumeID: rID, Path: "a", Blake3: digest(0x01), Status: StatusMissing, FirstSeenRunID: oldRun, LastSeenRunID: oldRun, IndexedAtNs: 100}
	if err := s.Upsert(ctx, r); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.TouchSeen(ctx, rID, "a", newRun); err != nil {
		t.Fatalf("TouchSeen: %v", err)
	}
	got, err := s.GetByPath(ctx, rID, "a")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if got.Status != StatusPresent {
		t.Fatalf("Status = %s, want present", got.Status)
	}
	if got.LastSeenRunID != newRun {
		t.Fatalf("LastSeenRunID = %d, want %d", got.LastSeenRunID, newRun)
	}
	if got.FirstSeenRunID != oldRun {
		t.Fatalf("FirstSeenRunID = %d, want %d (must not change on TouchSeen)", got.FirstSeenRunID, oldRun)
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
	run := makeRun(t, s, rID)
	if err := s.Upsert(ctx, FileRow{VolumeID: rID, Path: "a", Blake3: digest(0x01), Status: StatusPresent, FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1}); err != nil {
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
	outerRun := makeRun(t, s, outer)
	innerRun := makeRun(t, s, inner)

	// Same file path "x" upserted under each volume with a distinguishing digest.
	if err := s.Upsert(ctx, FileRow{VolumeID: outer, Path: "sub/x", Blake3: digest(0x01), Status: StatusPresent, FirstSeenRunID: outerRun, LastSeenRunID: outerRun, IndexedAtNs: 1}); err != nil {
		t.Fatalf("upsert outer: %v", err)
	}
	if err := s.Upsert(ctx, FileRow{VolumeID: inner, Path: "x", Blake3: digest(0x02), Status: StatusPresent, FirstSeenRunID: innerRun, LastSeenRunID: innerRun, IndexedAtNs: 1}); err != nil {
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

// TestRunLifecycleTracks verifies that BeginRun + FinishRun produce a runs
// row with the expected terminal state, and that the file rows reference the
// run id rather than a wall-clock timestamp.
func TestRunLifecycleTracks(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	runID, err := s.BeginRun(ctx, RunKindIndex, vID)
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	if runID == 0 {
		t.Fatalf("BeginRun returned 0 id")
	}

	row := FileRow{
		VolumeID: vID, Path: "a", Blake3: digest(0x42), SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: 1,
	}
	if err := s.Upsert(ctx, row); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := s.FinishRun(ctx, runID, RunStatusSuccess, "", 1); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	var kind, status string
	var endedAt sql.NullInt64
	var errStr sql.NullString
	var fileCount int
	if err := s.db.QueryRowContext(ctx,
		`SELECT kind, status, ended_at_ns, error, file_count FROM runs WHERE id = ?`, runID).
		Scan(&kind, &status, &endedAt, &errStr, &fileCount); err != nil {
		t.Fatalf("query run: %v", err)
	}
	if kind != RunKindIndex || status != RunStatusSuccess {
		t.Fatalf("run kind=%q status=%q, want index/success", kind, status)
	}
	if !endedAt.Valid {
		t.Fatalf("ended_at_ns was NULL after FinishRun")
	}
	if errStr.Valid {
		t.Fatalf("error column = %q, want NULL on success", errStr.String)
	}
	if fileCount != 1 {
		t.Fatalf("file_count = %d, want 1", fileCount)
	}

	got, err := s.GetByPath(ctx, vID, "a")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if got.FirstSeenRunID != runID || got.LastSeenRunID != runID {
		t.Fatalf("file run refs = (%d,%d), want (%d,%d)", got.FirstSeenRunID, got.LastSeenRunID, runID, runID)
	}
}

// TestListRunsForVolumeScopedAndOrdered verifies that ListRunsForVolume
// returns only the runs against the requested volume and orders them
// ascending by id (i.e. by start time).
func TestListRunsForVolumeScopedAndOrdered(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	a := makeVolume(t, s, "/a")
	b := makeVolume(t, s, "/b")
	r1 := makeRun(t, s, a)
	r2 := makeRun(t, s, b)
	r3 := makeRun(t, s, a)
	if err := s.FinishRun(ctx, r2, RunStatusSuccess, "", 0); err != nil {
		t.Fatalf("FinishRun r2: %v", err)
	}

	runs, err := s.ListRunsForVolume(ctx, a)
	if err != nil {
		t.Fatalf("ListRunsForVolume(a): %v", err)
	}
	if len(runs) != 2 || runs[0].ID != r1 || runs[1].ID != r3 {
		t.Fatalf("got runs %+v, want ids [%d %d] in order", runs, r1, r3)
	}
	if runs[0].EndedAtNs.Valid {
		t.Fatalf("r1 should still be in-flight, got ended_at = %+v", runs[0].EndedAtNs)
	}
	if runs[0].Status != RunStatusRunning {
		t.Fatalf("r1 status = %q, want %q", runs[0].Status, RunStatusRunning)
	}

	other, err := s.ListRunsForVolume(ctx, b)
	if err != nil {
		t.Fatalf("ListRunsForVolume(b): %v", err)
	}
	if len(other) != 1 || other[0].ID != r2 {
		t.Fatalf("got runs %+v, want single id %d", other, r2)
	}
}

// TestFinishRunPropagatesErrorMessage verifies that a non-empty error message
// passed to FinishRun lands in the runs.error column.
func TestFinishRunPropagatesErrorMessage(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	runID, err := s.BeginRun(ctx, RunKindIndex, vID)
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	if err := s.FinishRun(ctx, runID, RunStatusFailed, "walk: permission denied", 0); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	var status string
	var errStr sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT status, error FROM runs WHERE id = ?`, runID).
		Scan(&status, &errStr); err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != RunStatusFailed {
		t.Fatalf("status = %q, want failed", status)
	}
	if !errStr.Valid || !strings.Contains(errStr.String, "permission denied") {
		t.Fatalf("error column = %+v, want failure message", errStr)
	}
}

// TestFinishRunUnknownIDErrors guards against silently losing a run
// finalisation when the caller passes a runID that doesn't exist (e.g. typo
// in test plumbing, double-finalise, row deleted out from under us). Without
// the RowsAffected check, a run could be left stuck in 'running' forever.
func TestFinishRunUnknownIDErrors(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	err = s.FinishRun(ctx, 99999, RunStatusSuccess, "", 0)
	if err == nil {
		t.Fatalf("FinishRun on unknown id returned nil, want error")
	}
	if !strings.Contains(err.Error(), "no such run") {
		t.Fatalf("error = %q, want 'no such run'", err)
	}
}

// TestMigrateV2ToV3 builds a v2-shape database by hand, then opens it via
// Open() to trigger the migration. The migration must (a) create the runs
// table with one synthetic 'index' run per volume, (b) point every file at
// that run, and (c) drop the old last_seen_at_ns column.
func TestMigrateV2ToV3(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	v2DDL := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE files (
			volume_id     INTEGER NOT NULL REFERENCES volumes(id),
			path          TEXT NOT NULL,
			blake3        BLOB NOT NULL CHECK (length(blake3) = 32),
			size_bytes    INTEGER NOT NULL,
			mtime_ns      INTEGER NOT NULL,
			status        TEXT NOT NULL CHECK (status IN ('present','missing')),
			last_seen_at_ns INTEGER NOT NULL,
			indexed_at_ns INTEGER NOT NULL,
			PRIMARY KEY (volume_id, path)
		)`,
		`INSERT INTO schema_version (version) VALUES (2)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'photos', '/photos'), (2, 'videos', '/videos')`,
	}
	for _, q := range v2DDL {
		if _, err := rawDB.Exec(q); err != nil {
			t.Fatalf("v2 DDL %q: %v", q, err)
		}
	}
	d := digest(0x77)
	if _, err := rawDB.Exec(
		`INSERT INTO files (volume_id, path, blake3, size_bytes, mtime_ns, status, last_seen_at_ns, indexed_at_ns)
		 VALUES (?, ?, ?, ?, ?, 'present', ?, ?), (?, ?, ?, ?, ?, 'present', ?, ?)`,
		1, "a.txt", d, 5, 10, 100, 100,
		2, "clip.mp4", d, 99, 20, 200, 200,
	); err != nil {
		t.Fatalf("seed files: %v", err)
	}
	rawDB.Close()

	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open (should migrate): %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	v, err := s.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("CurrentSchemaVersion: %v", err)
	}
	if v != 3 {
		t.Fatalf("schema_version = %d, want 3", v)
	}

	var runCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runCount); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runCount != 2 {
		t.Fatalf("runs count = %d, want 2 (one per volume)", runCount)
	}

	// Each file row points to the run synthesized for its volume.
	for _, p := range []struct {
		vID int64
		rel string
	}{{1, "a.txt"}, {2, "clip.mp4"}} {
		row, err := s.GetByPath(ctx, p.vID, p.rel)
		if err != nil {
			t.Fatalf("GetByPath: %v", err)
		}
		if row.FirstSeenRunID == 0 || row.LastSeenRunID == 0 {
			t.Fatalf("file %s missing run refs: %+v", p.rel, row)
		}
		if row.FirstSeenRunID != row.LastSeenRunID {
			t.Fatalf("file %s first/last differ: %+v", p.rel, row)
		}
		var volID sql.NullInt64
		if err := s.db.QueryRowContext(ctx,
			`SELECT volume_id FROM runs WHERE id = ?`, row.LastSeenRunID).Scan(&volID); err != nil {
			t.Fatalf("lookup run %d: %v", row.LastSeenRunID, err)
		}
		if !volID.Valid || volID.Int64 != p.vID {
			t.Fatalf("synthesized run for %s has volume_id=%+v, want %d", p.rel, volID, p.vID)
		}
	}

	// The migration must drop last_seen_at_ns. Confirm via PRAGMA.
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(files)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan PRAGMA: %v", err)
		}
		if name == "last_seen_at_ns" {
			t.Fatalf("v2 column last_seen_at_ns still present after migration")
		}
	}
}

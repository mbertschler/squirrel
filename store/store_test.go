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
	id, err := s.BeginRun(context.Background(), RunKindIndex, volumeID, "")
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
	runID, err := s.BeginRun(ctx, RunKindIndex, vID, "")
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

	runs, err := s.ListRuns(ctx, ListRunsOpts{VolumeID: &a})
	if err != nil {
		t.Fatalf("ListRuns(a): %v", err)
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

	other, err := s.ListRuns(ctx, ListRunsOpts{VolumeID: &b})
	if err != nil {
		t.Fatalf("ListRuns(b): %v", err)
	}
	if len(other) != 1 || other[0].ID != r2 {
		t.Fatalf("got runs %+v, want single id %d", other, r2)
	}

	// Descending + limit: most recent run across both volumes.
	desc, err := s.ListRuns(ctx, ListRunsOpts{Limit: 1, Descending: true})
	if err != nil {
		t.Fatalf("ListRuns desc: %v", err)
	}
	if len(desc) != 1 || desc[0].ID != r3 {
		t.Fatalf("ListRuns desc limit 1 = %+v, want id %d", desc, r3)
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
	runID, err := s.BeginRun(ctx, RunKindIndex, vID, "")
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
	// v2 databases now chain through v3 and land at v4 on Open.
	if v != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", v, SchemaVersion)
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

// TestMigrateV3ToV4 builds a v3-shape database by hand, opens it via Open()
// to trigger only the v3→v4 step, and verifies (a) the PK is widened to
// include blake3, (b) the status CHECK accepts 'superseded', and (c) row
// data is preserved verbatim.
func TestMigrateV3ToV4(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	v3DDL := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE runs (
			id INTEGER PRIMARY KEY,
			kind TEXT NOT NULL CHECK (kind IN ('index','sync')),
			volume_id INTEGER REFERENCES volumes(id),
			started_at_ns INTEGER NOT NULL,
			ended_at_ns INTEGER,
			status TEXT NOT NULL CHECK (status IN ('running','success','failed','partial')),
			error TEXT,
			file_count INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE files (
			volume_id INTEGER NOT NULL REFERENCES volumes(id),
			path TEXT NOT NULL,
			blake3 BLOB NOT NULL CHECK (length(blake3) = 32),
			size_bytes INTEGER NOT NULL,
			mtime_ns INTEGER NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('present','missing')),
			first_seen_run_id INTEGER NOT NULL REFERENCES runs(id),
			last_seen_run_id INTEGER NOT NULL REFERENCES runs(id),
			indexed_at_ns INTEGER NOT NULL,
			PRIMARY KEY (volume_id, path)
		)`,
		`INSERT INTO schema_version (version) VALUES (3)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'photos', '/photos')`,
		`INSERT INTO runs (id, kind, volume_id, started_at_ns, status) VALUES (1, 'index', 1, 100, 'success')`,
	}
	for _, q := range v3DDL {
		if _, err := rawDB.Exec(q); err != nil {
			t.Fatalf("v3 DDL %q: %v", q, err)
		}
	}
	d := digest(0x55)
	if _, err := rawDB.Exec(
		`INSERT INTO files (volume_id, path, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns)
		 VALUES (1, 'photo.jpg', ?, 1024, 50, 'present', 1, 1, 50)`, d,
	); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	rawDB.Close()

	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open (should migrate v3→v4): %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	// Migrations chain on Open, so a v3 DB lands at SchemaVersion (currently
	// v5) — not at v4. The v3→v4 step still ran; we verify that below by
	// inserting a row that v3's narrower PK would have rejected.
	if v, _ := s.CurrentSchemaVersion(ctx); v != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", v, SchemaVersion)
	}

	// PK should now include blake3 — confirm by inserting a second row at the
	// same (volume_id, path) but different blake3, which would have collided
	// pre-migration.
	d2 := digest(0x66)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO files (volume_id, path, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns)
		VALUES (1, 'photo.jpg', ?, 1024, 60, 'superseded', 1, 1, 60)
	`, d2); err != nil {
		t.Fatalf("insert second blake3 at same path failed (PK not widened?): %v", err)
	}

	// Status CHECK should accept 'superseded'.
	row, err := s.GetByPath(ctx, 1, "photo.jpg")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if !bytes.Equal(row.Blake3, d) || row.Status != StatusPresent {
		t.Fatalf("live row = %+v, want original d=%x status=present", row, d)
	}
}

// TestUpsertContentChangePreservesOldHash is the core append-only guarantee:
// when content at a path changes, the old hash must remain in the database
// as a superseded row rather than being overwritten in place.
func TestUpsertContentChangePreservesOldHash(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	run1 := makeRun(t, s, vID)
	run2 := makeRun(t, s, vID)

	hashA := digest(0xaa)
	hashB := digest(0xbb)

	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "doc.txt", Blake3: hashA, SizeBytes: 10, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: run1, LastSeenRunID: run1, IndexedAtNs: 1,
	}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "doc.txt", Blake3: hashB, SizeBytes: 20, MtimeNs: 2,
		Status: StatusPresent, FirstSeenRunID: run2, LastSeenRunID: run2, IndexedAtNs: 2,
	}); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	history, err := s.ListHistoryByPath(ctx, vID, "doc.txt")
	if err != nil {
		t.Fatalf("ListHistoryByPath: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history has %d rows, want 2 (old superseded + new present)", len(history))
	}
	if !bytes.Equal(history[0].Blake3, hashA) || history[0].Status != StatusSuperseded {
		t.Fatalf("history[0] = %+v, want hashA superseded", history[0])
	}
	if !bytes.Equal(history[1].Blake3, hashB) || history[1].Status != StatusPresent {
		t.Fatalf("history[1] = %+v, want hashB present", history[1])
	}
	// The first-seen run on the surviving record of the original content
	// must be preserved at run1, not overwritten by run2.
	if history[0].FirstSeenRunID != run1 {
		t.Fatalf("superseded row FirstSeenRunID = %d, want %d", history[0].FirstSeenRunID, run1)
	}

	live, err := s.GetByPath(ctx, vID, "doc.txt")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if !bytes.Equal(live.Blake3, hashB) {
		t.Fatalf("GetByPath returned blake3 %x, want hashB %x (should skip superseded rows)", live.Blake3, hashB)
	}
}

// TestUpsertRevertContent verifies the round-trip case: content goes A → B → A
// at a path. The original A row should resurface as live and the B row
// should be superseded. There should still be just two physical rows (the
// revert reuses the original A row rather than appending a third).
func TestUpsertRevertContent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	r1 := makeRun(t, s, vID)
	r2 := makeRun(t, s, vID)
	r3 := makeRun(t, s, vID)
	hashA := digest(0xaa)
	hashB := digest(0xbb)
	mkRow := func(hash []byte, run int64, mtime int64) FileRow {
		return FileRow{
			VolumeID: vID, Path: "doc.txt", Blake3: hash, SizeBytes: 10, MtimeNs: mtime,
			Status: StatusPresent, FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: mtime,
		}
	}
	for _, r := range []FileRow{mkRow(hashA, r1, 1), mkRow(hashB, r2, 2), mkRow(hashA, r3, 3)} {
		if err := s.Upsert(ctx, r); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	history, err := s.ListHistoryByPath(ctx, vID, "doc.txt")
	if err != nil {
		t.Fatalf("ListHistoryByPath: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history has %d rows, want 2 (revert reuses original A row)", len(history))
	}
	// Reverted A is live; B is superseded.
	if !bytes.Equal(history[0].Blake3, hashA) || history[0].Status != StatusPresent {
		t.Fatalf("history[0] = %+v, want hashA present (revert resurrects original)", history[0])
	}
	if !bytes.Equal(history[1].Blake3, hashB) || history[1].Status != StatusSuperseded {
		t.Fatalf("history[1] = %+v, want hashB superseded", history[1])
	}
	// The revived row must keep its original first-seen run (r1), not r3.
	if history[0].FirstSeenRunID != r1 {
		t.Fatalf("revived row FirstSeenRunID = %d, want %d (must not be rewritten on revert)", history[0].FirstSeenRunID, r1)
	}
	if history[0].LastSeenRunID != r3 {
		t.Fatalf("revived row LastSeenRunID = %d, want %d (should advance to revert run)", history[0].LastSeenRunID, r3)
	}
}

// TestTriggerRejectsBlake3Update guards the schema-level "blake3 is
// immutable" rule. The trigger must reject any UPDATE that mentions blake3
// in its SET clause, even if invoked outside of Upsert (e.g. via raw SQL in
// some future code path).
func TestTriggerRejectsBlake3Update(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	run := makeRun(t, s, vID)
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "x", Blake3: digest(0xaa), SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Direct UPDATE bypassing the Upsert state machine — the trigger must
	// abort it.
	_, err = s.db.ExecContext(ctx,
		`UPDATE files SET blake3 = ? WHERE volume_id = ? AND path = ?`,
		digest(0xbb), vID, "x")
	if err == nil {
		t.Fatalf("direct UPDATE of blake3 succeeded; trigger did not fire")
	}
	if !strings.Contains(err.Error(), "blake3 is immutable") {
		t.Fatalf("got error %q, want one mentioning blake3 immutability", err)
	}

	// Untouched: the original row still has its original hash.
	row, err := s.GetByPath(ctx, vID, "x")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if !bytes.Equal(row.Blake3, digest(0xaa)) {
		t.Fatalf("blake3 = %x, want %x (trigger should have aborted the UPDATE)", row.Blake3, digest(0xaa))
	}
}

// TestUniqueIndexRejectsSecondLiveRow guards the path-level invariant that
// at most one non-superseded row exists per (volume_id, path), enforced by
// the partial UNIQUE index. The Upsert state machine should never produce
// two live rows, but if a future code path tried to via direct SQL, the
// index must reject it.
func TestUniqueIndexRejectsSecondLiveRow(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	run := makeRun(t, s, vID)
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "x", Blake3: digest(0xaa), SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Try to insert a second live row at the same (volume, path) without
	// superseding the first. The UNIQUE index must abort this.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO files (volume_id, path, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns)
		VALUES (?, ?, ?, ?, ?, 'present', ?, ?, ?)
	`, vID, "x", digest(0xbb), 1, 2, run, run, 2)
	if err == nil {
		t.Fatalf("direct INSERT of second live row succeeded; unique index did not fire")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("got error %q, want one mentioning UNIQUE constraint", err)
	}

	// Inserting a 'superseded' row at the same (V, P) is allowed — superseded
	// rows are exempt from the partial unique constraint, so the schema
	// supports unbounded historical depth per path.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO files (volume_id, path, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns)
		VALUES (?, ?, ?, ?, ?, 'superseded', ?, ?, ?)
	`, vID, "x", digest(0xcc), 1, 3, run, run, 3); err != nil {
		t.Fatalf("inserting superseded row should be allowed, got: %v", err)
	}
}

// TestMigrateV3ToV4InstallsSchemaGuards verifies that a v3 database upgraded
// to v4 ends up with the trigger and the partial unique index, not just the
// widened PK and the superseded status. Without these, the migration would
// leave existing databases lacking the enforcement that fresh installs get.
func TestMigrateV3ToV4InstallsSchemaGuards(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	v3DDL := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE runs (id INTEGER PRIMARY KEY, kind TEXT NOT NULL CHECK (kind IN ('index','sync')), volume_id INTEGER REFERENCES volumes(id), started_at_ns INTEGER NOT NULL, ended_at_ns INTEGER, status TEXT NOT NULL CHECK (status IN ('running','success','failed','partial')), error TEXT, file_count INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE files (volume_id INTEGER NOT NULL REFERENCES volumes(id), path TEXT NOT NULL, blake3 BLOB NOT NULL CHECK (length(blake3) = 32), size_bytes INTEGER NOT NULL, mtime_ns INTEGER NOT NULL, status TEXT NOT NULL CHECK (status IN ('present','missing')), first_seen_run_id INTEGER NOT NULL REFERENCES runs(id), last_seen_run_id INTEGER NOT NULL REFERENCES runs(id), indexed_at_ns INTEGER NOT NULL, PRIMARY KEY (volume_id, path))`,
		`INSERT INTO schema_version (version) VALUES (3)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'v', '/v')`,
		`INSERT INTO runs (id, kind, volume_id, started_at_ns, status) VALUES (1, 'index', 1, 1, 'success')`,
		`INSERT INTO files (volume_id, path, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns) VALUES (1, 'x', X'` + strings.Repeat("aa", 32) + `', 1, 1, 'present', 1, 1, 1)`,
	}
	for _, q := range v3DDL {
		if _, err := rawDB.Exec(q); err != nil {
			t.Fatalf("v3 DDL %q: %v", q, err)
		}
	}
	rawDB.Close()

	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open (migrates v3→v4): %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	// Trigger must reject blake3 updates on the migrated DB.
	_, err = s.db.ExecContext(ctx, `UPDATE files SET blake3 = ? WHERE volume_id = 1 AND path = 'x'`, digest(0xbb))
	if err == nil || !strings.Contains(err.Error(), "blake3 is immutable") {
		t.Fatalf("trigger missing after migration; err = %v", err)
	}

	// Partial UNIQUE index must reject a second live row at the same (V, P).
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO files (volume_id, path, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns)
		VALUES (1, 'x', ?, 1, 2, 'present', 1, 1, 2)
	`, digest(0xcc))
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("unique index missing after migration; err = %v", err)
	}
}

// TestMarkMissingIgnoresSupersededRows guards that MarkMissing only touches
// 'present' rows. Superseded rows hold historical content and should never
// transition to 'missing' regardless of how stale their last_seen_run_id is.
func TestMarkMissingIgnoresSupersededRows(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	oldRun := makeRun(t, s, vID)
	curRun := makeRun(t, s, vID)
	// Two upserts at the same path with different hashes — the first becomes
	// superseded once the second lands.
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "p", Blake3: digest(0x01), SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: oldRun, LastSeenRunID: oldRun, IndexedAtNs: 1,
	}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "p", Blake3: digest(0x02), SizeBytes: 1, MtimeNs: 2,
		Status: StatusPresent, FirstSeenRunID: curRun, LastSeenRunID: curRun, IndexedAtNs: 2,
	}); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	// Call MarkMissing with a brand-new run id that doesn't match anyone — both
	// rows have stale last_seen_run_id but only the live present row should flip.
	newerRun := makeRun(t, s, vID)
	n, err := s.MarkMissing(ctx, vID, newerRun)
	if err != nil {
		t.Fatalf("MarkMissing: %v", err)
	}
	if n != 1 {
		t.Fatalf("MarkMissing affected %d rows, want 1 (only the live present row)", n)
	}

	history, err := s.ListHistoryByPath(ctx, vID, "p")
	if err != nil {
		t.Fatalf("ListHistoryByPath: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history has %d rows, want 2", len(history))
	}
	if history[0].Status != StatusSuperseded {
		t.Fatalf("old row status = %q, want superseded (must not be flipped to missing)", history[0].Status)
	}
	if history[1].Status != StatusMissing {
		t.Fatalf("live row status = %q, want missing", history[1].Status)
	}
}

// TestMigrateV4ToV5 builds a v4-shape database by hand, opens it via Open()
// to trigger the v4→v5 step, and verifies (a) runs row data carries over
// with destination=NULL, (b) the destination column exists, and (c) the
// new kind↔destination CHECK rejects malformed inserts.
func TestMigrateV4ToV5(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	v4DDL := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE runs (
			id INTEGER PRIMARY KEY,
			kind TEXT NOT NULL CHECK (kind IN ('index','sync')),
			volume_id INTEGER REFERENCES volumes(id),
			started_at_ns INTEGER NOT NULL,
			ended_at_ns INTEGER,
			status TEXT NOT NULL CHECK (status IN ('running','success','failed','partial')),
			error TEXT,
			file_count INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE files (
			volume_id INTEGER NOT NULL REFERENCES volumes(id),
			path TEXT NOT NULL,
			blake3 BLOB NOT NULL CHECK (length(blake3) = 32),
			size_bytes INTEGER NOT NULL,
			mtime_ns INTEGER NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('present','missing','superseded')),
			first_seen_run_id INTEGER NOT NULL REFERENCES runs(id),
			last_seen_run_id INTEGER NOT NULL REFERENCES runs(id),
			indexed_at_ns INTEGER NOT NULL,
			PRIMARY KEY (volume_id, path, blake3)
		)`,
		`INSERT INTO schema_version (version) VALUES (4)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'photos', '/photos')`,
		`INSERT INTO runs (id, kind, volume_id, started_at_ns, status, file_count)
		 VALUES (1, 'index', 1, 100, 'success', 7)`,
	}
	for _, q := range v4DDL {
		if _, err := rawDB.Exec(q); err != nil {
			t.Fatalf("v4 DDL %q: %v", q, err)
		}
	}
	// Seed a file row so the v4→v5 FK rebuild has something to preserve
	// across the runs table drop+recreate.
	if _, err := rawDB.Exec(
		`INSERT INTO files (volume_id, path, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns)
		 VALUES (1, 'a.jpg', ?, 10, 1, 'present', 1, 1, 1)`, digest(0x12),
	); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	rawDB.Close()

	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open (should migrate v4→v5): %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if v, _ := s.CurrentSchemaVersion(ctx); v != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", v, SchemaVersion)
	}

	// Existing run carries over with destination=NULL and original ID.
	var dest sql.NullString
	var fileCount int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT destination, file_count FROM runs WHERE id = 1`).Scan(&dest, &fileCount); err != nil {
		t.Fatalf("read migrated run: %v", err)
	}
	if dest.Valid {
		t.Fatalf("destination = %+v, want NULL for migrated index run", dest)
	}
	if fileCount != 7 {
		t.Fatalf("file_count = %d, want 7 (data preserved)", fileCount)
	}

	// The FK from files.last_seen_run_id → runs(id) must still resolve.
	row, err := s.GetByPath(ctx, 1, "a.jpg")
	if err != nil {
		t.Fatalf("GetByPath after migration: %v", err)
	}
	if row.LastSeenRunID != 1 {
		t.Fatalf("LastSeenRunID = %d, want 1 (FK preserved)", row.LastSeenRunID)
	}
}

// TestRunsKindDestinationCheck enforces the v5 schema-level coupling:
// destination must be NULL for index runs and non-empty for sync/restore.
// We exercise the check by going around BeginRun (which enforces the same
// rule via parameter shape) and inserting raw rows.
func TestRunsKindDestinationCheck(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	cases := []struct {
		name      string
		kind      string
		dest      any // string or nil
		wantError bool
	}{
		{"index without destination", "index", nil, false},
		{"index with destination", "index", "nas", true},
		{"sync with destination", "sync", "nas", false},
		{"sync without destination", "sync", nil, true},
		{"sync with empty destination", "sync", "", true},
		{"restore with destination", "restore", "nas", false},
		{"restore without destination", "restore", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.db.ExecContext(ctx, `
				INSERT INTO runs (kind, volume_id, destination, started_at_ns, status)
				VALUES (?, ?, ?, ?, 'running')`, c.kind, vID, c.dest, NowNs())
			if c.wantError && err == nil {
				t.Fatalf("insert succeeded; want CHECK violation")
			}
			if !c.wantError && err != nil {
				t.Fatalf("insert failed: %v", err)
			}
		})
	}
}

// TestBeginRunDestinationRoundTrip verifies that destination passed to
// BeginRun shows up on ListRuns rows and that index runs leave it NULL.
func TestBeginRunDestinationRoundTrip(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	indexID, err := s.BeginRun(ctx, RunKindIndex, vID, "")
	if err != nil {
		t.Fatalf("BeginRun index: %v", err)
	}
	syncID, err := s.BeginRun(ctx, RunKindSync, vID, "nas")
	if err != nil {
		t.Fatalf("BeginRun sync: %v", err)
	}

	runs, err := s.ListRuns(ctx, ListRunsOpts{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	byID := map[int64]Run{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	if got := byID[indexID]; got.Destination.Valid {
		t.Fatalf("index run destination = %+v, want NULL", got.Destination)
	}
	if got := byID[syncID]; !got.Destination.Valid || got.Destination.String != "nas" {
		t.Fatalf("sync run destination = %+v, want 'nas'", got.Destination)
	}
}

// TestLatestSuccessfulIndexRun confirms the prerequisite-check helper used
// by sync: it returns the most recent success/partial index run for a
// volume, ignoring failed runs and other kinds.
func TestLatestSuccessfulIndexRun(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	if _, err := s.LatestSuccessfulIndexRun(ctx, vID); !IsNotFound(err) {
		t.Fatalf("expected ErrNoRows on fresh volume, got %v", err)
	}

	failID, _ := s.BeginRun(ctx, RunKindIndex, vID, "")
	_ = s.FinishRun(ctx, failID, RunStatusFailed, "walk: nope", 0)

	okID, _ := s.BeginRun(ctx, RunKindIndex, vID, "")
	_ = s.FinishRun(ctx, okID, RunStatusSuccess, "", 3)

	syncID, _ := s.BeginRun(ctx, RunKindSync, vID, "nas")
	_ = s.FinishRun(ctx, syncID, RunStatusSuccess, "", 3)

	got, err := s.LatestSuccessfulIndexRun(ctx, vID)
	if err != nil {
		t.Fatalf("LatestSuccessfulIndexRun: %v", err)
	}
	if got.ID != okID {
		t.Fatalf("got run id %d, want %d (most recent successful index)", got.ID, okID)
	}
}

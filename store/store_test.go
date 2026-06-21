package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	id, err := s.BeginIndexRun(context.Background(), RunKindIndex, volumeID, false)
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
	cases := map[string]string{
		"query_string":     "foo.db?_pragma=journal_mode(DELETE)",
		"fragment":         "foo.db#fragment",
		"file_uri":         "file:foo.db",
		"scheme_uri":       "sqlite://foo.db",
		"nul_byte":         "foo.db\x00.evil",
		"leading_space":    " foo.db",
		"trailing_space":   "foo.db ",
		"trailing_newline": "foo.db\n",
		"leading_tab":      "\tfoo.db",
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
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
	if err := s.Upsert(ctx, r, nil); err != nil {
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
	if err := s.Upsert(ctx, r2, nil); err != nil {
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
	if err := s.Upsert(ctx, r, nil); err != nil {
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
		if err := s.Upsert(ctx, r, nil); err != nil {
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
		if err := s.Upsert(ctx, r, nil); err != nil {
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
			if err := s.Upsert(ctx, tc.row, nil); err == nil {
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
		if err := s.Upsert(ctx, r, nil); err != nil {
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
	if err := s.Upsert(ctx, r, nil); err != nil {
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
	if err := s.Upsert(ctx, FileRow{VolumeID: rID, Path: "a", Blake3: digest(0x01), Status: StatusPresent, FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1}, nil); err != nil {
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
	if err := s.Upsert(ctx, FileRow{VolumeID: outer, Path: "sub/x", Blake3: digest(0x01), Status: StatusPresent, FirstSeenRunID: outerRun, LastSeenRunID: outerRun, IndexedAtNs: 1}, nil); err != nil {
		t.Fatalf("upsert outer: %v", err)
	}
	if err := s.Upsert(ctx, FileRow{VolumeID: inner, Path: "x", Blake3: digest(0x02), Status: StatusPresent, FirstSeenRunID: innerRun, LastSeenRunID: innerRun, IndexedAtNs: 1}, nil); err != nil {
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
	runID, err := s.BeginIndexRun(ctx, RunKindIndex, vID, false)
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
	if err := s.Upsert(ctx, row, nil); err != nil {
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
	runID, err := s.BeginIndexRun(ctx, RunKindIndex, vID, false)
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
	// Distinct content gets distinct hashes: a BLAKE3 digest is over the
	// bytes, so two files of different size cannot share one hash (and the
	// v13→v14 guard rejects a DB where they do).
	if _, err := rawDB.Exec(
		`INSERT INTO files (volume_id, path, blake3, size_bytes, mtime_ns, status, last_seen_at_ns, indexed_at_ns)
		 VALUES (?, ?, ?, ?, ?, 'present', ?, ?), (?, ?, ?, ?, ?, 'present', ?, ?)`,
		1, "a.txt", digest(0x77), 5, 10, 100, 100,
		2, "clip.mp4", digest(0x88), 99, 20, 200, 200,
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

	// PK now includes the content identity — confirm by inserting a second
	// row at the same (folder, name) with different content, which would
	// have collided pre-v4. v14 keys files off (folder_id, name,
	// content_id) but the same widening invariant applies.
	d2 := digest(0x66)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO contents (blake3, size_bytes) VALUES (?, 1024)`, d2); err != nil {
		t.Fatalf("insert second content: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO files (folder_id, name, content_id, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns)
		VALUES ((SELECT id FROM folders WHERE volume_id = 1 AND path = ''), 'photo.jpg',
		        (SELECT id FROM contents WHERE blake3 = ?), 60, 'superseded', 1, 1, 60)
	`, d2); err != nil {
		t.Fatalf("insert second content at same path failed (PK not widened?): %v", err)
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
	}, nil); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "doc.txt", Blake3: hashB, SizeBytes: 20, MtimeNs: 2,
		Status: StatusPresent, FirstSeenRunID: run2, LastSeenRunID: run2, IndexedAtNs: 2,
	}, nil); err != nil {
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
		if err := s.Upsert(ctx, r, nil); err != nil {
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

// TestContentsRowSharedAcrossPaths pins the id↔hash construction the v14
// split rests on: every path observing the same bytes resolves to the
// same contents row, and the UNIQUE constraint on contents.blake3 rejects
// a second row for the same digest, so a content id can never silently
// change which hash it stands for.
func TestContentsRowSharedAcrossPaths(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	run := makeRun(t, s, vID)
	d := digest(0xaa)
	for _, p := range []string{"x", "sub/y"} {
		if err := s.Upsert(ctx, FileRow{
			VolumeID: vID, Path: p, Blake3: d, SizeBytes: 1, MtimeNs: 1,
			Status: StatusPresent, FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1,
		}, nil); err != nil {
			t.Fatalf("Upsert %s: %v", p, err)
		}
	}

	rowX, err := s.GetByPath(ctx, vID, "x")
	if err != nil {
		t.Fatalf("GetByPath x: %v", err)
	}
	rowY, err := s.GetByPath(ctx, vID, "sub/y")
	if err != nil {
		t.Fatalf("GetByPath sub/y: %v", err)
	}
	if rowX.ContentID == 0 || rowX.ContentID != rowY.ContentID {
		t.Fatalf("content ids = (%d, %d), want one shared non-zero id", rowX.ContentID, rowY.ContentID)
	}

	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM contents`).Scan(&n); err != nil {
		t.Fatalf("count contents: %v", err)
	}
	if n != 1 {
		t.Fatalf("contents rows = %d, want 1 (one row per distinct hash)", n)
	}

	_, err = s.db.ExecContext(ctx, `INSERT INTO contents (blake3, size_bytes) VALUES (?, 1)`, d)
	if err == nil {
		t.Fatalf("second contents row for the same blake3 succeeded; UNIQUE did not fire")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("got error %q, want one mentioning UNIQUE constraint", err)
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
	}, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Try to insert a second live row at the same (folder, name) without
	// superseding the first. The UNIQUE index must abort this.
	var rootFolderID int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM folders WHERE volume_id = ? AND path = ''`, vID).Scan(&rootFolderID); err != nil {
		t.Fatalf("lookup root folder: %v", err)
	}
	insertContent := func(d []byte) int64 {
		t.Helper()
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO contents (blake3, size_bytes) VALUES (?, 1)`, d)
		if err != nil {
			t.Fatalf("insert content: %v", err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO files (folder_id, name, content_id, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns)
		VALUES (?, ?, ?, ?, 'present', ?, ?, ?)
	`, rootFolderID, "x", insertContent(digest(0xbb)), 2, run, run, 2)
	if err == nil {
		t.Fatalf("direct INSERT of second live row succeeded; unique index did not fire")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("got error %q, want one mentioning UNIQUE constraint", err)
	}

	// Inserting a 'superseded' row at the same (folder, name) is allowed —
	// superseded rows are exempt from the partial unique constraint, so the
	// schema supports unbounded historical depth per path.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO files (folder_id, name, content_id, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns)
		VALUES (?, ?, ?, ?, 'superseded', ?, ?, ?)
	`, rootFolderID, "x", insertContent(digest(0xcc)), 3, run, run, 3); err != nil {
		t.Fatalf("inserting superseded row should be allowed, got: %v", err)
	}
}

// TestMigrateV3ToV4InstallsSchemaGuards verifies that a v3 database
// migrated through the full chain ends up with the same enforcement a
// fresh install gets: the partial unique index keeps one live row per
// path, and the seeded content survives the v14 split with its hash
// resolvable through contents.
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

	// The seeded row's hash resolves through the v14 contents table.
	row, err := s.GetByPath(ctx, 1, "x")
	if err != nil {
		t.Fatalf("GetByPath after migration: %v", err)
	}
	if !bytes.Equal(row.Blake3, digest(0xaa)) {
		t.Fatalf("migrated row blake3 = %x, want %x", row.Blake3, digest(0xaa))
	}

	// Partial UNIQUE index must reject a second live row at the same
	// (folder, name).
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO contents (blake3, size_bytes) VALUES (?, 1)`, digest(0xcc)); err != nil {
		t.Fatalf("insert content: %v", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO files (folder_id, name, content_id, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns)
		VALUES ((SELECT id FROM folders WHERE volume_id = 1 AND path = ''), 'x',
		        (SELECT id FROM contents WHERE blake3 = ?), 2, 'present', 1, 1, 2)
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
	}, nil); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "p", Blake3: digest(0x02), SizeBytes: 1, MtimeNs: 2,
		Status: StatusPresent, FirstSeenRunID: curRun, LastSeenRunID: curRun, IndexedAtNs: 2,
	}, nil); err != nil {
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

	indexID, err := s.BeginIndexRun(ctx, RunKindIndex, vID, false)
	if err != nil {
		t.Fatalf("BeginRun index: %v", err)
	}
	syncID, err := s.BeginRun(ctx, RunKindSync, vID, "nas", false)
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

// TestBeginIndexRunRecordsShallow verifies that BeginIndexRun and
// BeginRun both stamp the runs.shallow column with the value the caller
// passed: a shallow run records 1, a full run records 0, across index,
// audit, and sync/restore kinds. (The pre-v10 NULL state survives only
// for the receiver side of a node sync and for migrated history.)
func TestBeginIndexRunRecordsShallow(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	shallowID, err := s.BeginIndexRun(ctx, RunKindIndex, vID, true)
	if err != nil {
		t.Fatalf("BeginIndexRun shallow: %v", err)
	}
	fullID, err := s.BeginIndexRun(ctx, RunKindIndex, vID, false)
	if err != nil {
		t.Fatalf("BeginIndexRun full: %v", err)
	}
	auditID, err := s.BeginIndexRun(ctx, RunKindAudit, vID, true)
	if err != nil {
		t.Fatalf("BeginIndexRun audit: %v", err)
	}
	syncShallowID, err := s.BeginRun(ctx, RunKindSync, vID, "nas", true)
	if err != nil {
		t.Fatalf("BeginRun sync shallow: %v", err)
	}
	restoreFullID, err := s.BeginRun(ctx, RunKindRestore, vID, "nas", false)
	if err != nil {
		t.Fatalf("BeginRun restore full: %v", err)
	}

	runs, err := s.ListRuns(ctx, ListRunsOpts{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	byID := map[int64]Run{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	if got := byID[shallowID]; !got.Shallow.Valid || !got.Shallow.Bool {
		t.Fatalf("shallow run: got %+v, want valid=true bool=true", got.Shallow)
	}
	if got := byID[fullID]; !got.Shallow.Valid || got.Shallow.Bool {
		t.Fatalf("full run: got %+v, want valid=true bool=false", got.Shallow)
	}
	if got := byID[auditID]; !got.Shallow.Valid || !got.Shallow.Bool {
		t.Fatalf("audit shallow run: got %+v, want valid=true bool=true", got.Shallow)
	}
	if got := byID[syncShallowID]; !got.Shallow.Valid || !got.Shallow.Bool {
		t.Fatalf("sync shallow run: got %+v, want valid=true bool=true", got.Shallow)
	}
	if got := byID[restoreFullID]; !got.Shallow.Valid || got.Shallow.Bool {
		t.Fatalf("restore full run: got %+v, want valid=true bool=false", got.Shallow)
	}
}

// TestBeginIndexRunRejectsWrongKind pins the validation: only index and
// audit kinds may go through BeginIndexRun. The shallow flag has no
// meaning for sync/restore so the API refuses to record one.
func TestBeginIndexRunRejectsWrongKind(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	for _, kind := range []string{RunKindSync, RunKindRestore, "bogus"} {
		if _, err := s.BeginIndexRun(ctx, kind, vID, true); err == nil {
			t.Fatalf("BeginIndexRun(%q) accepted, want error", kind)
		}
	}
}

// TestBeginRunRejectsIndexAuditKind pins the inverse: BeginRun refuses
// index and audit so callers can't accidentally insert a row with NULL
// shallow when the run did make a hashing-mode choice.
func TestBeginRunRejectsIndexAuditKind(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	for _, kind := range []string{RunKindIndex, RunKindAudit} {
		if _, err := s.BeginRun(ctx, kind, vID, "", false); err == nil {
			t.Fatalf("BeginRun(%q) accepted, want error directing to BeginIndexRun", kind)
		}
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

	failID, _ := s.BeginIndexRun(ctx, RunKindIndex, vID, false)
	_ = s.FinishRun(ctx, failID, RunStatusFailed, "walk: nope", 0)

	okID, _ := s.BeginIndexRun(ctx, RunKindIndex, vID, false)
	_ = s.FinishRun(ctx, okID, RunStatusSuccess, "", 3)

	syncID, _ := s.BeginRun(ctx, RunKindSync, vID, "nas", false)
	_ = s.FinishRun(ctx, syncID, RunStatusSuccess, "", 3)

	got, err := s.LatestSuccessfulIndexRun(ctx, vID)
	if err != nil {
		t.Fatalf("LatestSuccessfulIndexRun: %v", err)
	}
	if got.ID != okID {
		t.Fatalf("got run id %d, want %d (most recent successful index)", got.ID, okID)
	}
}

// TestLatestSuccessfulRunsByVolumeAndKind covers the dashboard helper:
// one row per (volume, kind) with status in success/partial, ignoring
// failed runs and runs from other (volume, kind) pairs, and reaching
// past any bounded recent-runs window.
func TestLatestSuccessfulRunsByVolumeAndKind(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	volA := makeVolume(t, s, "/a")
	volB := makeVolume(t, s, "/b")

	// volA: an old successful index, a newer failed index (must be
	// ignored), a partial sync to one destination, and a successful sync
	// to a different destination — the latest sync, across destinations,
	// is what the helper should return.
	oldIdx, _ := s.BeginIndexRun(ctx, RunKindIndex, volA, false)
	_ = s.FinishRun(ctx, oldIdx, RunStatusSuccess, "", 10)
	failIdx, _ := s.BeginIndexRun(ctx, RunKindIndex, volA, false)
	_ = s.FinishRun(ctx, failIdx, RunStatusFailed, "boom", 0)
	syncNas, _ := s.BeginRun(ctx, RunKindSync, volA, "nas", false)
	_ = s.FinishRun(ctx, syncNas, RunStatusPartial, "", 5)
	syncS3, _ := s.BeginRun(ctx, RunKindSync, volA, "s3", false)
	_ = s.FinishRun(ctx, syncS3, RunStatusSuccess, "", 5)

	// volB: only a single successful index, no sync. The map should
	// contain the index entry but no sync entry.
	bIdx, _ := s.BeginIndexRun(ctx, RunKindIndex, volB, false)
	_ = s.FinishRun(ctx, bIdx, RunStatusSuccess, "", 1)

	got, err := s.LatestSuccessfulRunsByVolumeAndKind(ctx)
	if err != nil {
		t.Fatalf("LatestSuccessfulRunsByVolumeAndKind: %v", err)
	}

	aByKind := got[volA]
	if aByKind == nil {
		t.Fatalf("missing volA entry in result: %+v", got)
	}
	if aByKind[RunKindIndex].ID != oldIdx {
		t.Errorf("volA latest index = %d, want %d (the failed one must be ignored)",
			aByKind[RunKindIndex].ID, oldIdx)
	}
	if aByKind[RunKindSync].ID != syncS3 {
		t.Errorf("volA latest sync = %d, want %d (latest across destinations)",
			aByKind[RunKindSync].ID, syncS3)
	}

	bByKind := got[volB]
	if bByKind == nil {
		t.Fatalf("missing volB entry in result: %+v", got)
	}
	if bByKind[RunKindIndex].ID != bIdx {
		t.Errorf("volB latest index = %d, want %d", bByKind[RunKindIndex].ID, bIdx)
	}
	if _, ok := bByKind[RunKindSync]; ok {
		t.Errorf("volB should have no sync entry: got %+v", bByKind)
	}
}

// TestFreshV6SelfNodeRow verifies the v6 acceptance contract: a fresh
// database has exactly one row in the nodes table, and that row carries
// the OpenOptions.NodeName the caller supplied (no synthetic peers).
func TestFreshV6SelfNodeRow(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenWithOptions(dsn, OpenOptions{NodeName: "laptop"})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes`).Scan(&count); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if count != 1 {
		t.Fatalf("nodes count = %d, want 1 (only the self row)", count)
	}
	var name string
	var endpoint, fp sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT name, endpoint, public_key_fingerprint FROM nodes`).Scan(&name, &endpoint, &fp); err != nil {
		t.Fatalf("read self row: %v", err)
	}
	if name != "laptop" {
		t.Fatalf("self node name = %q, want laptop", name)
	}
	if endpoint.Valid {
		t.Fatalf("endpoint = %+v, want NULL on self row", endpoint)
	}
	if fp.Valid {
		t.Fatalf("public_key_fingerprint = %+v, want NULL on v1 self row", fp)
	}
}

// TestOpenRejectsInvalidNodeName guards the identifier rule that the
// config layer also enforces — programmatic callers that bypass config
// should still be unable to seed a self row with a name later layers
// (sync wire, rclone.conf section, destination subfolder) would reject.
func TestOpenRejectsInvalidNodeName(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	_, err := OpenWithOptions(dsn, OpenOptions{NodeName: "has spaces"})
	if err == nil {
		t.Fatalf("OpenWithOptions accepted invalid node name; want rejection")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("error %q does not mention invalid identifier", err)
	}
}

// TestSanitiseNodeName covers the deterministic mapping from a raw
// hostname to a nodeNameRE-compliant identifier. The hostname fallback
// uses this so a real-world "laptop.local"-style host doesn't fail to
// open at the validator.
func TestSanitiseNodeName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"laptop", "laptop"},
		{"laptop.local", "laptop-local"},
		{"my host", "my-host"},
		{"---foo", "foo"},
		{"1abc", "1abc"},
		{"", ""},
		{"...", ""},
	}
	for _, c := range cases {
		got := sanitiseNodeName(c.in)
		if got != c.want {
			t.Fatalf("sanitiseNodeName(%q) = %q, want %q", c.in, got, c.want)
		}
		if got != "" && !nodeNameRE.MatchString(got) {
			t.Fatalf("sanitised %q = %q does not match nodeNameRE", c.in, got)
		}
	}
}

// TestFallbackNodeNameDeterministic covers the empty/pathological
// hostname path (L2): rather than erroring when a hostname sanitises to
// nothing, the store derives a stable, valid identifier. The test points
// machineIDPath at a temp file so it doesn't depend on the host actually
// having /etc/machine-id, and asserts the result is regex-compliant,
// reproducible, and seeded from the machine id (not the empty hostname).
func TestFallbackNodeNameDeterministic(t *testing.T) {
	idFile := filepath.Join(t.TempDir(), "machine-id")
	if err := os.WriteFile(idFile, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatalf("seed machine-id: %v", err)
	}
	orig := machineIDPath
	machineIDPath = idFile
	defer func() { machineIDPath = orig }()

	// Empty hostname is the canonical "nothing usable" input. The
	// fallback must still produce a non-empty, valid id.
	got := fallbackNodeName("")
	if !nodeNameRE.MatchString(got) {
		t.Fatalf("fallbackNodeName(%q) = %q does not match nodeNameRE", "", got)
	}
	if got != fallbackNodeName("a-totally-different-host") {
		t.Fatalf("fallback id depends on hostname; want it seeded from machine-id alone")
	}
	if got != "node-"+shortHashHex("0123456789abcdef0123456789abcdef") {
		t.Fatalf("fallback id %q not derived from the trimmed machine-id", got)
	}
}

// TestFallbackNodeNameHashesHostnameWithoutMachineID confirms the second
// tier: when machineIDPath is unreadable the fallback hashes the raw
// hostname so the id is at least stable per host, and remains valid.
func TestFallbackNodeNameHashesHostnameWithoutMachineID(t *testing.T) {
	orig := machineIDPath
	machineIDPath = filepath.Join(t.TempDir(), "does-not-exist")
	defer func() { machineIDPath = orig }()

	got := fallbackNodeName("...")
	if !nodeNameRE.MatchString(got) {
		t.Fatalf("fallbackNodeName(%q) = %q does not match nodeNameRE", "...", got)
	}
	if got != "node-"+shortHashHex("...") {
		t.Fatalf("fallback id %q not derived from the raw hostname", got)
	}
	if got == fallbackNodeName("other-host") {
		t.Fatalf("distinct hostnames collided without a machine id")
	}
}

// TestOpenHostnameFallback exercises the empty-NodeName path: when no
// explicit name is supplied the migration seeds the self row from
// os.Hostname() so the table is never left empty.
func TestOpenHostnameFallback(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	host, err := os.Hostname()
	if err != nil {
		t.Skipf("hostname unavailable: %v", err)
	}
	want := sanitiseNodeName(host)
	if want == "" {
		t.Skipf("hostname %q sanitises to empty; nothing to compare", host)
	}
	var name string
	if err := s.db.QueryRowContext(ctx, `SELECT name FROM nodes`).Scan(&name); err != nil {
		t.Fatalf("read self row: %v", err)
	}
	if name != want {
		t.Fatalf("self node name = %q, want sanitised hostname %q (raw %q)", name, want, host)
	}
}

// TestMigrateV5ToV6 builds a v5-shape database by hand, populates it with
// a file row plus an index run, then opens it via Open() to drive the
// v5→v6 step. The migration must (a) leave files rows untouched except
// for the two NULL provenance columns, (b) leave runs rows untouched
// except for the two NULL peer columns, (c) create the nodes table with
// a single self row, (d) create the peer_sync_state table, and (e) end
// with schema_version = 6.
func TestMigrateV5ToV6(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	v5DDL := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE runs (
			id            INTEGER PRIMARY KEY,
			kind          TEXT NOT NULL CHECK (kind IN ('index','sync','restore')),
			volume_id     INTEGER REFERENCES volumes(id),
			destination   TEXT,
			started_at_ns INTEGER NOT NULL,
			ended_at_ns   INTEGER,
			status        TEXT NOT NULL CHECK (status IN ('running','success','failed','partial')),
			error         TEXT,
			file_count    INTEGER NOT NULL DEFAULT 0,
			CHECK (
				(kind = 'index' AND destination IS NULL) OR
				(kind IN ('sync','restore') AND destination IS NOT NULL AND destination != '')
			)
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
			PRIMARY KEY (volume_id, path, blake3)
		)`,
		`INSERT INTO schema_version (version) VALUES (5)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'photos', '/photos')`,
		`INSERT INTO runs (id, kind, volume_id, started_at_ns, status, file_count)
		 VALUES (1, 'index', 1, 100, 'success', 1)`,
	}
	for _, q := range v5DDL {
		if _, err := rawDB.Exec(q); err != nil {
			t.Fatalf("v5 DDL %q: %v", q, err)
		}
	}
	d := digest(0xab)
	if _, err := rawDB.Exec(
		`INSERT INTO files (volume_id, path, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns)
		 VALUES (1, 'a.jpg', ?, 10, 50, 'present', 1, 1, 50)`, d,
	); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	rawDB.Close()

	s, err := OpenWithOptions(dsn, OpenOptions{NodeName: "nas"})
	if err != nil {
		t.Fatalf("OpenWithOptions (should migrate v5→v6): %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if v, _ := s.CurrentSchemaVersion(ctx); v != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", v, SchemaVersion)
	}

	row, err := s.GetByPath(ctx, 1, "a.jpg")
	if err != nil {
		t.Fatalf("GetByPath after migration: %v", err)
	}
	if !bytes.Equal(row.Blake3, d) || row.SizeBytes != 10 || row.Status != StatusPresent {
		t.Fatalf("file row mangled by migration: %+v", row)
	}
	if row.OriginNodeID.Valid || row.OriginRunID.Valid {
		t.Fatalf("migrated row has non-NULL provenance %+v / %+v, want NULL",
			row.OriginNodeID, row.OriginRunID)
	}

	var peerNode, correlated sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT peer_node_id, correlated_run_id FROM runs WHERE id = 1`).Scan(&peerNode, &correlated); err != nil {
		t.Fatalf("read migrated run: %v", err)
	}
	if peerNode.Valid || correlated.Valid {
		t.Fatalf("migrated run has non-NULL peer columns %+v / %+v, want NULL",
			peerNode, correlated)
	}

	var nodeCount, peerStateCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes`).Scan(&nodeCount); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if nodeCount != 1 {
		t.Fatalf("nodes count = %d, want 1", nodeCount)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM peer_sync_state`).Scan(&peerStateCount); err != nil {
		t.Fatalf("count peer_sync_state: %v", err)
	}
	if peerStateCount != 0 {
		t.Fatalf("peer_sync_state count = %d, want 0 (PR 3 populates it)", peerStateCount)
	}
	var selfName string
	if err := s.db.QueryRowContext(ctx, `SELECT name FROM nodes`).Scan(&selfName); err != nil {
		t.Fatalf("read self row: %v", err)
	}
	if selfName != "nas" {
		t.Fatalf("self name = %q, want 'nas'", selfName)
	}
}

// TestUpsertWithProvenance verifies that a non-nil *Provenance lands as
// the new content's (origin_node_id, origin_run_id), that a subsequent
// local overwrite supersedes the prior row, and that the supersede flow
// itself is unchanged (the prior content's origin survives on the
// historical record).
func TestUpsertWithProvenance(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenWithOptions(dsn, OpenOptions{NodeName: "local"})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	// Insert a peer node so its id is FK-valid.
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO nodes (name, endpoint) VALUES ('peer', 'https://peer.example')`)
	if err != nil {
		t.Fatalf("insert peer node: %v", err)
	}
	peerID, _ := res.LastInsertId()

	vID := makeVolume(t, s, "/v")
	run1 := makeRun(t, s, vID)
	run2 := makeRun(t, s, vID)

	prov1 := &Provenance{NodeID: peerID, RunID: run1}
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "doc.txt", Blake3: digest(0xaa), SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: run1, LastSeenRunID: run1, IndexedAtNs: 1,
	}, prov1); err != nil {
		t.Fatalf("Upsert with prov1: %v", err)
	}
	live, err := s.GetByPath(ctx, vID, "doc.txt")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if !live.OriginNodeID.Valid || live.OriginNodeID.Int64 != peerID {
		t.Fatalf("OriginNodeID = %+v, want %d", live.OriginNodeID, peerID)
	}
	if !live.OriginRunID.Valid || live.OriginRunID.Int64 != run1 {
		t.Fatalf("OriginRunID = %+v, want %d", live.OriginRunID, run1)
	}

	// New content + nil provenance — supersede the peer-sourced row with a
	// local write. The supersede flow must still preserve the prior row's
	// provenance on the historical record (we read the superseded row to
	// confirm).
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "doc.txt", Blake3: digest(0xbb), SizeBytes: 2, MtimeNs: 2,
		Status: StatusPresent, FirstSeenRunID: run2, LastSeenRunID: run2, IndexedAtNs: 2,
	}, nil); err != nil {
		t.Fatalf("Upsert with nil prov: %v", err)
	}
	history, err := s.ListHistoryByPath(ctx, vID, "doc.txt")
	if err != nil {
		t.Fatalf("ListHistoryByPath: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history len = %d, want 2", len(history))
	}
	old, newRow := history[0], history[1]
	if old.Status != StatusSuperseded || !bytes.Equal(old.Blake3, digest(0xaa)) {
		t.Fatalf("old row = %+v, want hashA superseded", old)
	}
	if !old.OriginNodeID.Valid || old.OriginNodeID.Int64 != peerID {
		t.Fatalf("superseded row lost provenance: %+v", old.OriginNodeID)
	}
	if newRow.Status != StatusPresent || !bytes.Equal(newRow.Blake3, digest(0xbb)) {
		t.Fatalf("new row = %+v, want hashB present", newRow)
	}
	if newRow.OriginNodeID.Valid || newRow.OriginRunID.Valid {
		t.Fatalf("local-write row has non-NULL provenance: %+v / %+v",
			newRow.OriginNodeID, newRow.OriginRunID)
	}
}

// TestUpsertProvenanceFK pins the FK shape of the contents origin
// columns: origin_node_id is a real FK (a bogus node id must fail
// rather than land a dangling reference), while origin_run_id is in the
// origin node's run space and deliberately not FK-bound to local runs —
// a run id with no local row must be accepted.
func TestUpsertProvenanceFK(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	run := makeRun(t, s, vID)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO nodes (name, endpoint) VALUES ('peer', 'https://peer.example')`)
	if err != nil {
		t.Fatalf("insert peer node: %v", err)
	}
	peerID, _ := res.LastInsertId()

	mkRow := func(path string, hash byte) FileRow {
		return FileRow{
			VolumeID: vID, Path: path, Blake3: digest(hash), SizeBytes: 1, MtimeNs: 1,
			Status: StatusPresent, FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1,
		}
	}
	if err := s.Upsert(ctx, mkRow("x-node", 0x10), &Provenance{NodeID: 99999, RunID: run}); err == nil {
		t.Fatalf("Upsert with bogus node id succeeded; FK not enforced")
	}
	if err := s.Upsert(ctx, mkRow("x-run", 0x11), &Provenance{NodeID: peerID, RunID: 99999}); err != nil {
		t.Fatalf("Upsert with foreign-space run id rejected: %v (origin_run_id must not be a local FK)", err)
	}
	row, err := s.GetByPath(ctx, vID, "x-run")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if !row.OriginRunID.Valid || row.OriginRunID.Int64 != 99999 {
		t.Fatalf("OriginRunID = %+v, want 99999", row.OriginRunID)
	}
}

// TestPeerSyncStateAcceptsForeignRunID guards the design contract that
// peer_sync_state.last_shared_run_id is not FK-bound to the local
// runs(id) — the value carries the initiator's local id, which on the
// receiver is recorded as runs.correlated_run_id, not as a local run.
// FK-constraining it would reject the watermark update on most peer
// syncs.
func TestPeerSyncStateAcceptsForeignRunID(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO nodes (name, endpoint) VALUES ('peer', 'https://peer.example')`)
	if err != nil {
		t.Fatalf("insert peer node: %v", err)
	}
	peerID, _ := res.LastInsertId()

	// 99999 is not a local runs.id — must still insert.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO peer_sync_state (volume_id, peer_node_id, last_shared_run_id, last_synced_at)
		VALUES (?, ?, ?, ?)
	`, vID, peerID, 99999, NowNs()); err != nil {
		t.Fatalf("peer_sync_state insert with foreign run id rejected: %v", err)
	}
}

// TestPartialIndexOnOriginNodeExists verifies the partial index backing
// ListPresentByOrigin: contents(origin_node_id) WHERE origin_node_id IS
// NOT NULL, excluding the locally-introduced majority.
func TestPartialIndexOnOriginNodeExists(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	var ddl string
	err = s.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_contents_origin_node'`).Scan(&ddl)
	if err != nil {
		t.Fatalf("look up partial index: %v", err)
	}
	for _, want := range []string{"origin_node_id", "origin_node_id IS NOT NULL"} {
		if !strings.Contains(ddl, want) {
			t.Fatalf("idx_contents_origin_node SQL = %q, missing %q (partial index must exclude local-origin NULLs)", ddl, want)
		}
	}
}

// TestPartialIndexAbsentOnV5 builds a v5-shape DB by hand without opening
// it through the migration, and confirms idx_files_source_node is not
// present — the index is a v6 artifact, not a v5 one.
func TestPartialIndexAbsentOnV5(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	defer rawDB.Close()
	v5DDL := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE runs (
			id            INTEGER PRIMARY KEY,
			kind          TEXT NOT NULL CHECK (kind IN ('index','sync','restore')),
			volume_id     INTEGER REFERENCES volumes(id),
			destination   TEXT,
			started_at_ns INTEGER NOT NULL,
			ended_at_ns   INTEGER,
			status        TEXT NOT NULL CHECK (status IN ('running','success','failed','partial')),
			error         TEXT,
			file_count    INTEGER NOT NULL DEFAULT 0
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
			PRIMARY KEY (volume_id, path, blake3)
		)`,
		`INSERT INTO schema_version (version) VALUES (5)`,
	}
	for _, q := range v5DDL {
		if _, err := rawDB.Exec(q); err != nil {
			t.Fatalf("v5 DDL %q: %v", q, err)
		}
	}
	var name sql.NullString
	row := rawDB.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_files_source_node'`)
	if err := row.Scan(&name); err == nil {
		t.Fatalf("idx_files_source_node found on v5; expected migration to add it on v6")
	}
}

// TestRunMigrationsAppliesInOrder exercises the registry mechanism with a
// custom slice of fake migrations on a fresh DB at the v5 baseline. Each
// fake migration appends its version to a side table and bumps
// schema_version; the test asserts both ran in order and the version row
// advanced after each step (matching the per-migration atomicity contract).
func TestRunMigrationsAppliesInOrder(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	stmts := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE migration_trace (step INTEGER NOT NULL, version_at_entry INTEGER NOT NULL)`,
		`INSERT INTO schema_version (version) VALUES (5)`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}

	fake := []migration{
		{version: 100, up: func(ctx context.Context, db *sql.DB) error {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			var maxV int
			if err := tx.QueryRowContext(ctx,
				`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&maxV); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO migration_trace (step, version_at_entry) VALUES (100, ?)`, maxV); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (100)`); err != nil {
				return err
			}
			return tx.Commit()
		}},
		{version: 101, up: func(ctx context.Context, db *sql.DB) error {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			var maxV int
			if err := tx.QueryRowContext(ctx,
				`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&maxV); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO migration_trace (step, version_at_entry) VALUES (101, ?)`, maxV); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (101)`); err != nil {
				return err
			}
			return tx.Commit()
		}},
	}

	end, err := runMigrations(ctx, db, 5, fake)
	if err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if end != 101 {
		t.Fatalf("end version = %d, want 101", end)
	}

	rows, err := db.QueryContext(ctx,
		`SELECT step, version_at_entry FROM migration_trace ORDER BY rowid`)
	if err != nil {
		t.Fatalf("query trace: %v", err)
	}
	defer rows.Close()
	type traceRow struct{ step, entry int }
	var got []traceRow
	for rows.Next() {
		var r traceRow
		if err := rows.Scan(&r.step, &r.entry); err != nil {
			t.Fatalf("scan trace: %v", err)
		}
		got = append(got, r)
	}
	want := []traceRow{{100, 5}, {101, 100}}
	if len(got) != len(want) {
		t.Fatalf("trace = %+v, want %+v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("trace[%d] = %+v, want %+v", i, got[i], w)
		}
	}

	var finalV int
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_version`).Scan(&finalV); err != nil {
		t.Fatalf("read final version: %v", err)
	}
	if finalV != 101 {
		t.Fatalf("schema_version max = %d, want 101", finalV)
	}
}

// TestRunMigrationsSkipsAlreadyApplied verifies that migrations whose
// version is <= current are skipped — the loop's idempotency guarantee.
func TestRunMigrationsSkipsAlreadyApplied(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	var ran []int
	ms := []migration{
		{version: 5, up: func(_ context.Context, _ *sql.DB) error { ran = append(ran, 5); return nil }},
		{version: 6, up: func(_ context.Context, _ *sql.DB) error { ran = append(ran, 6); return nil }},
		{version: 7, up: func(_ context.Context, _ *sql.DB) error { ran = append(ran, 7); return nil }},
	}
	end, err := runMigrations(context.Background(), db, 6, ms)
	if err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if end != 7 {
		t.Fatalf("end version = %d, want 7", end)
	}
	if len(ran) != 1 || ran[0] != 7 {
		t.Fatalf("ran = %v, want [7]", ran)
	}
}

// TestRunMigrationsRejectsOutOfOrder confirms the safety net in
// runMigrations that catches a registry slice with non-ascending versions
// before any harm is done.
func TestRunMigrationsRejectsOutOfOrder(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	ms := []migration{
		{version: 6, up: func(_ context.Context, _ *sql.DB) error { return nil }},
		{version: 5, up: func(_ context.Context, _ *sql.DB) error { return nil }},
	}
	if _, err := runMigrations(context.Background(), db, 0, ms); err == nil {
		t.Fatalf("runMigrations accepted out-of-order registry; want error")
	}
}

// TestBuildMigrationsAscending pins the production registry's contract:
// versions strictly ascend. A future migration that accidentally lands
// before its predecessor in the slice trips this test instead of
// surfacing as a confusing migration error at runtime.
func TestBuildMigrationsAscending(t *testing.T) {
	ms := buildMigrations(migrationCtx{nodeName: "test"})
	prev := -1
	for _, m := range ms {
		if m.version <= prev {
			t.Fatalf("buildMigrations not ascending: v%d follows v%d", m.version, prev)
		}
		prev = m.version
	}
}

// TestFileRowScanInsertRoundTrip locks in the column-order invariant
// between the files INSERT shape, scanFrom, and the JOIN-based SELECT
// projection. A row written via Upsert and read back via GetByPath must
// equal the original. Adding a column without updating every helper would
// surface here as a Scan arity mismatch or a field whose value lands in
// the wrong slot.
func TestFileRowScanInsertRoundTrip(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	volID := makeVolume(t, s, "/photos")
	runID := makeRun(t, s, volID)
	peerNode, err := s.GetOrCreatePeerNode(ctx, "peer-x", "https://peer-x.example", true)
	if err != nil {
		t.Fatalf("GetOrCreatePeerNode: %v", err)
	}
	peerNodeID := peerNode.ID

	want := FileRow{
		VolumeID:       volID,
		Path:           "a/b/c.jpg",
		Blake3:         digest(0x42),
		SizeBytes:      4242,
		MtimeNs:        1_700_000_000_000_000_000,
		Status:         StatusPresent,
		FirstSeenRunID: runID,
		LastSeenRunID:  runID,
		IndexedAtNs:    1_700_000_500_000_000_000,
		OriginNodeID:   sql.NullInt64{Int64: peerNodeID, Valid: true},
		OriginRunID:    sql.NullInt64{Int64: runID, Valid: true},
	}

	if err := s.Upsert(ctx, want, &Provenance{NodeID: want.OriginNodeID.Int64, RunID: want.OriginRunID.Int64}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := s.GetByPath(ctx, want.VolumeID, want.Path)
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}

	if got.VolumeID != want.VolumeID || got.Path != want.Path ||
		!bytes.Equal(got.Blake3, want.Blake3) ||
		got.SizeBytes != want.SizeBytes || got.MtimeNs != want.MtimeNs ||
		got.Status != want.Status ||
		got.FirstSeenRunID != want.FirstSeenRunID || got.LastSeenRunID != want.LastSeenRunID ||
		got.IndexedAtNs != want.IndexedAtNs ||
		got.OriginNodeID != want.OriginNodeID || got.OriginRunID != want.OriginRunID {
		t.Fatalf("round-trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

// TestRecordConflictPreStageAtomic exercises the supersede + insert
// transaction used by the receiver's conflict pre-stage: the live row
// at the original path goes 'superseded' and a new 'present' row
// appears at the conflict path carrying the prior blake3 + the
// supplied provenance. Both halves must be visible together — the
// transaction guarantee is what protects against an agent crash
// between the two updates.
func TestRecordConflictPreStageAtomic(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	volID := makeVolume(t, s, "/v")
	runID := makeRun(t, s, volID)
	priorBlake3 := digest(0x01)

	if err := s.Upsert(ctx, FileRow{
		VolumeID: volID, Path: "doc.md", Blake3: priorBlake3,
		SizeBytes: 5, MtimeNs: 100, Status: StatusPresent,
		FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: 100,
	}, nil); err != nil {
		t.Fatalf("seed live row: %v", err)
	}

	conflictRow := FileRow{
		VolumeID: volID, Path: ".squirrel-conflicts/run-9/doc.md", Blake3: priorBlake3,
		SizeBytes: 5, MtimeNs: 100, Status: StatusPresent,
		FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: 200,
	}
	if err := s.RecordConflictPreStage(ctx, volID, "doc.md", conflictRow, nil); err != nil {
		t.Fatalf("RecordConflictPreStage: %v", err)
	}

	// Original path no longer has a live row.
	if _, err := s.GetByPath(ctx, volID, "doc.md"); !IsNotFound(err) {
		t.Fatalf("GetByPath(doc.md) after pre-stage = %v, want NotFound", err)
	}
	// Conflict path has a present row carrying the prior blake3.
	got, err := s.GetByPath(ctx, volID, ".squirrel-conflicts/run-9/doc.md")
	if err != nil {
		t.Fatalf("GetByPath(conflict): %v", err)
	}
	if !bytes.Equal(got.Blake3, priorBlake3) {
		t.Fatalf("conflict-path blake3 = %x, want %x", got.Blake3, priorBlake3)
	}
	if got.Status != StatusPresent {
		t.Fatalf("conflict-path status = %q, want present", got.Status)
	}

	// Prior blake3 is reachable by hash: GetByBlake3 returns both
	// rows (one present at conflict path, one superseded at original
	// path), which is the append-only history contract.
	matches, err := s.GetByBlake3(ctx, priorBlake3)
	if err != nil {
		t.Fatalf("GetByBlake3: %v", err)
	}
	byPath := make(map[string]string, len(matches))
	for _, m := range matches {
		byPath[m.File.Path] = m.File.Status
	}
	if byPath[".squirrel-conflicts/run-9/doc.md"] != StatusPresent {
		t.Fatalf("conflict-path row status = %q, want present (matches=%+v)",
			byPath[".squirrel-conflicts/run-9/doc.md"], byPath)
	}
	if byPath["doc.md"] != StatusSuperseded {
		t.Fatalf("original-path row status = %q, want superseded (matches=%+v)",
			byPath["doc.md"], byPath)
	}
}

// TestCountFilesFirstSeenByRunWithPathPrefix exercises the grouped
// path-prefix counter `squirrel runs` uses to derive each peer-sync
// run's conflict count in a single query. Rows under the prefix
// should be summed per-run; rows outside the prefix (different
// first-seen run, or unrelated path) must not contribute. An empty
// runIDs slice short-circuits to an empty result without a query.
func TestCountFilesFirstSeenByRunWithPathPrefix(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	volID := makeVolume(t, s, "/v")
	thisRun := makeRun(t, s, volID)
	otherRun := makeRun(t, s, volID)
	emptyRun := makeRun(t, s, volID)

	for i, p := range []string{".squirrel-conflicts/run-1/a", ".squirrel-conflicts/run-1/sub/b"} {
		if err := s.Upsert(ctx, FileRow{
			VolumeID: volID, Path: p, Blake3: digest(byte(0x10 + i)),
			SizeBytes: 1, MtimeNs: 1, Status: StatusPresent,
			FirstSeenRunID: thisRun, LastSeenRunID: thisRun, IndexedAtNs: 1,
		}, nil); err != nil {
			t.Fatalf("upsert %s: %v", p, err)
		}
	}
	// Another run, same prefix → counted under that run's id.
	if err := s.Upsert(ctx, FileRow{
		VolumeID: volID, Path: ".squirrel-conflicts/run-2/x", Blake3: digest(0x99),
		SizeBytes: 1, MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: otherRun, LastSeenRunID: otherRun, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("upsert other-run row: %v", err)
	}
	// Same-run row outside the prefix → must not contribute.
	if err := s.Upsert(ctx, FileRow{
		VolumeID: volID, Path: "unrelated.txt", Blake3: digest(0xEE),
		SizeBytes: 1, MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: thisRun, LastSeenRunID: thisRun, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("upsert outside-prefix row: %v", err)
	}

	counts, err := s.CountFilesFirstSeenByRunWithPathPrefix(ctx,
		[]int64{thisRun, otherRun, emptyRun}, ".squirrel-conflicts")
	if err != nil {
		t.Fatalf("CountFilesFirstSeenByRunWithPathPrefix: %v", err)
	}
	if counts[thisRun] != 2 {
		t.Fatalf("counts[thisRun] = %d, want 2", counts[thisRun])
	}
	if counts[otherRun] != 1 {
		t.Fatalf("counts[otherRun] = %d, want 1", counts[otherRun])
	}
	if _, ok := counts[emptyRun]; ok {
		t.Fatalf("counts[emptyRun] present (%d), want absent", counts[emptyRun])
	}

	empty, err := s.CountFilesFirstSeenByRunWithPathPrefix(ctx, nil, ".squirrel-conflicts")
	if err != nil {
		t.Fatalf("empty input: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty input returned %v, want empty map", empty)
	}
}

// TestListPresentByOrigin pins the two filter modes: valid nodeID
// returns rows attributed to that peer, NULL nodeID returns rows
// without provenance (local writes). Superseded and missing rows
// must be excluded under either mode.
func TestListPresentByOrigin(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenWithOptions(dsn, OpenOptions{NodeName: "self"})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	peerA, err := s.CreateNode(ctx, "peer-a", "https://a.example")
	if err != nil {
		t.Fatalf("CreateNode peer-a: %v", err)
	}
	peerB, err := s.CreateNode(ctx, "peer-b", "https://b.example")
	if err != nil {
		t.Fatalf("CreateNode peer-b: %v", err)
	}

	vID := makeVolume(t, s, "/v")
	run := makeRun(t, s, vID)

	upsert := func(p string, b byte, status string, prov *Provenance) {
		t.Helper()
		if err := s.Upsert(ctx, FileRow{
			VolumeID: vID, Path: p, Blake3: digest(b), SizeBytes: 1, MtimeNs: 1,
			Status: status, FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1,
		}, prov); err != nil {
			t.Fatalf("Upsert %s: %v", p, err)
		}
	}
	upsert("a-from-peer-a.txt", 0x11, StatusPresent, &Provenance{NodeID: peerA.ID, RunID: run})
	upsert("b-from-peer-a.txt", 0x12, StatusPresent, &Provenance{NodeID: peerA.ID, RunID: run})
	upsert("c-from-peer-b.txt", 0x21, StatusPresent, &Provenance{NodeID: peerB.ID, RunID: run})
	upsert("local-write.txt", 0x31, StatusPresent, nil)
	upsert("missing-from-peer-a.txt", 0x41, StatusMissing, &Provenance{NodeID: peerA.ID, RunID: run})

	collect := func(nodeID sql.NullInt64) []string {
		var got []string
		for row, err := range s.ListPresentByOrigin(ctx, vID, nodeID) {
			if err != nil {
				t.Fatalf("iter: %v", err)
			}
			got = append(got, row.Path)
		}
		return got
	}

	peerAOnly := collect(sql.NullInt64{Int64: peerA.ID, Valid: true})
	wantA := []string{"a-from-peer-a.txt", "b-from-peer-a.txt"}
	if fmt.Sprint(peerAOnly) != fmt.Sprint(wantA) {
		t.Fatalf("peer-a paths = %v, want %v", peerAOnly, wantA)
	}

	peerBOnly := collect(sql.NullInt64{Int64: peerB.ID, Valid: true})
	if fmt.Sprint(peerBOnly) != fmt.Sprint([]string{"c-from-peer-b.txt"}) {
		t.Fatalf("peer-b paths = %v", peerBOnly)
	}

	localOnly := collect(sql.NullInt64{})
	if fmt.Sprint(localOnly) != fmt.Sprint([]string{"local-write.txt"}) {
		t.Fatalf("local (NULL) paths = %v", localOnly)
	}
}

// TestListPresentByOriginEarlyBreakClosesRows confirms the iter.Seq2
// implementation closes its underlying rows when the consumer breaks
// early. Without this guarantee a long-running CLI could leak a
// statement handle whenever the user pages results.
func TestListPresentByOriginEarlyBreakClosesRows(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenWithOptions(dsn, OpenOptions{NodeName: "self"})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	peer, _ := s.CreateNode(ctx, "peer", "https://p.example")
	vID := makeVolume(t, s, "/v")
	run := makeRun(t, s, vID)
	for i := byte(1); i <= 5; i++ {
		if err := s.Upsert(ctx, FileRow{
			VolumeID: vID, Path: fmt.Sprintf("p%d", i), Blake3: digest(i), SizeBytes: 1, MtimeNs: 1,
			Status: StatusPresent, FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1,
		}, &Provenance{NodeID: peer.ID, RunID: run}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	var seen int
	for _, err := range s.ListPresentByOrigin(ctx, vID, sql.NullInt64{Int64: peer.ID, Valid: true}) {
		if err != nil {
			t.Fatalf("iter: %v", err)
		}
		seen++
		if seen == 2 {
			break
		}
	}
	if seen != 2 {
		t.Fatalf("seen = %d, want 2", seen)
	}
	// A second pass must succeed, proving the prior pass released its
	// rows handle (the store pins MaxOpenConns=1, so a leak would block
	// the next QueryContext indefinitely — guard with a separate scan).
	again := 0
	for _, err := range s.ListPresentByOrigin(ctx, vID, sql.NullInt64{Int64: peer.ID, Valid: true}) {
		if err != nil {
			t.Fatalf("second iter: %v", err)
		}
		again++
	}
	if again != 5 {
		t.Fatalf("second pass = %d rows, want 5", again)
	}
}

// TestListRunsByPeer pins peer-id filtering, ordering, and the limit
// argument's behaviour. Index runs and bucket-sync runs must be
// excluded entirely.
func TestListRunsByPeer(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenWithOptions(dsn, OpenOptions{NodeName: "self"})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	peerA, _ := s.CreateNode(ctx, "peer-a", "https://a.example")
	peerB, _ := s.CreateNode(ctx, "peer-b", "https://b.example")
	vID := makeVolume(t, s, "/v")

	// Two peer-sync runs for peer-a, one for peer-b, one bucket sync.
	r1, _ := s.BeginPeerSyncRun(ctx, vID, peerA.ID, 101, "peer-a")
	r2, _ := s.BeginPeerSyncRun(ctx, vID, peerA.ID, 102, "peer-a")
	r3, _ := s.BeginPeerSyncRun(ctx, vID, peerB.ID, 201, "peer-b")
	bucketRun, _ := s.BeginRun(ctx, RunKindSync, vID, "scratch", false)
	_ = bucketRun
	_ = makeRun(t, s, vID) // an index run, to confirm it's excluded too
	_ = r3

	runs, err := s.ListRunsByPeer(ctx, peerA.ID, 0)
	if err != nil {
		t.Fatalf("ListRunsByPeer: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2", len(runs))
	}
	// Descending id order: r2 first.
	if runs[0].ID != r2 || runs[1].ID != r1 {
		t.Fatalf("ordering: got [%d, %d], want [%d, %d]", runs[0].ID, runs[1].ID, r2, r1)
	}
	for _, r := range runs {
		if !r.PeerNodeID.Valid || r.PeerNodeID.Int64 != peerA.ID {
			t.Fatalf("row has wrong peer: %+v", r)
		}
	}

	capped, err := s.ListRunsByPeer(ctx, peerA.ID, 1)
	if err != nil {
		t.Fatalf("ListRunsByPeer cap: %v", err)
	}
	if len(capped) != 1 || capped[0].ID != r2 {
		t.Fatalf("limit=1 returned %+v, want only r2 (%d)", capped, r2)
	}
}

// TestMigrateV6ToV7AddsAuditKind builds a v6-shape database by hand,
// populates it with file and run rows, then opens it via Open() to
// drive the v6→v7 step. The migration must (a) carry every existing
// runs row through verbatim, (b) widen the kind CHECK so an 'audit'
// row inserts cleanly, (c) keep the kind↔destination coupling intact
// (audit shares the index branch — destination must be NULL), and
// (d) leave the files FK to runs(id) resolvable.
func TestMigrateV6ToV7AddsAuditKind(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	v6DDL := []string{
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
			kind          TEXT NOT NULL CHECK (kind IN ('index','sync','restore')),
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
				(kind = 'index' AND destination IS NULL) OR
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
		`INSERT INTO schema_version (version) VALUES (6)`,
		`INSERT INTO nodes (name, endpoint, public_key_fingerprint) VALUES ('self', NULL, NULL)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'photos', '/photos')`,
		`INSERT INTO runs (id, kind, volume_id, started_at_ns, status, file_count)
		 VALUES (1, 'index', 1, 100, 'success', 1)`,
		`INSERT INTO runs (id, kind, volume_id, destination, started_at_ns, status, file_count)
		 VALUES (2, 'sync', 1, 'nas', 200, 'success', 1)`,
	}
	for _, q := range v6DDL {
		if _, err := rawDB.Exec(q); err != nil {
			t.Fatalf("v6 DDL %q: %v", q, err)
		}
	}
	if _, err := rawDB.Exec(
		`INSERT INTO files (volume_id, path, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns)
		 VALUES (1, 'a.jpg', ?, 10, 50, 'present', 1, 1, 50)`, digest(0xab),
	); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	rawDB.Close()

	s, err := OpenWithOptions(dsn, OpenOptions{NodeName: "nas"})
	if err != nil {
		t.Fatalf("OpenWithOptions (should migrate v6→v7): %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if v, _ := s.CurrentSchemaVersion(ctx); v != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", v, SchemaVersion)
	}

	// Existing rows carried over verbatim.
	var sawIndex, sawSync bool
	runs, err := s.ListRuns(ctx, ListRunsOpts{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	for _, r := range runs {
		if r.ID == 1 && r.Kind == "index" {
			sawIndex = true
		}
		if r.ID == 2 && r.Kind == "sync" && r.Destination.Valid && r.Destination.String == "nas" {
			sawSync = true
		}
	}
	if !sawIndex || !sawSync {
		t.Fatalf("post-migration runs missing rows: %+v", runs)
	}

	// FK from files.last_seen_run_id → runs(id) still resolves.
	row, err := s.GetByPath(ctx, 1, "a.jpg")
	if err != nil {
		t.Fatalf("GetByPath after migration: %v", err)
	}
	if row.LastSeenRunID != 1 {
		t.Fatalf("LastSeenRunID = %d, want 1 (FK preserved)", row.LastSeenRunID)
	}

	// New 'audit' kind inserts cleanly via BeginRun, with destination NULL.
	auditID, err := s.BeginIndexRun(ctx, RunKindAudit, 1, false)
	if err != nil {
		t.Fatalf("BeginRun audit: %v", err)
	}
	r, err := s.GetRun(ctx, auditID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if r.Kind != RunKindAudit {
		t.Fatalf("audit run kind = %q, want %q", r.Kind, RunKindAudit)
	}
	if r.Destination.Valid {
		t.Fatalf("audit run destination = %+v, want NULL", r.Destination)
	}

	// audit-with-destination is rejected by the kind↔destination CHECK,
	// proving the destination-NULL branch was widened to include audit
	// (not weakened to allow audit-with-destination).
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (kind, volume_id, destination, started_at_ns, status)
		VALUES ('audit', 1, 'nas', ?, 'running')`, NowNs()); err == nil {
		t.Fatalf("INSERT audit-with-destination succeeded; want CHECK violation")
	}
}

// TestAuditRunWidensKindCheckOnFreshDB confirms the fresh-DB code path
// (no v6 baseline to migrate) lands at v7 with the audit kind already
// permitted. Catches a regression where the fresh-DB schema and the
// migrated schema drift apart.
func TestAuditRunWidensKindCheckOnFreshDB(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	if _, err := s.BeginIndexRun(ctx, RunKindAudit, vID, false); err != nil {
		t.Fatalf("BeginRun audit on fresh DB: %v", err)
	}
}

// TestListAuditRunsSince filters by (volume, sinceNs). Audit runs on
// the same volume but at or before the watermark are excluded;
// non-audit runs are excluded; runs on other volumes are excluded.
func TestListAuditRunsSince(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	otherID := makeVolume(t, s, "/other")
	// A regular index run (excluded by kind).
	_, _ = s.BeginIndexRun(ctx, RunKindIndex, vID, false)
	// Three audit runs on vID. Their started_at_ns is set by BeginRun
	// to time.Now().UnixNano(); we read it back rather than synthesise.
	r1, _ := s.BeginIndexRun(ctx, RunKindAudit, vID, false)
	row1, _ := s.GetRun(ctx, r1)
	r2, _ := s.BeginIndexRun(ctx, RunKindAudit, vID, false)
	r3, _ := s.BeginIndexRun(ctx, RunKindAudit, vID, false)
	// An audit run on a different volume — must not appear.
	_, _ = s.BeginIndexRun(ctx, RunKindAudit, otherID, false)

	got, err := s.ListAuditRunsSince(ctx, vID, 0)
	if err != nil {
		t.Fatalf("ListAuditRunsSince all: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d audit runs, want 3 (full list): %+v", len(got), got)
	}
	for i, r := range got {
		if r.Kind != RunKindAudit {
			t.Fatalf("entry %d kind = %q, want audit", i, r.Kind)
		}
	}

	// Use r1's start as a watermark — r1 itself is excluded (strict >).
	got, err = s.ListAuditRunsSince(ctx, vID, row1.StartedAtNs)
	if err != nil {
		t.Fatalf("ListAuditRunsSince since-r1: %v", err)
	}
	wantIDs := []int64{r2, r3}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d runs, want %d (since=%d)", len(got), len(wantIDs), row1.StartedAtNs)
	}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Fatalf("got[%d].ID = %d, want %d", i, got[i].ID, id)
		}
	}
}

// TestCountModifiedFilesByRun verifies the modified-count helper used
// by the drift-warning handshake path. A 'modified' file is one whose
// first_seen_run_id is the audit run AND a prior superseded row
// exists at the same (volume, path) — i.e. content changed at a
// previously-seen path. Additions (no prior superseded row) don't
// count.
func TestCountModifiedFilesByRun(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	indexRun := makeRun(t, s, vID)
	// Two files added in the index run.
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "a.txt", Blake3: digest(0x11), SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: indexRun, LastSeenRunID: indexRun, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("upsert a.txt: %v", err)
	}
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "b.txt", Blake3: digest(0x22), SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: indexRun, LastSeenRunID: indexRun, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("upsert b.txt: %v", err)
	}

	auditRun, err := s.BeginIndexRun(ctx, RunKindAudit, vID, false)
	if err != nil {
		t.Fatalf("BeginRun audit: %v", err)
	}
	// During the audit: a.txt content changes (modification), c.txt is
	// new (addition). The addition must not be counted.
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "a.txt", Blake3: digest(0xAA), SizeBytes: 2, MtimeNs: 2,
		Status: StatusPresent, FirstSeenRunID: auditRun, LastSeenRunID: auditRun, IndexedAtNs: 2,
	}, nil); err != nil {
		t.Fatalf("upsert a.txt (audit): %v", err)
	}
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "c.txt", Blake3: digest(0xCC), SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: auditRun, LastSeenRunID: auditRun, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("upsert c.txt: %v", err)
	}

	got, err := s.CountModifiedFilesByRun(ctx, auditRun)
	if err != nil {
		t.Fatalf("CountModifiedFilesByRun: %v", err)
	}
	if got != 1 {
		t.Fatalf("modified = %d, want 1 (only a.txt; c.txt is an addition)", got)
	}

	// A clean run (no modifications) returns zero.
	cleanRun, _ := s.BeginIndexRun(ctx, RunKindAudit, vID, false)
	got, err = s.CountModifiedFilesByRun(ctx, cleanRun)
	if err != nil {
		t.Fatalf("CountModifiedFilesByRun clean: %v", err)
	}
	if got != 0 {
		t.Fatalf("clean modified = %d, want 0", got)
	}
}

// TestMarkMissingStampsRunIDAndCount verifies the MarkMissing
// contract relied on by CountMissingFilesByRun: rows flipped to
// 'missing' carry last_seen_run_id = currentRunID, so the count
// helper can attribute the deletion to a specific audit run without
// an extra schema column.
func TestMarkMissingStampsRunIDAndCount(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	vID := makeVolume(t, s, "/v")
	indexRun := makeRun(t, s, vID)
	for i, path := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := s.Upsert(ctx, FileRow{
			VolumeID: vID, Path: path, Blake3: digest(byte(0xA0 + i)),
			SizeBytes: 1, MtimeNs: 1, Status: StatusPresent,
			FirstSeenRunID: indexRun, LastSeenRunID: indexRun, IndexedAtNs: 1,
		}, nil); err != nil {
			t.Fatalf("upsert %s: %v", path, err)
		}
	}

	auditRun, err := s.BeginIndexRun(ctx, RunKindAudit, vID, false)
	if err != nil {
		t.Fatalf("BeginRun audit: %v", err)
	}
	// Touch only a.txt during the audit; b.txt and c.txt should flip
	// to missing.
	if err := s.TouchSeen(ctx, vID, "a.txt", auditRun); err != nil {
		t.Fatalf("TouchSeen: %v", err)
	}
	n, err := s.MarkMissing(ctx, vID, auditRun)
	if err != nil {
		t.Fatalf("MarkMissing: %v", err)
	}
	if n != 2 {
		t.Fatalf("MarkMissing affected %d, want 2", n)
	}

	// Both b.txt and c.txt carry last_seen_run_id = auditRun now.
	for _, path := range []string{"b.txt", "c.txt"} {
		row, err := s.GetByPath(ctx, vID, path)
		if err != nil {
			t.Fatalf("GetByPath %s: %v", path, err)
		}
		if row.Status != StatusMissing {
			t.Fatalf("%s status = %q, want missing", path, row.Status)
		}
		if row.LastSeenRunID != auditRun {
			t.Fatalf("%s LastSeenRunID = %d, want %d (audit run stamps flip)",
				path, row.LastSeenRunID, auditRun)
		}
	}

	got, err := s.CountMissingFilesByRun(ctx, auditRun)
	if err != nil {
		t.Fatalf("CountMissingFilesByRun: %v", err)
	}
	if got != 2 {
		t.Fatalf("missing-by-audit = %d, want 2", got)
	}

	// A subsequent audit that touches a.txt as still-present
	// (TouchSeen advances last_seen to laterRun) and re-MarkMissings
	// the volume yields zero newly-missing rows: b.txt and c.txt are
	// already missing, and a.txt is present.
	laterRun, _ := s.BeginIndexRun(ctx, RunKindAudit, vID, false)
	if err := s.TouchSeen(ctx, vID, "a.txt", laterRun); err != nil {
		t.Fatalf("TouchSeen later: %v", err)
	}
	if _, err := s.MarkMissing(ctx, vID, laterRun); err != nil {
		t.Fatalf("MarkMissing (later): %v", err)
	}
	got, err = s.CountMissingFilesByRun(ctx, laterRun)
	if err != nil {
		t.Fatalf("CountMissingFilesByRun later: %v", err)
	}
	if got != 0 {
		t.Fatalf("later missing-by-audit = %d, want 0 (no newly-missing rows)", got)
	}
}

// TestBeginSyncRunIfClearAtomic exercises the in-progress gate at the
// store layer. Many goroutines race to start the same (volume,
// destination) sync; the BEGIN IMMEDIATE wrap inside
// BeginSyncRunIfClear must serialise them so exactly one inserts and
// the rest see the inserted row as the blocker. This is the rclone-free
// companion to TestRunPairRefusesConcurrentInvocations in the sync
// package.
func TestBeginSyncRunIfClearAtomic(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	const parallel = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		newIDs   []int64
		blockers []*Run
		execErrs []error
	)
	start := make(chan struct{})
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, blocker, err := s.BeginSyncRunIfClear(ctx, SyncRunSpec{
				VolumeID:    vID,
				Destination: "nas",
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				execErrs = append(execErrs, err)
			case blocker != nil:
				blockers = append(blockers, blocker)
			default:
				newIDs = append(newIDs, id)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(execErrs) > 0 {
		t.Fatalf("unexpected errors: %v", execErrs)
	}
	if len(newIDs) != 1 {
		t.Fatalf("inserts = %d, want exactly 1 (IDs=%v)", len(newIDs), newIDs)
	}
	if len(blockers) != parallel-1 {
		t.Fatalf("blockers = %d, want %d", len(blockers), parallel-1)
	}
	for _, b := range blockers {
		if b.ID != newIDs[0] {
			t.Fatalf("blocker id = %d, want %d (the lone inserter)", b.ID, newIDs[0])
		}
	}

	// A second pair (different destination) is independent — its slot
	// is free, so the gate lets it through.
	id2, blocker2, err := s.BeginSyncRunIfClear(ctx, SyncRunSpec{
		VolumeID:    vID,
		Destination: "scratch",
	})
	if err != nil || blocker2 != nil || id2 == 0 {
		t.Fatalf("independent (volume, destination) blocked: id=%d blocker=%+v err=%v", id2, blocker2, err)
	}

	// Finishing the first run reopens the slot.
	if err := s.FinishRun(ctx, newIDs[0], RunStatusSuccess, "", 0); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	id3, blocker3, err := s.BeginSyncRunIfClear(ctx, SyncRunSpec{
		VolumeID:    vID,
		Destination: "nas",
	})
	if err != nil || blocker3 != nil || id3 == 0 {
		t.Fatalf("post-finish reuse blocked: id=%d blocker=%+v err=%v", id3, blocker3, err)
	}
}

// TestBeginSyncRunIfClearRejectsEmptyDestination keeps callers from
// silently inserting NULL/empty destination rows that the schema CHECK
// would reject only at INSERT time. Pre-checking gives the
// "BeginSyncRunIfClear: destination must be non-empty" diagnostic
// instead of the SQLite CHECK constraint failure.
func TestBeginSyncRunIfClearRejectsEmptyDestination(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	_, _, err = s.BeginSyncRunIfClear(ctx, SyncRunSpec{VolumeID: vID, Destination: ""})
	if err == nil || !strings.Contains(err.Error(), "destination must be non-empty") {
		t.Fatalf("want destination-empty error, got %v", err)
	}
}

// TestBeginSyncRunIfClearRecordsShallow pins M1 on the production sync
// gate: a run started with Shallow:true persists shallow=1 so a forensic
// reader can tell the transfer skipped BLAKE3 verification, while
// Shallow:false (the default, full-verification) persists shallow=0.
func TestBeginSyncRunIfClearRecordsShallow(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	shallowID, _, err := s.BeginSyncRunIfClear(ctx, SyncRunSpec{
		VolumeID: vID, Destination: "nas", Shallow: true,
	})
	if err != nil {
		t.Fatalf("BeginSyncRunIfClear shallow: %v", err)
	}
	fullID, _, err := s.BeginSyncRunIfClear(ctx, SyncRunSpec{
		VolumeID: vID, Destination: "scratch", Shallow: false,
	})
	if err != nil {
		t.Fatalf("BeginSyncRunIfClear full: %v", err)
	}

	shallowRun, err := s.GetRun(ctx, shallowID)
	if err != nil {
		t.Fatalf("GetRun shallow: %v", err)
	}
	if !shallowRun.Shallow.Valid || !shallowRun.Shallow.Bool {
		t.Fatalf("shallow sync run: got %+v, want valid=true bool=true", shallowRun.Shallow)
	}
	fullRun, err := s.GetRun(ctx, fullID)
	if err != nil {
		t.Fatalf("GetRun full: %v", err)
	}
	if !fullRun.Shallow.Valid || fullRun.Shallow.Bool {
		t.Fatalf("full sync run: got %+v, want valid=true bool=false", fullRun.Shallow)
	}
}

// TestGetPresentByBlake3InVolume covers the planner's blake3-wide
// lookup used to satisfy CopyFromExisting: the query must return only
// rows in the same volume, only rows that are present, and must pick a
// deterministic source when several paths share the digest.
func TestGetPresentByBlake3InVolume(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	volA := makeVolume(t, s, "/a")
	volB := makeVolume(t, s, "/b")
	runA := makeRun(t, s, volA)
	runB := makeRun(t, s, volB)

	x := digest(0xab)

	// volA: two present rows with the same blake3 (path-order tiebreak).
	if err := s.Upsert(ctx, FileRow{
		VolumeID: volA, Path: "zeta.jpg", Blake3: x, SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: runA, LastSeenRunID: runA, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert zeta: %v", err)
	}
	if err := s.Upsert(ctx, FileRow{
		VolumeID: volA, Path: "alpha.jpg", Blake3: x, SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: runA, LastSeenRunID: runA, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert alpha: %v", err)
	}
	// volA: same blake3 but missing — must be skipped.
	if err := s.Upsert(ctx, FileRow{
		VolumeID: volA, Path: "gone.jpg", Blake3: x, SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: runA, LastSeenRunID: runA, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert gone: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE files SET status = 'missing'
		 WHERE folder_id = (SELECT id FROM folders WHERE volume_id = ? AND path = '') AND name = ?`,
		volA, "gone.jpg"); err != nil {
		t.Fatalf("flip gone to missing: %v", err)
	}
	// volB: same blake3 — must be skipped because we're scoping to volA.
	if err := s.Upsert(ctx, FileRow{
		VolumeID: volB, Path: "other.jpg", Blake3: x, SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: runB, LastSeenRunID: runB, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert other: %v", err)
	}
	// volA reserved subtree: a conflict-preservation row with the
	// same blake3. Must be skipped — dedup must not elevate a
	// conflict-preserved version back into a live user path.
	if err := s.Upsert(ctx, FileRow{
		VolumeID: volA, Path: ".squirrel-conflicts/run-1/preserved.jpg", Blake3: x,
		SizeBytes: 1, MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: runA, LastSeenRunID: runA, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert reserved: %v", err)
	}

	got, err := s.GetPresentByBlake3InVolume(ctx, volA, x)
	if err != nil {
		t.Fatalf("GetPresentByBlake3InVolume: %v", err)
	}
	if got.Path != "alpha.jpg" {
		t.Fatalf("path = %q, want alpha.jpg (deterministic path-order tiebreak)", got.Path)
	}
	if got.VolumeID != volA {
		t.Fatalf("volume = %d, want %d (volA)", got.VolumeID, volA)
	}

	// Different blake3 → not found.
	_, err = s.GetPresentByBlake3InVolume(ctx, volA, digest(0xcd))
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("want ErrNoRows for unknown digest, got %v", err)
	}
}

// TestBeginIndexRunIfClearAtomic is the index-side companion to
// TestBeginSyncRunIfClearAtomic: many goroutines racing on the same
// volume must serialise inside BEGIN IMMEDIATE so exactly one inserts
// and the rest see the inserted row as the blocker. The gate covers
// both 'index' and 'audit' kinds against each other on the same
// volume since both walk the tree and call MarkMissing.
func TestBeginIndexRunIfClearAtomic(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	const parallel = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		newIDs   []int64
		blockers []*Run
		execErrs []error
	)
	start := make(chan struct{})
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, blocker, err := s.BeginIndexRunIfClear(ctx, RunKindIndex, vID, false)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				execErrs = append(execErrs, err)
			case blocker != nil:
				blockers = append(blockers, blocker)
			default:
				newIDs = append(newIDs, id)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(execErrs) > 0 {
		t.Fatalf("unexpected errors: %v", execErrs)
	}
	if len(newIDs) != 1 {
		t.Fatalf("inserts = %d, want exactly 1 (IDs=%v)", len(newIDs), newIDs)
	}
	if len(blockers) != parallel-1 {
		t.Fatalf("blockers = %d, want %d", len(blockers), parallel-1)
	}
	for _, b := range blockers {
		if b.ID != newIDs[0] {
			t.Fatalf("blocker id = %d, want %d", b.ID, newIDs[0])
		}
	}

	// Cross-kind: a running index also blocks a fresh audit on the
	// same volume — they share the walk and MarkMissing surface.
	id2, blocker2, err := s.BeginIndexRunIfClear(ctx, RunKindAudit, vID, true)
	if err != nil {
		t.Fatalf("audit gate err: %v", err)
	}
	if blocker2 == nil || blocker2.ID != newIDs[0] {
		t.Fatalf("audit should have been blocked by the running index (got id=%d blocker=%+v)", id2, blocker2)
	}

	// A different volume is independent.
	vB := makeVolume(t, s, "/w")
	idB, blockerB, err := s.BeginIndexRunIfClear(ctx, RunKindIndex, vB, false)
	if err != nil || blockerB != nil || idB == 0 {
		t.Fatalf("independent volume blocked: id=%d blocker=%+v err=%v", idB, blockerB, err)
	}

	// Finishing the first run reopens the slot, and the next kind can
	// be audit (or index) freely.
	if err := s.FinishRun(ctx, newIDs[0], RunStatusSuccess, "", 0); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	id3, blocker3, err := s.BeginIndexRunIfClear(ctx, RunKindAudit, vID, true)
	if err != nil || blocker3 != nil || id3 == 0 {
		t.Fatalf("post-finish reuse blocked: id=%d blocker=%+v err=%v", id3, blocker3, err)
	}
}

// TestBeginIndexRunIfClearRejectsWrongKind ensures the gate refuses
// kinds that don't belong on its branch, so sync/restore callers get
// a clear diagnostic instead of an INSERT that violates the schema
// CHECK at commit time.
func TestBeginIndexRunIfClearRejectsWrongKind(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	vID := makeVolume(t, s, "/v")

	for _, k := range []string{RunKindSync, RunKindRestore, ""} {
		_, _, err := s.BeginIndexRunIfClear(context.Background(), k, vID, false)
		if err == nil || !strings.Contains(err.Error(), "kind must be") {
			t.Fatalf("kind=%q: want kind-validation error, got %v", k, err)
		}
	}
}

// TestBeginSyncRunIfClearBlockedByIndex is the #103 cross-kind guard: a
// running index (or audit) on the volume refuses a new sync, so a sync
// never captures its enumeration snapshot against a tree an index is
// mutating. Once the index finishes, the sync is admitted.
func TestBeginSyncRunIfClearBlockedByIndex(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	for _, indexKind := range []string{RunKindIndex, RunKindAudit} {
		idxID, blocker, err := s.BeginIndexRunIfClear(ctx, indexKind, vID, false)
		if err != nil || blocker != nil {
			t.Fatalf("%s: begin index: id=%d blocker=%+v err=%v", indexKind, idxID, blocker, err)
		}

		syncID, syncBlocker, err := s.BeginSyncRunIfClear(ctx, SyncRunSpec{VolumeID: vID, Destination: "backup"})
		if err != nil {
			t.Fatalf("%s: begin sync: %v", indexKind, err)
		}
		if syncBlocker == nil || syncID != 0 {
			t.Fatalf("%s: sync admitted (id=%d) while index running, want blocked", indexKind, syncID)
		}
		if syncBlocker.Kind != indexKind {
			t.Fatalf("%s: blocker kind = %q, want %q", indexKind, syncBlocker.Kind, indexKind)
		}

		if err := s.FinishRun(ctx, idxID, RunStatusSuccess, "", 0); err != nil {
			t.Fatalf("%s: finish index: %v", indexKind, err)
		}
		syncID, syncBlocker, err = s.BeginSyncRunIfClear(ctx, SyncRunSpec{VolumeID: vID, Destination: "backup"})
		if err != nil || syncBlocker != nil || syncID == 0 {
			t.Fatalf("%s: sync refused after index finished: id=%d blocker=%+v err=%v", indexKind, syncID, syncBlocker, err)
		}
		if err := s.FinishRun(ctx, syncID, RunStatusSuccess, "", 0); err != nil {
			t.Fatalf("%s: finish sync: %v", indexKind, err)
		}
	}
}

// TestBeginIndexRunIfClearAllowsConcurrentSync pins the deliberate
// asymmetry to the sync→index block (#103): a running sync does NOT
// block a new index, because the sync's advance is pinned to the
// snapshot it already captured and the agent scheduler must be free to
// index before a sync even while an unrelated sync is in flight.
func TestBeginIndexRunIfClearAllowsConcurrentSync(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	syncID, blocker, err := s.BeginSyncRunIfClear(ctx, SyncRunSpec{VolumeID: vID, Destination: "backup"})
	if err != nil || blocker != nil || syncID == 0 {
		t.Fatalf("begin sync: id=%d blocker=%+v err=%v", syncID, blocker, err)
	}

	idxID, idxBlocker, err := s.BeginIndexRunIfClear(ctx, RunKindIndex, vID, false)
	if err != nil {
		t.Fatalf("begin index during sync: %v", err)
	}
	if idxBlocker != nil || idxID == 0 {
		t.Fatalf("index blocked by running sync (id=%d blocker=%+v), want admitted", idxID, idxBlocker)
	}
}

// TestBeginSyncRunIfClearBlockedByOffload makes the run gate symmetric:
// offload already blocks on every kind, so a sync must refuse to start
// while an offload is in flight (#114). A concurrent unlink would
// otherwise race the sync's enumeration.
func TestBeginSyncRunIfClearBlockedByOffload(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	offID, blocker, err := s.BeginOffloadRunIfClear(ctx, vID)
	if err != nil || blocker != nil || offID == 0 {
		t.Fatalf("begin offload: id=%d blocker=%+v err=%v", offID, blocker, err)
	}

	syncID, syncBlocker, err := s.BeginSyncRunIfClear(ctx, SyncRunSpec{VolumeID: vID, Destination: "backup"})
	if err != nil {
		t.Fatalf("begin sync during offload: %v", err)
	}
	if syncBlocker == nil || syncID != 0 {
		t.Fatalf("sync admitted (id=%d) while offload running, want blocked", syncID)
	}
	if syncBlocker.Kind != RunKindOffload {
		t.Fatalf("blocker kind = %q, want %q", syncBlocker.Kind, RunKindOffload)
	}

	if err := s.FinishRun(ctx, offID, RunStatusSuccess, "", 0); err != nil {
		t.Fatalf("finish offload: %v", err)
	}
	syncID, syncBlocker, err = s.BeginSyncRunIfClear(ctx, SyncRunSpec{VolumeID: vID, Destination: "backup"})
	if err != nil || syncBlocker != nil || syncID == 0 {
		t.Fatalf("sync refused after offload finished: id=%d blocker=%+v err=%v", syncID, syncBlocker, err)
	}
}

// TestBeginIndexRunIfClearBlockedByOffload: an in-flight offload blocks a
// new index or audit so the walk can't observe-and-flip a row mid-unlink
// (#114).
func TestBeginIndexRunIfClearBlockedByOffload(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for _, indexKind := range []string{RunKindIndex, RunKindAudit} {
		vID := makeVolume(t, s, "/v-"+indexKind)

		offID, blocker, err := s.BeginOffloadRunIfClear(ctx, vID)
		if err != nil || blocker != nil || offID == 0 {
			t.Fatalf("%s: begin offload: id=%d blocker=%+v err=%v", indexKind, offID, blocker, err)
		}

		idxID, idxBlocker, err := s.BeginIndexRunIfClear(ctx, indexKind, vID, false)
		if err != nil {
			t.Fatalf("%s: begin index during offload: %v", indexKind, err)
		}
		if idxBlocker == nil || idxID != 0 {
			t.Fatalf("%s: index admitted (id=%d) while offload running, want blocked", indexKind, idxID)
		}
		if idxBlocker.Kind != RunKindOffload {
			t.Fatalf("%s: blocker kind = %q, want %q", indexKind, idxBlocker.Kind, RunKindOffload)
		}

		if err := s.FinishRun(ctx, offID, RunStatusSuccess, "", 0); err != nil {
			t.Fatalf("%s: finish offload: %v", indexKind, err)
		}
		idxID, idxBlocker, err = s.BeginIndexRunIfClear(ctx, indexKind, vID, false)
		if err != nil || idxBlocker != nil || idxID == 0 {
			t.Fatalf("%s: index refused after offload finished: id=%d blocker=%+v err=%v", indexKind, idxID, idxBlocker, err)
		}
	}
}

// TestBackupVacuumIntoProducesValidSnapshot exercises Backup against
// a populated store, then opens the snapshot as a regular DB and
// verifies it carries the same volume row. Cheapest reliable check
// that VACUUM INTO actually wrote a usable copy.
func TestBackupVacuumIntoProducesValidSnapshot(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")

	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := s.Backup(ctx, snap); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := os.Stat(snap); err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
	s2, err := OpenWithOptions(snap, OpenOptions{DisablePreMigrationBackup: true})
	if err != nil {
		t.Fatalf("Open snapshot: %v", err)
	}
	defer s2.Close()
	v, err := s2.GetVolumeByID(ctx, vID)
	if err != nil {
		t.Fatalf("snapshot missing volume row: %v", err)
	}
	if v.Path != "/v" {
		t.Fatalf("snapshot volume path = %q, want /v", v.Path)
	}
}

// TestBackupRefusesExistingPath: callers should not silently overwrite
// an existing snapshot — every backup should be a fresh file with a
// unique timestamp.
func TestBackupRefusesExistingPath(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(snap, []byte("preexisting"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(context.Background(), snap); err == nil {
		t.Fatalf("Backup over an existing file should refuse")
	}
}

// TestIntegrityCheckCleanDB returns ["ok"] on a fresh DB.
func TestIntegrityCheckCleanDB(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	rows, err := s.IntegrityCheck(context.Background())
	if err != nil {
		t.Fatalf("IntegrityCheck: %v", err)
	}
	if !IsIntegrityClean(rows) {
		t.Fatalf("rows = %v, want [ok]", rows)
	}
}

// TestPreflightCheckSnapshotRoundTrip: Backup + PreflightCheckSnapshot
// must read back the schema version that's currently in the live DB.
func TestPreflightCheckSnapshotRoundTrip(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := s.Backup(context.Background(), snap); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	v, err := PreflightCheckSnapshot(context.Background(), snap)
	if err != nil {
		t.Fatalf("PreflightCheckSnapshot: %v", err)
	}
	if v != SchemaVersion {
		t.Fatalf("snapshot version = %d, want %d", v, SchemaVersion)
	}
}

// TestProbeLiveDBExclusiveDetectsActiveAgent: while a Store holds the
// DB open, ProbeLiveDBExclusive must refuse to acquire the exclusive
// lock. After the Store closes, the probe succeeds.
func TestProbeLiveDBExclusiveDetectsActiveAgent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	// Hold a real write open to be sure we're holding the lock.
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE probe (x INTEGER)`); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("seed tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO probe VALUES (1)`); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	defer tx.Rollback()

	if err := ProbeLiveDBExclusive(ctx, dsn); err == nil {
		t.Fatalf("probe should refuse while a writer is active")
	}

	// Release the in-flight tx and close the store, then probe again.
	_ = tx.Rollback()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := ProbeLiveDBExclusive(ctx, dsn); err != nil {
		t.Fatalf("probe on closed db: %v", err)
	}
}

// TestMigratePreFlight verifies that opening an existing DB at an
// older schema version produces a snapshot in the configured
// backup directory before migrating forward.
func TestMigratePreMigrationBackup(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "test.db")
	// Step 1: create a DB at v5 by hand. We use applyV5 directly to
	// stop the chain at the baseline; the next OpenWithOptions will
	// see current=5 and migrate forward.
	{
		db, err := openSQLite(buildDSN(dsn))
		if err != nil {
			t.Fatalf("openSQLite: %v", err)
		}
		if _, err := db.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL PRIMARY KEY)`); err != nil {
			t.Fatalf("create schema_version: %v", err)
		}
		if err := applyV5(context.Background(), db); err != nil {
			t.Fatalf("applyV5: %v", err)
		}
		_ = db.Close()
	}

	backupDir := filepath.Join(dir, "snapshots")
	s, err := OpenWithOptions(dsn, OpenOptions{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer s.Close()

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("backup dir entries = %d, want 1: %+v", len(entries), entries)
	}
	if !strings.HasPrefix(entries[0].Name(), "pre-migration-v5-to-v") {
		t.Fatalf("backup name = %q, want pre-migration-v5-to-v*", entries[0].Name())
	}
}

// v13Fixture returns the DDL + seed for a fully populated v13 database
// — the last schema before the contents split. The seed exercises every
// backfill rule of migrateV13ToV14:
//
//   - hash X lives at two paths; the earliest observation (a.txt,
//     first_seen_run_id=1, local write) donates the contents row's size
//     and NULL origin even though the later sub/b.txt observation
//     carries peer attribution.
//   - hash Y is peer-sourced (node 2, run 2) and live at c.txt.
//   - hash Z is c.txt's superseded predecessor.
//   - hash W is a missing row.
func v13Fixture() []string {
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
			shallow INTEGER CHECK (shallow IS NULL OR shallow IN (0, 1)),
			CHECK (
				(kind IN ('index','audit') AND destination IS NULL) OR
				(kind IN ('sync','restore') AND destination IS NOT NULL AND destination != '')
			)
		)`,
		`CREATE TABLE folders (
			id                  INTEGER PRIMARY KEY,
			volume_id           INTEGER NOT NULL REFERENCES volumes(id),
			parent_id           INTEGER REFERENCES folders(id),
			path                TEXT NOT NULL,
			shallow_blake3      BLOB CHECK (shallow_blake3 IS NULL OR length(shallow_blake3) = 32),
			deep_blake3         BLOB CHECK (deep_blake3    IS NULL OR length(deep_blake3)    = 32),
			last_changed_run_id INTEGER REFERENCES runs(id),
			file_count      INTEGER NOT NULL DEFAULT 0,
			cumulative_size INTEGER NOT NULL DEFAULT 0,
			UNIQUE (volume_id, path)
		)`,
		`CREATE TABLE files (
			folder_id         INTEGER NOT NULL REFERENCES folders(id),
			name              TEXT NOT NULL,
			blake3            BLOB NOT NULL CHECK (length(blake3) = 32),
			size_bytes        INTEGER NOT NULL,
			mtime_ns          INTEGER NOT NULL,
			status            TEXT NOT NULL CHECK (status IN ('present','missing','superseded')),
			first_seen_run_id INTEGER NOT NULL REFERENCES runs(id),
			last_seen_run_id  INTEGER NOT NULL REFERENCES runs(id),
			indexed_at_ns     INTEGER NOT NULL,
			source_node_id    INTEGER REFERENCES nodes(id),
			source_run_id     INTEGER REFERENCES runs(id),
			PRIMARY KEY (folder_id, name, blake3)
		)`,
		`CREATE UNIQUE INDEX uniq_files_live_per_path ON files(folder_id, name) WHERE status != 'superseded'`,
		`CREATE TRIGGER files_blake3_immutable BEFORE UPDATE OF blake3 ON files
		 BEGIN
		     SELECT RAISE(ABORT, 'blake3 is immutable; supersede the row and insert a new one');
		 END`,
		`INSERT INTO schema_version (version) VALUES (13)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'photos', '/photos')`,
		`INSERT INTO nodes (id, name) VALUES (1, 'self'), (2, 'peer')`,
		`INSERT INTO runs (id, kind, volume_id, destination, started_at_ns, status, peer_node_id, correlated_run_id)
		 VALUES (1, 'index', 1, NULL, 100, 'success', NULL, NULL),
		        (2, 'sync',  1, 'peer', 200, 'success', 2, 900),
		        (3, 'index', 1, NULL, 300, 'success', NULL, NULL)`,
		`INSERT INTO folders (id, volume_id, parent_id, path) VALUES (1, 1, NULL, ''), (2, 1, 1, 'sub')`,
		`INSERT INTO files (folder_id, name, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns, source_node_id, source_run_id) VALUES
		 (1, 'a.txt', X'` + strings.Repeat("11", 32) + `', 10, 1, 'present',    1, 3, 1, NULL, NULL),
		 (2, 'b.txt', X'` + strings.Repeat("11", 32) + `', 10, 2, 'present',    3, 3, 2, 2, 2),
		 (1, 'c.txt', X'` + strings.Repeat("22", 32) + `', 20, 3, 'present',    2, 3, 3, 2, 2),
		 (1, 'c.txt', X'` + strings.Repeat("33", 32) + `', 30, 4, 'superseded', 1, 1, 4, NULL, NULL),
		 (2, 'd.txt', X'` + strings.Repeat("44", 32) + `', 40, 5, 'missing',    1, 3, 5, NULL, NULL)`,
	}
}

// TestMigrateV13ContentsSplit drives a populated v13 database through
// the v14–v16 chain and verifies the reshape end to end: row counts,
// the hash↔content mapping with its size/origin backfill, preserved
// statuses and run stamps, the surviving partial unique index, the
// dropped immutability trigger, the widened runs CHECK, and the new
// destination watermark store with its rewind refusal.
func TestMigrateV13ContentsSplit(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	for _, q := range v13Fixture() {
		if _, err := rawDB.Exec(q); err != nil {
			rawDB.Close()
			t.Fatalf("v13 DDL %q: %v", q, err)
		}
	}
	rawDB.Close()

	s, err := OpenWithOptions(dsn, OpenOptions{NodeName: "self"})
	if err != nil {
		t.Fatalf("Open (migrates v13→v%d): %v", SchemaVersion, err)
	}
	defer s.Close()
	ctx := context.Background()

	if v, _ := s.CurrentSchemaVersion(ctx); v != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", v, SchemaVersion)
	}

	assertContentsBackfill(t, s)
	assertFilesReshape(t, s)
	assertSchemaGuardsAfterSplit(t, s)
	assertRunsOffloadCheck(t, s)
	assertDestinationStoreAfterMigration(t, s)
	assertStatusChangedBackfill(t, s)
}

// assertStatusChangedBackfill checks the v17→v18 status_changed_run_id
// backfill on the migrated legacy rows: a present row stamps its
// first_seen_run_id (re-flips to present weren't recorded pre-v18), while a
// superseded/missing row stamps its last_seen_run_id (the closest recorded
// coordinate to its status flip). No live API exposes the column, so the
// assertion reads it directly against the v13 fixture's known statuses.
func assertStatusChangedBackfill(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	cases := []struct {
		name   string
		status string
		want   int64
	}{
		{"a.txt", "present", 1},    // first_seen=1
		{"b.txt", "present", 3},    // first_seen=3 (folder 2 'sub')
		{"c.txt", "present", 2},    // live c.txt: first_seen=2
		{"c.txt", "superseded", 1}, // superseded predecessor: last_seen=1
		{"d.txt", "missing", 3},    // missing row: last_seen=3
	}
	for _, c := range cases {
		var got sql.NullInt64
		if err := s.db.QueryRowContext(ctx,
			`SELECT status_changed_run_id FROM files WHERE name = ? AND status = ?`,
			c.name, c.status).Scan(&got); err != nil {
			t.Fatalf("status_changed_run_id for %s/%s: %v", c.name, c.status, err)
		}
		if !got.Valid || got.Int64 != c.want {
			t.Fatalf("%s/%s status_changed_run_id = %+v, want %d", c.name, c.status, got, c.want)
		}
	}
}

// assertContentsBackfill checks the distinct-hash → contents mapping:
// one row per hash, size and origin taken from the earliest observation.
func assertContentsBackfill(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	var fileCount, contentCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&fileCount); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if fileCount != 5 {
		t.Fatalf("files rows = %d, want 5 (no row lost in the rebuild)", fileCount)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM contents`).Scan(&contentCount); err != nil {
		t.Fatalf("count contents: %v", err)
	}
	if contentCount != 4 {
		t.Fatalf("contents rows = %d, want 4 (one per distinct hash)", contentCount)
	}

	cases := []struct {
		hash       []byte
		size       int64
		originNode sql.NullInt64
		originRun  sql.NullInt64
	}{
		// X: earliest observation is the local a.txt row, so the later
		// peer-attributed duplicate does not become the origin.
		{digest(0x11), 10, sql.NullInt64{}, sql.NullInt64{}},
		{digest(0x22), 20, sql.NullInt64{Int64: 2, Valid: true}, sql.NullInt64{Int64: 2, Valid: true}},
		{digest(0x33), 30, sql.NullInt64{}, sql.NullInt64{}},
		{digest(0x44), 40, sql.NullInt64{}, sql.NullInt64{}},
	}
	for _, c := range cases {
		var size int64
		var originNode, originRun sql.NullInt64
		if err := s.db.QueryRowContext(ctx,
			`SELECT size_bytes, origin_node_id, origin_run_id FROM contents WHERE blake3 = ?`,
			c.hash).Scan(&size, &originNode, &originRun); err != nil {
			t.Fatalf("contents row for %x: %v", c.hash[:2], err)
		}
		if size != c.size || originNode != c.originNode || originRun != c.originRun {
			t.Fatalf("contents %x = (size=%d, origin=%+v/%+v), want (size=%d, origin=%+v/%+v)",
				c.hash[:2], size, originNode, originRun, c.size, c.originNode, c.originRun)
		}
	}
}

// assertFilesReshape checks the per-path view through the store API:
// statuses, run stamps, the supersede chain, and duplicate detection
// across the shared content row.
func assertFilesReshape(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	a, err := s.GetByPath(ctx, 1, "a.txt")
	if err != nil {
		t.Fatalf("GetByPath a.txt: %v", err)
	}
	if !bytes.Equal(a.Blake3, digest(0x11)) || a.Status != StatusPresent ||
		a.SizeBytes != 10 || a.FirstSeenRunID != 1 || a.LastSeenRunID != 3 {
		t.Fatalf("a.txt = %+v, want X present first=1 last=3 size=10", a)
	}

	history, err := s.ListHistoryByPath(ctx, 1, "c.txt")
	if err != nil {
		t.Fatalf("ListHistoryByPath c.txt: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("c.txt history rows = %d, want 2", len(history))
	}
	if !bytes.Equal(history[0].Blake3, digest(0x33)) || history[0].Status != StatusSuperseded {
		t.Fatalf("c.txt history[0] = %+v, want Z superseded", history[0])
	}
	if !bytes.Equal(history[1].Blake3, digest(0x22)) || history[1].Status != StatusPresent {
		t.Fatalf("c.txt history[1] = %+v, want Y present", history[1])
	}
	if !history[1].OriginNodeID.Valid || history[1].OriginNodeID.Int64 != 2 {
		t.Fatalf("c.txt live origin = %+v, want node 2", history[1].OriginNodeID)
	}

	d, err := s.GetByPath(ctx, 1, "sub/d.txt")
	if err != nil {
		t.Fatalf("GetByPath sub/d.txt: %v", err)
	}
	if d.Status != StatusMissing {
		t.Fatalf("sub/d.txt status = %q, want missing", d.Status)
	}

	dups, err := s.ListDuplicates(ctx)
	if err != nil {
		t.Fatalf("ListDuplicates: %v", err)
	}
	if len(dups) != 2 {
		t.Fatalf("duplicates = %d rows, want 2 (a.txt + sub/b.txt share X)", len(dups))
	}
	if dups[0].File.ContentID != dups[1].File.ContentID {
		t.Fatalf("duplicate rows carry different content ids: %d vs %d",
			dups[0].File.ContentID, dups[1].File.ContentID)
	}
}

// assertSchemaGuardsAfterSplit checks the post-split schema shape: the
// partial unique index still guards one live row per path, and the
// blake3-immutability trigger is gone (id↔hash is immutable by
// construction on contents).
func assertSchemaGuardsAfterSplit(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO contents (blake3, size_bytes) VALUES (?, 1)`, digest(0x55)); err != nil {
		t.Fatalf("insert content: %v", err)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO files (folder_id, name, content_id, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns)
		VALUES (1, 'a.txt', (SELECT id FROM contents WHERE blake3 = ?), 9, 'present', 1, 1, 9)
	`, digest(0x55))
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("second live row at a.txt: err = %v, want UNIQUE violation", err)
	}

	var triggers int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'files_blake3_immutable'`).Scan(&triggers); err != nil {
		t.Fatalf("count triggers: %v", err)
	}
	if triggers != 0 {
		t.Fatalf("files_blake3_immutable still present after v14, want dropped")
	}
}

// assertRunsOffloadCheck checks the v15 kind CHECK: offload joins the
// destination-NULL branch.
func assertRunsOffloadCheck(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (kind, volume_id, destination, started_at_ns, status)
		VALUES ('offload', 1, NULL, 400, 'running')
	`); err != nil {
		t.Fatalf("offload run with NULL destination rejected: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (kind, volume_id, destination, started_at_ns, status)
		VALUES ('offload', 1, 'bucket', 500, 'running')
	`); err == nil {
		t.Fatalf("offload run with a destination accepted, want CHECK violation")
	}
}

// assertDestinationStoreAfterMigration checks the v16 watermark store
// against the migrated DB: an advance lands with history, and a rewind
// is refused.
func assertDestinationStoreAfterMigration(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	if err := s.UpsertDestinationRunID(ctx, 1, "bucket", 2, 5, false); err != nil {
		t.Fatalf("UpsertDestinationRunID after migration: %v", err)
	}
	if err := s.UpsertDestinationRunID(ctx, 1, "bucket", 2, 4, false); !errors.Is(err, ErrWatermarkRewind) {
		t.Fatalf("rewind err = %v, want ErrWatermarkRewind", err)
	}
	history, err := s.ListDestinationRunIDHistory(ctx, 1, "bucket")
	if err != nil {
		t.Fatalf("ListDestinationRunIDHistory: %v", err)
	}
	if len(history) != 1 || history[0].OriginRunID != 5 {
		t.Fatalf("history = %+v, want one advance to 5", history)
	}
}

// v18Fixture is a populated v18 database covering the offload-substrate
// tables (contents, remote_objects, destination_run_ids) so the
// v18→v19→v20→v21 chain can be exercised against real rows. The runs
// kind CHECK already carries 'offload' (v15) and status_changed_run_id
// exists on files (v18); verify_method, destination_push_freshness, and
// the contents triggers are what the chain still adds.
func v18Fixture() []string {
	return []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, endpoint TEXT, public_key_fingerprint TEXT)`,
		`CREATE TABLE runs (
			id            INTEGER PRIMARY KEY,
			kind          TEXT NOT NULL CHECK (kind IN ('index','sync','restore','audit','offload')),
			volume_id     INTEGER REFERENCES volumes(id),
			destination   TEXT,
			started_at_ns INTEGER NOT NULL,
			ended_at_ns   INTEGER,
			status        TEXT NOT NULL CHECK (status IN ('running','success','failed','partial')),
			error         TEXT,
			file_count    INTEGER NOT NULL DEFAULT 0,
			peer_node_id      INTEGER REFERENCES nodes(id),
			correlated_run_id INTEGER,
			shallow INTEGER CHECK (shallow IS NULL OR shallow IN (0, 1)),
			CHECK (
				(kind IN ('index','audit','offload') AND destination IS NULL) OR
				(kind IN ('sync','restore') AND destination IS NOT NULL AND destination != '')
			)
		)`,
		`CREATE TABLE folders (
			id                  INTEGER PRIMARY KEY,
			volume_id           INTEGER NOT NULL REFERENCES volumes(id),
			parent_id           INTEGER REFERENCES folders(id),
			path                TEXT NOT NULL,
			shallow_blake3      BLOB,
			deep_blake3         BLOB,
			last_changed_run_id INTEGER REFERENCES runs(id),
			file_count      INTEGER NOT NULL DEFAULT 0,
			cumulative_size INTEGER NOT NULL DEFAULT 0,
			UNIQUE (volume_id, path)
		)`,
		`CREATE TABLE contents (
			id             INTEGER PRIMARY KEY,
			blake3         BLOB NOT NULL UNIQUE CHECK (length(blake3) = 32),
			size_bytes     INTEGER NOT NULL,
			origin_node_id INTEGER REFERENCES nodes(id),
			origin_run_id  INTEGER
		)`,
		`CREATE TABLE files (
			folder_id         INTEGER NOT NULL REFERENCES folders(id),
			name              TEXT NOT NULL,
			content_id        INTEGER NOT NULL REFERENCES contents(id),
			mtime_ns          INTEGER NOT NULL,
			status            TEXT NOT NULL CHECK (status IN ('present','missing','superseded','offloaded')),
			first_seen_run_id INTEGER NOT NULL REFERENCES runs(id),
			last_seen_run_id  INTEGER NOT NULL REFERENCES runs(id),
			indexed_at_ns     INTEGER NOT NULL,
			status_changed_run_id INTEGER REFERENCES runs(id),
			PRIMARY KEY (folder_id, name, content_id)
		)`,
		`CREATE UNIQUE INDEX uniq_files_live_per_path ON files(folder_id, name) WHERE status != 'superseded'`,
		`CREATE TABLE destination_run_ids (
			volume_id      INTEGER NOT NULL REFERENCES volumes(id),
			destination    TEXT NOT NULL,
			origin_node_id INTEGER NOT NULL REFERENCES nodes(id),
			origin_run_id  INTEGER NOT NULL,
			updated_at_ns  INTEGER NOT NULL,
			PRIMARY KEY (volume_id, destination, origin_node_id)
		)`,
		`CREATE TABLE destination_run_ids_history (
			id             INTEGER PRIMARY KEY,
			volume_id      INTEGER NOT NULL,
			destination    TEXT NOT NULL,
			origin_node_id INTEGER NOT NULL,
			origin_run_id  INTEGER NOT NULL,
			at_ns          INTEGER NOT NULL
		)`,
		`CREATE TABLE remote_objects (
			content_id      INTEGER NOT NULL REFERENCES contents(id),
			destination     TEXT NOT NULL,
			uploaded_run_id INTEGER NOT NULL REFERENCES runs(id),
			checksum_algo   TEXT,
			checksum        TEXT,
			verified_at_ns  INTEGER,
			PRIMARY KEY (content_id, destination),
			CHECK ((checksum_algo IS NULL) = (checksum IS NULL))
		)`,
		`INSERT INTO schema_version (version) VALUES (18)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'photos', '/photos')`,
		`INSERT INTO nodes (id, name) VALUES (1, 'self'), (2, 'peer')`,
		`INSERT INTO runs (id, kind, volume_id, destination, started_at_ns, status)
		 VALUES (1, 'index', 1, NULL, 100, 'success'),
		        (2, 'sync',  1, 'bucket', 200, 'success')`,
		`INSERT INTO folders (id, volume_id, parent_id, path) VALUES (1, 1, NULL, '')`,
		`INSERT INTO contents (id, blake3, size_bytes, origin_node_id, origin_run_id) VALUES
		 (1, X'` + strings.Repeat("11", 32) + `', 10, NULL, NULL),
		 (2, X'` + strings.Repeat("22", 32) + `', 20, 2, 9)`,
		`INSERT INTO files (folder_id, name, content_id, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns, status_changed_run_id) VALUES
		 (1, 'a.txt', 1, 1, 'present', 1, 1, 1, 1),
		 (1, 'b.txt', 2, 2, 'present', 1, 1, 2, 1)`,
		`INSERT INTO destination_run_ids (volume_id, destination, origin_node_id, origin_run_id, updated_at_ns)
		 VALUES (1, 'bucket', 1, 7, 100)`,
		`INSERT INTO destination_run_ids_history (volume_id, destination, origin_node_id, origin_run_id, at_ns)
		 VALUES (1, 'bucket', 1, 7, 100)`,
		`INSERT INTO remote_objects (content_id, destination, uploaded_run_id, checksum_algo, checksum, verified_at_ns)
		 VALUES (1, 'bucket', 2, 'blake3', 'deadbeef', 150)`,
	}
}

// TestMigrateV18ChainToV21 drives a populated v18 database through the
// v19–v21 chain and confirms the offload-substrate rows survive intact
// (destination_run_ids with its NULL-backfilled verify_method,
// remote_objects with its fingerprint) and that the v21 contents triggers
// actually abort an UPDATE and a DELETE on a contents row.
func TestMigrateV18ChainToV21(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	for _, q := range v18Fixture() {
		if _, err := rawDB.Exec(q); err != nil {
			rawDB.Close()
			t.Fatalf("v18 DDL %q: %v", q, err)
		}
	}
	rawDB.Close()

	s, err := OpenWithOptions(dsn, OpenOptions{NodeName: "self"})
	if err != nil {
		t.Fatalf("Open (migrates v18→v%d): %v", SchemaVersion, err)
	}
	defer s.Close()
	ctx := context.Background()

	if v, _ := s.CurrentSchemaVersion(ctx); v != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", v, SchemaVersion)
	}

	assertV18SubstrateSurvived(t, s)
	assertContentsTriggersAbort(t, s)
}

// assertV18SubstrateSurvived checks the offload-substrate rows carried
// through the v19–v21 chain: the durability vector keeps its coordinate
// with a NULL-backfilled verify_method, and the remote_objects fingerprint
// is intact.
func assertV18SubstrateSurvived(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	got, err := s.GetDestinationRunID(ctx, 1, "bucket", 1)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.OriginRunID != 7 {
		t.Fatalf("origin run = %d, want 7 (carried over)", got.OriginRunID)
	}
	if got.VerifyMethod != "" {
		t.Fatalf("verify method = %q, want empty (NULL backfill)", got.VerifyMethod)
	}

	var algo, checksum string
	if err := s.db.QueryRowContext(ctx,
		`SELECT checksum_algo, checksum FROM remote_objects WHERE content_id = 1 AND destination = 'bucket'`).
		Scan(&algo, &checksum); err != nil {
		t.Fatalf("remote_objects row: %v", err)
	}
	if algo != "blake3" || checksum != "deadbeef" {
		t.Fatalf("remote_objects fingerprint = (%q,%q), want (blake3,deadbeef)", algo, checksum)
	}
}

// assertContentsTriggersAbort checks the v21 schema-level immutability:
// an in-place UPDATE and a DELETE on a contents row both abort, while the
// row stays exactly as written.
func assertContentsTriggersAbort(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	if _, err := s.db.ExecContext(ctx, `UPDATE contents SET size_bytes = 999 WHERE id = 1`); err == nil {
		t.Fatalf("UPDATE on contents succeeded, want trigger ABORT")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM contents WHERE id = 1`); err == nil {
		t.Fatalf("DELETE on contents succeeded, want trigger ABORT")
	}

	var size int64
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT size_bytes FROM contents WHERE id = 1`).Scan(&size); err != nil {
		t.Fatalf("contents row after refused mutations: %v", err)
	}
	if size != 10 {
		t.Fatalf("size_bytes = %d after refused UPDATE, want 10", size)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM contents`).Scan(&count); err != nil {
		t.Fatalf("count contents: %v", err)
	}
	if count != 2 {
		t.Fatalf("contents rows = %d after refused DELETE, want 2", count)
	}
}

// v13CoreDDL is the v13 table set the corrupt-shape fixtures below seed
// against. It omits the data and the uniq_files_live_per_path index so each
// fixture can choose whether to install the index — a legacy DB missing it
// is exactly the shape that lets a duplicate live row exist.
func v13CoreDDL() []string {
	return []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, endpoint TEXT, public_key_fingerprint TEXT)`,
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
			shallow INTEGER CHECK (shallow IS NULL OR shallow IN (0, 1)),
			CHECK (
				(kind IN ('index','audit') AND destination IS NULL) OR
				(kind IN ('sync','restore') AND destination IS NOT NULL AND destination != '')
			)
		)`,
		`CREATE TABLE folders (
			id                  INTEGER PRIMARY KEY,
			volume_id           INTEGER NOT NULL REFERENCES volumes(id),
			parent_id           INTEGER REFERENCES folders(id),
			path                TEXT NOT NULL,
			shallow_blake3      BLOB,
			deep_blake3         BLOB,
			last_changed_run_id INTEGER REFERENCES runs(id),
			file_count      INTEGER NOT NULL DEFAULT 0,
			cumulative_size INTEGER NOT NULL DEFAULT 0,
			UNIQUE (volume_id, path)
		)`,
		`CREATE TABLE files (
			folder_id         INTEGER NOT NULL REFERENCES folders(id),
			name              TEXT NOT NULL,
			blake3            BLOB NOT NULL CHECK (length(blake3) = 32),
			size_bytes        INTEGER NOT NULL,
			mtime_ns          INTEGER NOT NULL,
			status            TEXT NOT NULL CHECK (status IN ('present','missing','superseded')),
			first_seen_run_id INTEGER NOT NULL REFERENCES runs(id),
			last_seen_run_id  INTEGER NOT NULL REFERENCES runs(id),
			indexed_at_ns     INTEGER NOT NULL,
			source_node_id    INTEGER REFERENCES nodes(id),
			source_run_id     INTEGER REFERENCES runs(id),
			PRIMARY KEY (folder_id, name, blake3)
		)`,
		`INSERT INTO schema_version (version) VALUES (13)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'photos', '/photos')`,
		`INSERT INTO nodes (id, name) VALUES (1, 'self')`,
		`INSERT INTO runs (id, kind, volume_id, destination, started_at_ns, status)
		 VALUES (1, 'index', 1, NULL, 100, 'success')`,
		`INSERT INTO folders (id, volume_id, parent_id, path) VALUES (1, 1, NULL, '')`,
	}
}

// migrateRawFixture writes the fixture DDL to a fresh DB through a raw
// connection (so FK enforcement stays off and pathological rows are
// insertable), then opens it through the real migration path and returns the
// Open error for the caller to assert on.
func migrateRawFixture(t *testing.T, ddl []string) error {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	for _, q := range ddl {
		if _, err := rawDB.Exec(q); err != nil {
			rawDB.Close()
			t.Fatalf("fixture DDL %q: %v", q, err)
		}
	}
	rawDB.Close()

	s, err := OpenWithOptions(dsn, OpenOptions{NodeName: "self", DisablePreMigrationBackup: true})
	if err == nil {
		s.Close()
	}
	return err
}

// TestMigrateV13SameHashDifferentSizeRefused asserts the v13→v14 backfill
// guard: two v13 files rows sharing a blake3 with differing size_bytes (only
// reachable via prior corruption or a stat/hash TOCTOU) make the migration
// refuse loudly instead of silently coalescing to the earliest size.
func TestMigrateV13SameHashDifferentSizeRefused(t *testing.T) {
	ddl := append(v13CoreDDL(),
		`INSERT INTO files (folder_id, name, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns) VALUES
		 (1, 'a.txt', X'`+strings.Repeat("11", 32)+`', 10, 1, 'present', 1, 1, 1),
		 (1, 'b.txt', X'`+strings.Repeat("11", 32)+`', 20, 2, 'present', 1, 1, 2)`,
	)
	err := migrateRawFixture(t, ddl)
	if err == nil {
		t.Fatal("migration accepted same-hash-different-size files, want refusal")
	}
	if !strings.Contains(err.Error(), "differing size_bytes") {
		t.Fatalf("error = %v, want same-hash-different-size refusal", err)
	}
}

// TestMigrateV13SameHashSameSizeAccepted is the negative control for the
// guard: the same hash at two paths with the *same* size is valid (a
// duplicate file) and must migrate cleanly into one contents row.
func TestMigrateV13SameHashSameSizeAccepted(t *testing.T) {
	ddl := append(v13CoreDDL(),
		`INSERT INTO files (folder_id, name, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns) VALUES
		 (1, 'a.txt', X'`+strings.Repeat("11", 32)+`', 10, 1, 'present', 1, 1, 1),
		 (1, 'b.txt', X'`+strings.Repeat("11", 32)+`', 10, 2, 'present', 1, 1, 2)`,
	)
	if err := migrateRawFixture(t, ddl); err != nil {
		t.Fatalf("migration refused a valid same-hash-same-size duplicate: %v", err)
	}
}

// TestMigrateV13OrphanedFKRolledBack asserts the v13→v14 rebuild's
// foreign_key_check catches a files row referencing a non-existent run and
// rolls the migration back loudly rather than landing a dangling reference.
func TestMigrateV13OrphanedFKRolledBack(t *testing.T) {
	ddl := append(v13CoreDDL(),
		`INSERT INTO files (folder_id, name, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns) VALUES
		 (1, 'a.txt', X'`+strings.Repeat("11", 32)+`', 10, 1, 'present', 1, 999, 1)`,
	)
	err := migrateRawFixture(t, ddl)
	if err == nil {
		t.Fatal("migration accepted an orphaned files→runs FK, want rollback")
	}
	if !strings.Contains(err.Error(), "dangling FK") {
		t.Fatalf("error = %v, want dangling FK refusal", err)
	}
}

// TestMigrateV13DuplicateLiveRowRolledBack asserts that a legacy DB missing
// uniq_files_live_per_path with two live rows at one path fails loudly when
// the v13→v14 rebuild recreates the partial unique index, rather than
// migrating two live rows into the reshaped table.
func TestMigrateV13DuplicateLiveRowRolledBack(t *testing.T) {
	ddl := append(v13CoreDDL(),
		`INSERT INTO files (folder_id, name, blake3, size_bytes, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns) VALUES
		 (1, 'a.txt', X'`+strings.Repeat("11", 32)+`', 10, 1, 'present', 1, 1, 1),
		 (1, 'a.txt', X'`+strings.Repeat("22", 32)+`', 20, 2, 'present', 1, 1, 2)`,
	)
	err := migrateRawFixture(t, ddl)
	if err == nil {
		t.Fatal("migration accepted two live rows at one path, want rollback")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("error = %v, want UNIQUE violation on uniq_files_live_per_path", err)
	}
}

// v16Fixture returns a populated v16 database whose remote_objects table
// carries the pre-v17 NOT NULL (checksum_algo, checksum) shape. It exists so
// the v16→v17 rebuild — which actually rewrites remote_objects to relax those
// columns to nullable — is exercised against real rows; the v18 fixture seeds
// remote_objects but starts after that rebuild.
func v16Fixture() []string {
	return []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, endpoint TEXT, public_key_fingerprint TEXT)`,
		`CREATE TABLE runs (
			id            INTEGER PRIMARY KEY,
			kind          TEXT NOT NULL CHECK (kind IN ('index','sync','restore','audit','offload')),
			volume_id     INTEGER REFERENCES volumes(id),
			destination   TEXT,
			started_at_ns INTEGER NOT NULL,
			ended_at_ns   INTEGER,
			status        TEXT NOT NULL CHECK (status IN ('running','success','failed','partial')),
			error         TEXT,
			file_count    INTEGER NOT NULL DEFAULT 0,
			peer_node_id      INTEGER REFERENCES nodes(id),
			correlated_run_id INTEGER,
			shallow INTEGER CHECK (shallow IS NULL OR shallow IN (0, 1)),
			CHECK (
				(kind IN ('index','audit','offload') AND destination IS NULL) OR
				(kind IN ('sync','restore') AND destination IS NOT NULL AND destination != '')
			)
		)`,
		`CREATE TABLE folders (
			id                  INTEGER PRIMARY KEY,
			volume_id           INTEGER NOT NULL REFERENCES volumes(id),
			parent_id           INTEGER REFERENCES folders(id),
			path                TEXT NOT NULL,
			shallow_blake3      BLOB,
			deep_blake3         BLOB,
			last_changed_run_id INTEGER REFERENCES runs(id),
			file_count      INTEGER NOT NULL DEFAULT 0,
			cumulative_size INTEGER NOT NULL DEFAULT 0,
			UNIQUE (volume_id, path)
		)`,
		`CREATE TABLE contents (
			id             INTEGER PRIMARY KEY,
			blake3         BLOB NOT NULL UNIQUE CHECK (length(blake3) = 32),
			size_bytes     INTEGER NOT NULL,
			origin_node_id INTEGER REFERENCES nodes(id),
			origin_run_id  INTEGER
		)`,
		`CREATE TABLE files (
			folder_id         INTEGER NOT NULL REFERENCES folders(id),
			name              TEXT NOT NULL,
			content_id        INTEGER NOT NULL REFERENCES contents(id),
			mtime_ns          INTEGER NOT NULL,
			status            TEXT NOT NULL CHECK (status IN ('present','missing','superseded','offloaded')),
			first_seen_run_id INTEGER NOT NULL REFERENCES runs(id),
			last_seen_run_id  INTEGER NOT NULL REFERENCES runs(id),
			indexed_at_ns     INTEGER NOT NULL,
			PRIMARY KEY (folder_id, name, content_id)
		)`,
		`CREATE UNIQUE INDEX uniq_files_live_per_path ON files(folder_id, name) WHERE status != 'superseded'`,
		`CREATE TABLE destination_run_ids (
			volume_id      INTEGER NOT NULL REFERENCES volumes(id),
			destination    TEXT NOT NULL,
			origin_node_id INTEGER NOT NULL REFERENCES nodes(id),
			origin_run_id  INTEGER NOT NULL,
			updated_at_ns  INTEGER NOT NULL,
			PRIMARY KEY (volume_id, destination, origin_node_id)
		)`,
		`CREATE TABLE destination_run_ids_history (
			id             INTEGER PRIMARY KEY,
			volume_id      INTEGER NOT NULL,
			destination    TEXT NOT NULL,
			origin_node_id INTEGER NOT NULL,
			origin_run_id  INTEGER NOT NULL,
			at_ns          INTEGER NOT NULL
		)`,
		`CREATE TABLE remote_objects (
			content_id      INTEGER NOT NULL REFERENCES contents(id),
			destination     TEXT NOT NULL,
			uploaded_run_id INTEGER NOT NULL REFERENCES runs(id),
			checksum_algo   TEXT NOT NULL,
			checksum        TEXT NOT NULL,
			verified_at_ns  INTEGER,
			PRIMARY KEY (content_id, destination)
		)`,
		`INSERT INTO schema_version (version) VALUES (16)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'photos', '/photos')`,
		`INSERT INTO nodes (id, name) VALUES (1, 'self')`,
		`INSERT INTO runs (id, kind, volume_id, destination, started_at_ns, status)
		 VALUES (1, 'index', 1, NULL, 100, 'success'),
		        (2, 'sync',  1, 'bucket', 200, 'success')`,
		`INSERT INTO folders (id, volume_id, parent_id, path) VALUES (1, 1, NULL, '')`,
		`INSERT INTO contents (id, blake3, size_bytes) VALUES
		 (1, X'` + strings.Repeat("11", 32) + `', 10),
		 (2, X'` + strings.Repeat("22", 32) + `', 20)`,
		`INSERT INTO files (folder_id, name, content_id, mtime_ns, status, first_seen_run_id, last_seen_run_id, indexed_at_ns) VALUES
		 (1, 'a.txt', 1, 1, 'present', 1, 1, 1),
		 (1, 'b.txt', 2, 2, 'present', 1, 1, 2)`,
		`INSERT INTO remote_objects (content_id, destination, uploaded_run_id, checksum_algo, checksum, verified_at_ns) VALUES
		 (1, 'bucket', 2, 'blake3', 'deadbeef', 150),
		 (2, 'bucket', 2, 'blake3', 'cafebabe', 150)`,
	}
}

// TestMigrateV16RemoteObjectsRebuild drives a populated v16 database through
// the v17 remote_objects rebuild and confirms the rows survive the table
// rewrite with their fingerprints intact, and that the relaxed v17 shape now
// accepts an upload record with a pending (NULL,NULL) fingerprint.
func TestMigrateV16RemoteObjectsRebuild(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	for _, q := range v16Fixture() {
		if _, err := rawDB.Exec(q); err != nil {
			rawDB.Close()
			t.Fatalf("v16 DDL %q: %v", q, err)
		}
	}
	rawDB.Close()

	s, err := OpenWithOptions(dsn, OpenOptions{NodeName: "self"})
	if err != nil {
		t.Fatalf("Open (migrates v16→v%d): %v", SchemaVersion, err)
	}
	defer s.Close()
	ctx := context.Background()

	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_objects`).Scan(&rows); err != nil {
		t.Fatalf("count remote_objects: %v", err)
	}
	if rows != 2 {
		t.Fatalf("remote_objects rows = %d after rebuild, want 2 (none lost)", rows)
	}
	var algo, checksum string
	if err := s.db.QueryRowContext(ctx,
		`SELECT checksum_algo, checksum FROM remote_objects WHERE content_id = 1 AND destination = 'bucket'`).
		Scan(&algo, &checksum); err != nil {
		t.Fatalf("remote_objects row: %v", err)
	}
	if algo != "blake3" || checksum != "deadbeef" {
		t.Fatalf("fingerprint = (%q,%q), want (blake3,deadbeef)", algo, checksum)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO remote_objects (content_id, destination, uploaded_run_id, checksum_algo, checksum)
		 VALUES (2, 'bucket2', 2, NULL, NULL)`); err != nil {
		t.Fatalf("v17 relaxed shape rejected a pending-fingerprint upload: %v", err)
	}
}

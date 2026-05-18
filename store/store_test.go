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
	}, nil); err != nil {
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
	}, nil); err != nil {
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
	if row.SourceNodeID.Valid || row.SourceRunID.Valid {
		t.Fatalf("migrated row has non-NULL provenance %+v / %+v, want NULL",
			row.SourceNodeID, row.SourceRunID)
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

// TestUpsertWithProvenance verifies that a non-nil *Provenance lands the
// source_node_id and source_run_id columns on the inserted row, that a
// subsequent provenance-aware overwrite supersedes the prior row, and
// that the supersede flow itself is unchanged (the prior row's
// provenance survives on the historical record).
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
	if !live.SourceNodeID.Valid || live.SourceNodeID.Int64 != peerID {
		t.Fatalf("SourceNodeID = %+v, want %d", live.SourceNodeID, peerID)
	}
	if !live.SourceRunID.Valid || live.SourceRunID.Int64 != run1 {
		t.Fatalf("SourceRunID = %+v, want %d", live.SourceRunID, run1)
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
	if !old.SourceNodeID.Valid || old.SourceNodeID.Int64 != peerID {
		t.Fatalf("superseded row lost provenance: %+v", old.SourceNodeID)
	}
	if newRow.Status != StatusPresent || !bytes.Equal(newRow.Blake3, digest(0xbb)) {
		t.Fatalf("new row = %+v, want hashB present", newRow)
	}
	if newRow.SourceNodeID.Valid || newRow.SourceRunID.Valid {
		t.Fatalf("local-write row has non-NULL provenance: %+v / %+v",
			newRow.SourceNodeID, newRow.SourceRunID)
	}
}

// TestUpsertProvenanceFKRejected guards the FK enforcement on both new
// provenance columns: pointing at a node or run id that does not exist
// must fail rather than silently land a dangling reference.
func TestUpsertProvenanceFKRejected(t *testing.T) {
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

	cases := []struct {
		name string
		prov *Provenance
	}{
		{"bogus node id", &Provenance{NodeID: 99999, RunID: run}},
		{"bogus run id", &Provenance{NodeID: peerID, RunID: 99999}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.Upsert(ctx, FileRow{
				// Distinct paths per case so a prior failure can't shadow a
				// later one through the live-row state machine.
				VolumeID: vID, Path: fmt.Sprintf("x-%d", i), Blake3: digest(byte(0x10 + i)),
				SizeBytes: 1, MtimeNs: 1,
				Status: StatusPresent, FirstSeenRunID: run, LastSeenRunID: run, IndexedAtNs: 1,
			}, c.prov)
			if err == nil {
				t.Fatalf("Upsert with %s succeeded; FK not enforced", c.name)
			}
		})
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

// TestPartialIndexOnSourceNodeExistsV6 verifies the schema-introspection
// expectation called out in the PR description: the partial index on
// files(source_node_id) WHERE status='present' exists on v6 (and is
// absent on a v5 fixture that hasn't been migrated yet).
func TestPartialIndexOnSourceNodeExistsV6(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	var ddl string
	err = s.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_files_source_node'`).Scan(&ddl)
	if err != nil {
		t.Fatalf("look up partial index: %v", err)
	}
	for _, want := range []string{"source_node_id", "status = 'present'", "source_node_id IS NOT NULL"} {
		if !strings.Contains(ddl, want) {
			t.Fatalf("idx_files_source_node SQL = %q, missing %q (partial index must exclude local-write NULLs)", ddl, want)
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
// between fileColumns, scanFrom, and insertArgs. A row inserted via
// insertArgs and read back via scanFrom must equal the original. Adding
// a column without updating every helper would surface here as a Scan
// arity mismatch or a field whose value lands in the wrong slot.
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
		SourceNodeID:   sql.NullInt64{Int64: 1, Valid: true},
		SourceRunID:    sql.NullInt64{Int64: runID, Valid: true},
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO files (`+fileColumns+`) VALUES (`+filePlaceholders+`)`,
		want.insertArgs()...); err != nil {
		t.Fatalf("insert via insertArgs: %v", err)
	}

	var got FileRow
	row := s.db.QueryRowContext(ctx,
		`SELECT `+fileColumns+` FROM files WHERE volume_id = ? AND path = ? AND blake3 = ?`,
		want.VolumeID, want.Path, want.Blake3)
	if err := got.scanFrom(row); err != nil {
		t.Fatalf("scanFrom: %v", err)
	}

	if got.VolumeID != want.VolumeID || got.Path != want.Path ||
		!bytes.Equal(got.Blake3, want.Blake3) ||
		got.SizeBytes != want.SizeBytes || got.MtimeNs != want.MtimeNs ||
		got.Status != want.Status ||
		got.FirstSeenRunID != want.FirstSeenRunID || got.LastSeenRunID != want.LastSeenRunID ||
		got.IndexedAtNs != want.IndexedAtNs ||
		got.SourceNodeID != want.SourceNodeID || got.SourceRunID != want.SourceRunID {
		t.Fatalf("round-trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

// TestFilePlaceholdersMatchesFileColumns guards the derived placeholder
// list against drift from fileColumns. A column added to fileColumns but
// forgotten elsewhere (or vice versa) trips here before any INSERT hits
// SQLite and produces an opaque parameter-count error.

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

// TestListPresentBySource pins the two filter modes: valid nodeID
// returns rows attributed to that peer, NULL nodeID returns rows
// without provenance (local writes). Superseded and missing rows
// must be excluded under either mode.
func TestListPresentBySource(t *testing.T) {
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
		for row, err := range s.ListPresentBySource(ctx, vID, nodeID) {
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

// TestListPresentBySourceEarlyBreakClosesRows confirms the iter.Seq2
// implementation closes its underlying rows when the consumer breaks
// early. Without this guarantee a long-running CLI could leak a
// statement handle whenever the user pages results.
func TestListPresentBySourceEarlyBreakClosesRows(t *testing.T) {
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
	for _, err := range s.ListPresentBySource(ctx, vID, sql.NullInt64{Int64: peer.ID, Valid: true}) {
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
	for _, err := range s.ListPresentBySource(ctx, vID, sql.NullInt64{Int64: peer.ID, Valid: true}) {
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
	bucketRun, _ := s.BeginRun(ctx, RunKindSync, vID, "scratch")
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

func TestFilePlaceholdersMatchesFileColumns(t *testing.T) {
	gotCols := strings.Count(fileColumns, ",") + 1
	gotPlaceholders := strings.Count(filePlaceholders, "?")
	if gotCols != gotPlaceholders {
		t.Fatalf("fileColumns has %d entries, filePlaceholders has %d ?s", gotCols, gotPlaceholders)
	}
	// insertArgs is the third leg of the invariant — its return length
	// must match too. Use a zero-value row; the only thing we sample is
	// the slice length.
	if n := len(FileRow{}.insertArgs()); n != gotCols {
		t.Fatalf("insertArgs returns %d args, fileColumns has %d entries", n, gotCols)
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
	auditID, err := s.BeginRun(ctx, RunKindAudit, 1, "")
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
	if _, err := s.BeginRun(ctx, RunKindAudit, vID, ""); err != nil {
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
	_, _ = s.BeginRun(ctx, RunKindIndex, vID, "")
	// Three audit runs on vID. Their started_at_ns is set by BeginRun
	// to time.Now().UnixNano(); we read it back rather than synthesise.
	r1, _ := s.BeginRun(ctx, RunKindAudit, vID, "")
	row1, _ := s.GetRun(ctx, r1)
	r2, _ := s.BeginRun(ctx, RunKindAudit, vID, "")
	r3, _ := s.BeginRun(ctx, RunKindAudit, vID, "")
	// An audit run on a different volume — must not appear.
	_, _ = s.BeginRun(ctx, RunKindAudit, otherID, "")

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

	auditRun, err := s.BeginRun(ctx, RunKindAudit, vID, "")
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
	cleanRun, _ := s.BeginRun(ctx, RunKindAudit, vID, "")
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

	auditRun, err := s.BeginRun(ctx, RunKindAudit, vID, "")
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
	laterRun, _ := s.BeginRun(ctx, RunKindAudit, vID, "")
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
		`UPDATE files SET status = 'missing' WHERE volume_id = ? AND path = ?`,
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

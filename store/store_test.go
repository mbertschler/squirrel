package store

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func digest(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

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
	// Re-open and confirm the version is still the latest (no double-migrate).
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

func TestRefuseFutureSchema(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.SetSchemaVersion(context.Background(), SchemaVersion+5); err != nil {
		t.Fatalf("SetSchemaVersion: %v", err)
	}
	s.Close()

	_, err = Open(dsn)
	if err == nil {
		t.Fatalf("expected refusal to open future-version DB, got nil")
	}
	if !strings.Contains(err.Error(), "newer than binary") {
		t.Fatalf("error %q does not mention version mismatch", err)
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

	r := FileRow{
		Root: "/x", Path: "a", Blake3: digest(0xab), SizeBytes: 10, MtimeNs: 1, Status: StatusPresent,
		LastSeenAt: 100, IndexedAt: 100,
	}
	if err := s.Upsert(ctx, r); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := s.GetByPath(ctx, "/x", "a")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if !bytes.Equal(got.Blake3, digest(0xab)) || got.SizeBytes != 10 {
		t.Fatalf("got %+v", got)
	}

	// Same relative path under different root should not collide.
	r2 := r
	r2.Root = "/y"
	r2.Blake3 = digest(0xcd)
	if err := s.Upsert(ctx, r2); err != nil {
		t.Fatalf("Upsert /y/a: %v", err)
	}
	gotX, _ := s.GetByPath(ctx, "/x", "a")
	gotY, _ := s.GetByPath(ctx, "/y", "a")
	if !bytes.Equal(gotX.Blake3, digest(0xab)) || !bytes.Equal(gotY.Blake3, digest(0xcd)) {
		t.Fatalf("root scoping broken: x=%x y=%x", gotX.Blake3, gotY.Blake3)
	}

	// Update same (root, path)
	r.Blake3 = digest(0xef)
	r.SizeBytes = 20
	if err := s.Upsert(ctx, r); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err = s.GetByPath(ctx, "/x", "a")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if !bytes.Equal(got.Blake3, digest(0xef)) || got.SizeBytes != 20 {
		t.Fatalf("after update got %+v", got)
	}

	// GetByAbsolutePath should find /x/a.
	abs, err := s.GetByAbsolutePath(ctx, "/x/a")
	if err != nil {
		t.Fatalf("GetByAbsolutePath: %v", err)
	}
	if abs.Root != "/x" || abs.Path != "a" {
		t.Fatalf("GetByAbsolutePath returned %+v", abs)
	}
}

func TestMarkMissingScopedToRoot(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	rows := []FileRow{
		{Root: "/root", Path: "a", Blake3: digest(0x01), SizeBytes: 1, MtimeNs: 1, Status: StatusPresent, LastSeenAt: 50, IndexedAt: 50},
		{Root: "/root", Path: "b", Blake3: digest(0x02), SizeBytes: 1, MtimeNs: 1, Status: StatusPresent, LastSeenAt: 200, IndexedAt: 200},
		{Root: "/other", Path: "c", Blake3: digest(0x03), SizeBytes: 1, MtimeNs: 1, Status: StatusPresent, LastSeenAt: 50, IndexedAt: 50},
	}
	for _, r := range rows {
		if err := s.Upsert(ctx, r); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	n, err := s.MarkMissing(ctx, "/root", 100)
	if err != nil {
		t.Fatalf("MarkMissing: %v", err)
	}
	if n != 1 {
		t.Fatalf("MarkMissing affected %d, want 1", n)
	}

	a, _ := s.GetByPath(ctx, "/root", "a")
	if a.Status != StatusMissing {
		t.Fatalf("/root/a status = %s, want missing", a.Status)
	}
	b, _ := s.GetByPath(ctx, "/root", "b")
	if b.Status != StatusPresent {
		t.Fatalf("/root/b status = %s, want present", b.Status)
	}
	c, _ := s.GetByPath(ctx, "/other", "c")
	if c.Status != StatusPresent {
		t.Fatalf("/other/c status = %s, want present (different root)", c.Status)
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

	rows := []FileRow{
		{Root: "/r", Path: "a", Blake3: digest(0x11), Status: StatusPresent, LastSeenAt: 1, IndexedAt: 1},
		{Root: "/r", Path: "b", Blake3: digest(0x11), Status: StatusPresent, LastSeenAt: 1, IndexedAt: 1},
		{Root: "/r", Path: "c", Blake3: digest(0x22), Status: StatusPresent, LastSeenAt: 1, IndexedAt: 1},
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
	if !bytes.Equal(dups[0].Blake3, digest(0x11)) || !bytes.Equal(dups[1].Blake3, digest(0x11)) {
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

	cases := []struct {
		name string
		row  FileRow
	}{
		{"short blake3", FileRow{Root: "/r", Path: "a", Blake3: bytes.Repeat([]byte{1}, 31), Status: StatusPresent}},
		{"long blake3", FileRow{Root: "/r", Path: "b", Blake3: bytes.Repeat([]byte{1}, 33), Status: StatusPresent}},
		{"empty blake3", FileRow{Root: "/r", Path: "c", Blake3: nil, Status: StatusPresent}},
		{"invalid status", FileRow{Root: "/r", Path: "d", Blake3: digest(0x01), Status: "weird"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Upsert(ctx, tc.row); err == nil {
				t.Fatalf("Upsert(%+v) succeeded, want CHECK constraint failure", tc.row)
			}
		})
	}
}

func TestCrossRootDuplicates(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	shared := digest(0x42)
	rows := []FileRow{
		{Root: "/A", Path: "x", Blake3: shared, Status: StatusPresent, LastSeenAt: 1, IndexedAt: 1},
		{Root: "/B", Path: "y", Blake3: shared, Status: StatusPresent, LastSeenAt: 1, IndexedAt: 1},
		{Root: "/A", Path: "z", Blake3: digest(0x99), Status: StatusPresent, LastSeenAt: 1, IndexedAt: 1},
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
		t.Fatalf("dups = %d, want 2 (one per root)", len(dups))
	}
	roots := map[string]bool{dups[0].Root: true, dups[1].Root: true}
	if !roots["/A"] || !roots["/B"] {
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

	r := FileRow{Root: "/r", Path: "a", Blake3: digest(0x01), Status: StatusMissing, LastSeenAt: 100, IndexedAt: 100}
	if err := s.Upsert(ctx, r); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.TouchSeen(ctx, "/r", "a", 500); err != nil {
		t.Fatalf("TouchSeen: %v", err)
	}
	got, err := s.GetByPath(ctx, "/r", "a")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if got.Status != StatusPresent {
		t.Fatalf("Status = %s, want present", got.Status)
	}
	if got.LastSeenAt != 500 {
		t.Fatalf("LastSeenAt = %d, want 500", got.LastSeenAt)
	}
}

func TestGetByAbsolutePathNotUnderAnyRoot(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.Upsert(ctx, FileRow{Root: "/r", Path: "a", Blake3: digest(0x01), Status: StatusPresent, LastSeenAt: 1, IndexedAt: 1}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := s.GetByAbsolutePath(ctx, "/somewhere/else"); !IsNotFound(err) {
		t.Fatalf("got err=%v, want sql.ErrNoRows", err)
	}
}

package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
)

// packContentFixture upserts one file so a contents row exists and returns
// its content id. hashByte seeds a distinct BLAKE3 per call.
func packContentFixture(t *testing.T, s *Store, vID, runID int64, path string, hashByte byte) int64 {
	t.Helper()
	ctx := context.Background()
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: path, Blake3: digest(hashByte), SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert %s: %v", path, err)
	}
	row, err := s.GetByPath(ctx, vID, path)
	if err != nil {
		t.Fatalf("GetByPath %s: %v", path, err)
	}
	return row.ContentID
}

func packKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// TestInsertPacksRoundTrip: a pack and its members insert atomically and
// read back with the freshly assigned pack id linking members to the pack.
func TestInsertPacksRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	runID := makeRun(t, s, vID)
	c1 := packContentFixture(t, s, vID, runID, "a.txt", 0xa1)
	c2 := packContentFixture(t, s, vID, runID, "b.txt", 0xa2)

	err := s.InsertPacks(ctx, []PackWrite{{
		Pack: Pack{PackKey: packKey(0x11), SizeBytes: 500, MemberCount: 2, CreatedRunID: runID},
		Members: []PackMember{
			{ContentID: c1, ByteOffset: 512, ByteLength: 1},
			{ContentID: c2, ByteOffset: 1024, ByteLength: 1},
		},
	}})
	if err != nil {
		t.Fatalf("InsertPacks: %v", err)
	}

	pack, err := s.GetPackByKey(ctx, packKey(0x11))
	if err != nil {
		t.Fatalf("GetPackByKey: %v", err)
	}
	if pack.SizeBytes != 500 || pack.MemberCount != 2 || pack.CreatedRunID != runID {
		t.Fatalf("pack = %+v, want size 500 members 2 run %d", pack, runID)
	}

	members, err := s.ListPackMembers(ctx, pack.ID)
	if err != nil {
		t.Fatalf("ListPackMembers: %v", err)
	}
	if len(members) != 2 || members[0].ContentID != c1 || members[0].ByteOffset != 512 ||
		members[1].ContentID != c2 || members[1].PackID != pack.ID {
		t.Fatalf("members = %+v, want c1@512 then c2 linked to pack %d", members, pack.ID)
	}
}

// TestHasPackMember: the pack-once gate reports content unpacked before the
// insert and packed after, and GetPackMember returns the placement.
func TestHasPackMember(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	runID := makeRun(t, s, vID)
	c1 := packContentFixture(t, s, vID, runID, "a.txt", 0xb1)

	if has, err := s.HasPackMember(ctx, c1); err != nil || has {
		t.Fatalf("HasPackMember before = %v (err %v), want false", has, err)
	}
	if _, err := s.GetPackMember(ctx, c1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetPackMember before = %v, want ErrNoRows", err)
	}

	if err := s.InsertPacks(ctx, []PackWrite{{
		Pack:    Pack{PackKey: packKey(0x22), SizeBytes: 9, MemberCount: 1, CreatedRunID: runID},
		Members: []PackMember{{ContentID: c1, ByteOffset: 512, ByteLength: 1}},
	}}); err != nil {
		t.Fatalf("InsertPacks: %v", err)
	}

	if has, err := s.HasPackMember(ctx, c1); err != nil || !has {
		t.Fatalf("HasPackMember after = %v (err %v), want true", has, err)
	}
	m, err := s.GetPackMember(ctx, c1)
	if err != nil || m.ByteOffset != 512 || m.ByteLength != 1 {
		t.Fatalf("GetPackMember after = %+v (err %v), want offset 512 length 1", m, err)
	}
}

// TestInsertPacksRejectsSecondPackForContent: pack_members' primary key on
// content_id enforces packed-exactly-once — a second pack claiming the same
// content fails, and because the insert is transactional the second pack's
// row does not land either.
func TestInsertPacksRejectsSecondPackForContent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	runID := makeRun(t, s, vID)
	c1 := packContentFixture(t, s, vID, runID, "a.txt", 0xc1)

	if err := s.InsertPacks(ctx, []PackWrite{{
		Pack:    Pack{PackKey: packKey(0x33), SizeBytes: 9, MemberCount: 1, CreatedRunID: runID},
		Members: []PackMember{{ContentID: c1, ByteOffset: 512, ByteLength: 1}},
	}}); err != nil {
		t.Fatalf("first InsertPacks: %v", err)
	}
	err := s.InsertPacks(ctx, []PackWrite{{
		Pack:    Pack{PackKey: packKey(0x44), SizeBytes: 9, MemberCount: 1, CreatedRunID: runID},
		Members: []PackMember{{ContentID: c1, ByteOffset: 512, ByteLength: 1}},
	}})
	if err == nil {
		t.Fatalf("second InsertPacks for the same content unexpectedly succeeded")
	}
	if _, err := s.GetPackByKey(ctx, packKey(0x44)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rolled-back pack still present: %v", err)
	}
}

// TestInsertPacksRejectsBadKey: a pack key that is not 32 bytes is refused
// (the schema CHECK plus the accessor guard).
func TestInsertPacksRejectsBadKey(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	runID := makeRun(t, s, vID)
	err := s.InsertPacks(ctx, []PackWrite{{
		Pack: Pack{PackKey: []byte("short"), SizeBytes: 1, MemberCount: 0, CreatedRunID: runID},
	}})
	if err == nil {
		t.Fatalf("InsertPacks accepted a non-32-byte pack key")
	}
}

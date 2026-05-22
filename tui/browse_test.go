package tui

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

// upsertFile is a tight test helper for seeding a present file row. The
// callers care about (path, size, hash) — everything else is plumbing.
func upsertFile(t *testing.T, s *store.Store, volID, runID int64, path string, hashByte byte, size int64) {
	t.Helper()
	row := store.FileRow{
		VolumeID:       volID,
		Path:           path,
		Blake3:         bytes.Repeat([]byte{hashByte}, 32),
		SizeBytes:      size,
		MtimeNs:        1_000_000_000,
		Status:         store.StatusPresent,
		FirstSeenRunID: runID,
		LastSeenRunID:  runID,
		IndexedAtNs:    1_000_000_000,
	}
	if err := s.Upsert(context.Background(), row, nil); err != nil {
		t.Fatalf("Upsert(%q): %v", path, err)
	}
}

func TestBrowseLoadPathRootListsChildrenAndFiles(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vol, err := s.GetOrCreateVolume(ctx, "/srv/photos")
	if err != nil {
		t.Fatalf("GetOrCreateVolume: %v", err)
	}
	run, err := s.BeginRun(ctx, store.RunKindIndex, vol.ID, "")
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	// Layout under root:
	//   /readme.txt          (present file)
	//   /albums/a.jpg
	//   /albums/b.jpg
	//   /videos/c.mp4
	upsertFile(t, s, vol.ID, run, "readme.txt", 0x01, 42)
	upsertFile(t, s, vol.ID, run, "albums/a.jpg", 0x02, 1024)
	upsertFile(t, s, vol.ID, run, "albums/b.jpg", 0x03, 2048)
	upsertFile(t, s, vol.ID, run, "videos/c.mp4", 0x04, 4096)

	m := newBrowseModel(s)
	m.setVolume(vol.ID, "photos")

	cmd := m.loadPath("")
	if cmd == nil {
		t.Fatal("loadPath returned nil cmd")
	}
	msg, ok := cmd().(browseDataMsg)
	if !ok {
		t.Fatalf("loadPath cmd produced %T, want browseDataMsg", cmd())
	}
	if msg.err != nil {
		t.Fatalf("browseDataMsg.err = %v", msg.err)
	}
	if !msg.atRoot {
		t.Errorf("atRoot = false, want true at root path")
	}
	// Expect two folders (albums, videos) + one file (readme.txt), in that
	// order — folders first because ListChildFolders returns ordered by
	// path and we append files after.
	if len(msg.entries) != 3 {
		t.Fatalf("entries = %d, want 3 (albums, videos, readme.txt)", len(msg.entries))
	}
	if msg.entries[0].folder == nil || folderDisplayName(msg.entries[0].folder.Path) != "albums" {
		t.Errorf("entry[0] not 'albums' folder: %+v", msg.entries[0])
	}
	if msg.entries[1].folder == nil || folderDisplayName(msg.entries[1].folder.Path) != "videos" {
		t.Errorf("entry[1] not 'videos' folder: %+v", msg.entries[1])
	}
	if msg.entries[2].file == nil || msg.entries[2].file.Path != "readme.txt" {
		t.Errorf("entry[2] not 'readme.txt' file: %+v", msg.entries[2])
	}
}

func TestBrowseLoadPathDescend(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vol, _ := s.GetOrCreateVolume(ctx, "/srv/photos")
	run, _ := s.BeginRun(ctx, store.RunKindIndex, vol.ID, "")
	upsertFile(t, s, vol.ID, run, "albums/a.jpg", 0x02, 1024)
	upsertFile(t, s, vol.ID, run, "albums/b.jpg", 0x03, 2048)

	m := newBrowseModel(s)
	m.setVolume(vol.ID, "photos")
	msg := m.loadPath("albums")().(browseDataMsg)
	if msg.err != nil {
		t.Fatalf("err = %v", msg.err)
	}
	if msg.atRoot {
		t.Errorf("atRoot = true at /albums, want false")
	}
	if len(msg.entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(msg.entries))
	}
	if msg.entries[0].file == nil || msg.entries[0].file.Path != "albums/a.jpg" {
		t.Errorf("entry[0] = %+v, want a.jpg", msg.entries[0])
	}
}

func TestBrowseLoadPathMissingFolder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vol, _ := s.GetOrCreateVolume(ctx, "/srv/photos")

	m := newBrowseModel(s)
	m.setVolume(vol.ID, "photos")
	msg := m.loadPath("")().(browseDataMsg)
	// Volume exists but root folder was never created (no index run yet) —
	// the loader should surface a friendly "not indexed yet" error rather
	// than a raw sql.ErrNoRows.
	if msg.err == nil {
		t.Fatalf("expected error for un-indexed volume root")
	}
	if errors.Is(msg.err, errNoAgent) {
		t.Errorf("wrong error kind: %v", msg.err)
	}
}

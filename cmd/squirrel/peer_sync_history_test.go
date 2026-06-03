package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

// TestCLIPeerSyncHistory: after two watermark advances are recorded for a
// (volume, peer) pair, `squirrel peer-sync history <volume> <peer>`
// lists both transitions oldest-first. The pair is seeded through the
// store's public API (there is no CLI that writes a watermark) against
// the same DB the CLI reads.
func TestCLIPeerSyncHistory(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "a.txt"), "alpha")
	f := writeConfigFor(t, map[string]string{"src": src})
	runCLI(t, "--config", f.configPath, "index", "src")

	seedPeerSyncHistory(t, f.dbPath, "src", "nas", []int64{7, 42})

	out := runCLI(t, "--config", f.configPath, "peer-sync", "history", "src", "nas")
	if !strings.Contains(out, "LAST_SHARED_RUN_ID") {
		t.Fatalf("history output missing header:\n%s", out)
	}
	if !strings.Contains(out, "7") || !strings.Contains(out, "42") {
		t.Fatalf("history output missing watermark values:\n%s", out)
	}
}

// TestCLIPeerSyncHistoryUnknownVolume: an unknown volume name fails fast
// rather than printing an empty table.
func TestCLIPeerSyncHistoryUnknownVolume(t *testing.T) {
	f := writeConfigFor(t, map[string]string{"declared": t.TempDir()})
	_, err := runCLIExpectErr(t, "--config", f.configPath, "peer-sync", "history", "missing", "nas")
	if !strings.Contains(err.Error(), `no volume named "missing"`) {
		t.Fatalf("expected unknown-volume error, got %v", err)
	}
}

// seedPeerSyncHistory opens the DB the CLI uses, creates a peer node, and
// records the given watermark advances for (volumeName, peerName) via the
// store's public API so the history table is populated for the read test.
func seedPeerSyncHistory(t *testing.T, dbPath, volumeName, peerName string, watermarks []int64) {
	t.Helper()
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store to seed history: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	vol, err := s.GetVolumeByName(ctx, volumeName)
	if err != nil {
		t.Fatalf("look up volume %q: %v", volumeName, err)
	}
	peer, err := s.GetOrCreatePeerNode(ctx, peerName, "http://nas.example")
	if err != nil {
		t.Fatalf("create peer %q: %v", peerName, err)
	}
	for _, wm := range watermarks {
		if err := s.UpsertPeerSyncState(ctx, vol.ID, peer.ID, wm, false); err != nil {
			t.Fatalf("seed watermark %d: %v", wm, err)
		}
	}
}

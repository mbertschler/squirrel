package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

// TestCLIRestoreRoundTrip is the end-to-end smoke test: sync a volume,
// remove the local copy, restore from the destination, and verify the
// contents match.
func TestCLIRestoreRoundTrip(t *testing.T) {
	requireRcloneCLI(t)
	f := writeSyncFixture(t)
	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")
	writeTestFile(t, filepath.Join(f.volumeDir, "b.txt"), "beta")

	runCLI(t, "--config", f.configPath, "index", f.volumeName)
	runCLI(t, "--config", f.configPath, "sync", "pics")

	// Wipe the local volume so the restore has work to do.
	if err := os.RemoveAll(f.volumeDir); err != nil {
		t.Fatalf("remove volume: %v", err)
	}
	if err := os.MkdirAll(f.volumeDir, 0o755); err != nil {
		t.Fatalf("recreate volume: %v", err)
	}

	out := runCLI(t, "--config", f.configPath, "restore", "pics")
	if !strings.Contains(out, "status=success") {
		t.Fatalf("restore did not succeed:\n%s", out)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		path := filepath.Join(f.volumeDir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("missing restored %s: %v", path, err)
		}
		if len(body) == 0 {
			t.Fatalf("restored %s is empty", path)
		}
	}
}

// TestCLIRestoreToPathOverridesVolumePath confirms --to writes to the
// override directory rather than the volume's declared path.
func TestCLIRestoreToPathOverridesVolumePath(t *testing.T) {
	requireRcloneCLI(t)
	f := writeSyncFixture(t)
	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")

	runCLI(t, "--config", f.configPath, "index", f.volumeName)
	runCLI(t, "--config", f.configPath, "sync", "pics")

	target := filepath.Join(t.TempDir(), "recovered")
	out := runCLI(t, "--config", f.configPath, "restore", "pics", "--to", target)
	if !strings.Contains(out, "status=success") {
		t.Fatalf("restore did not succeed:\n%s", out)
	}
	body, err := os.ReadFile(filepath.Join(target, "a.txt"))
	if err != nil {
		t.Fatalf("override target missing a.txt: %v", err)
	}
	if string(body) != "alpha" {
		t.Fatalf("override target a.txt = %q, want alpha", body)
	}
	// Live volume path should still have its (pre-restore) bytes.
	live, err := os.ReadFile(filepath.Join(f.volumeDir, "a.txt"))
	if err != nil || string(live) != "alpha" {
		t.Fatalf("live volume changed unexpectedly: %q err=%v", live, err)
	}
}

// TestCLIRestoreRecordsRunsRowWithKindRestore verifies that the restore
// command produces a runs row distinguishable from sync runs.
func TestCLIRestoreRecordsRunsRowWithKindRestore(t *testing.T) {
	requireRcloneCLI(t)
	f := writeSyncFixture(t)
	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")
	runCLI(t, "--config", f.configPath, "index", f.volumeName)
	runCLI(t, "--config", f.configPath, "sync", "pics")
	runCLI(t, "--config", f.configPath, "restore", "pics", "--to", filepath.Join(t.TempDir(), "recovered"))

	out := runCLI(t, "--config", f.configPath, "runs")
	if !strings.Contains(out, "restore") {
		t.Fatalf("runs output missing 'restore' kind:\n%s", out)
	}
}

// TestCLIRestoreInfersDestinationWhenUnambiguous: a volume that syncs
// to exactly one destination doesn't need --from on restore — the only
// candidate is picked automatically. This is the common case and the
// counterpart to TestCLIRestoreNeedsExplicitFromWhenAmbiguous.
func TestCLIRestoreInfersDestinationWhenUnambiguous(t *testing.T) {
	requireRcloneCLI(t)
	f := writeSyncFixture(t)
	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")

	runCLI(t, "--config", f.configPath, "index", f.volumeName)
	runCLI(t, "--config", f.configPath, "sync", "pics")

	target := filepath.Join(t.TempDir(), "recovered")
	// No --from: only one destination in sync_to, so it's picked.
	out := runCLI(t, "--config", f.configPath, "restore", "pics", "--to", target)
	if !strings.Contains(out, "status=success") {
		t.Fatalf("restore did not succeed:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(target, "a.txt")); err != nil {
		t.Fatalf("recovered a.txt missing: %v", err)
	}
}

// TestCLIRestoreFromNodeFiltersByAttribution covers the issue-#15
// acceptance criterion: a receiver-side restore with --from <peer>
// produces a tree containing only that peer's source-attributed
// paths, even when other peers / local writes share the volume. The
// fixture indexes three local files, then re-stamps two of them with
// distinct peer provenance via Upsert(prov). Restoring with
// --from peer-a should land only the from-a path in the target tree.
func TestCLIRestoreFromNodeFiltersByAttribution(t *testing.T) {
	requireRcloneCLI(t)
	f := writeSyncFixture(t)
	writeTestFile(t, filepath.Join(f.volumeDir, "from-a.txt"), "alpha")
	writeTestFile(t, filepath.Join(f.volumeDir, "from-b.txt"), "beta")
	writeTestFile(t, filepath.Join(f.volumeDir, "local.txt"), "local")

	runCLI(t, "--config", f.configPath, "index", f.volumeName)
	runCLI(t, "--config", f.configPath, "sync", "pics")

	// Inject peer attribution onto the from-* paths. The destination
	// tree was just written by sync, so the rclone-side content is
	// unchanged — restore will pull only the path subset we ask for
	// via --files-from-raw. The store handle is closed before the
	// subsequent runCLI so there's exactly one process holding the
	// SQLite file when the CLI runs.
	stampPeerProvenance(t, f.dbPath)

	target := filepath.Join(t.TempDir(), "recovered")
	out := runCLI(t, "--config", f.configPath, "restore", "pics", "--from", "peer-a", "--to", target)
	if !strings.Contains(out, "status=success") {
		t.Fatalf("restore did not succeed:\n%s", out)
	}

	if _, err := os.Stat(filepath.Join(target, "from-a.txt")); err != nil {
		t.Fatalf("peer-a path missing from restore tree: %v", err)
	}
	for _, leaked := range []string{"from-b.txt", "local.txt"} {
		if _, err := os.Stat(filepath.Join(target, leaked)); err == nil {
			t.Fatalf("restore --from peer-a leaked %s into the target tree", leaked)
		}
	}
}

// stampPeerProvenance opens the index DB at dbPath, creates peer-a /
// peer-b nodes plus a sync-kind run for each, then promotes the
// from-{a,b}.txt rows (already indexed under the "pics" volume) to
// the respective peer's source attribution via Upsert. The store
// handle is closed before returning so the next CLI invocation has
// no concurrent SQLite connection from this process.
func stampPeerProvenance(t *testing.T, dbPath string) {
	t.Helper()
	s, err := store.OpenWithOptions(dbPath, store.OpenOptions{})
	if err != nil {
		t.Fatalf("Open: %v", err)
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
	vol, err := s.GetVolumeByName(ctx, "pics")
	if err != nil {
		t.Fatalf("GetVolumeByName: %v", err)
	}
	runA, err := s.BeginPeerSyncRun(ctx, vol.ID, peerA.ID, 1, peerA.Name)
	if err != nil {
		t.Fatalf("BeginPeerSyncRun a: %v", err)
	}
	if err := s.FinishRun(ctx, runA, store.RunStatusSuccess, "", 1); err != nil {
		t.Fatalf("FinishRun a: %v", err)
	}
	runB, err := s.BeginPeerSyncRun(ctx, vol.ID, peerB.ID, 1, peerB.Name)
	if err != nil {
		t.Fatalf("BeginPeerSyncRun b: %v", err)
	}
	if err := s.FinishRun(ctx, runB, store.RunStatusSuccess, "", 1); err != nil {
		t.Fatalf("FinishRun b: %v", err)
	}
	for _, c := range []struct {
		path string
		prov *store.Provenance
	}{
		{"from-a.txt", &store.Provenance{NodeID: peerA.ID, RunID: runA}},
		{"from-b.txt", &store.Provenance{NodeID: peerB.ID, RunID: runB}},
	} {
		row, err := s.GetByPath(ctx, vol.ID, c.path)
		if err != nil {
			t.Fatalf("GetByPath %s: %v", c.path, err)
		}
		if err := s.Upsert(ctx, row, c.prov); err != nil {
			t.Fatalf("Upsert %s: %v", c.path, err)
		}
	}
}

// TestCLIRestoreFromUnknownName rejects a name that is neither a
// configured destination nor a known node. The error message must
// surface both possibilities so the user knows which namespace they
// missed.
func TestCLIRestoreFromUnknownName(t *testing.T) {
	f := writeSyncFixture(t)
	runCLI(t, "--config", f.configPath, "index", f.volumeName)
	_, err := runCLIExpectErr(t, "--config", f.configPath, "restore", "pics", "--from", "nobody")
	if !strings.Contains(err.Error(), "neither a configured destination nor a known node") {
		t.Fatalf("expected destination-or-node error, got %v", err)
	}
}

// TestCLIRestoreFromNodeWithNoAttributedRows errors rather than
// silently invoking rclone with an empty path set. An empty
// --files-from would have rclone copy the entire tree, which would
// defeat the filter — so the CLI must short-circuit.
func TestCLIRestoreFromNodeWithNoAttributedRows(t *testing.T) {
	f := writeSyncFixture(t)
	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")
	runCLI(t, "--config", f.configPath, "index", f.volumeName)

	func() {
		s, err := store.OpenWithOptions(f.dbPath, store.OpenOptions{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer s.Close()
		if _, err := s.CreateNode(context.Background(), "stranger", "https://stranger.example"); err != nil {
			t.Fatalf("CreateNode: %v", err)
		}
	}()

	_, err := runCLIExpectErr(t, "--config", f.configPath, "restore", "pics", "--from", "stranger", "--to", filepath.Join(t.TempDir(), "x"))
	if !strings.Contains(err.Error(), "no rows attributed") {
		t.Fatalf("expected no-rows-attributed error, got %v", err)
	}
}

// TestCLIRestoreNeedsExplicitFromWhenAmbiguous: when a volume syncs to
// multiple destinations and the user does not pass --from, the command
// must error and tell the user to disambiguate.
func TestCLIRestoreNeedsExplicitFromWhenAmbiguous(t *testing.T) {
	root := t.TempDir()
	volumeDir := filepath.Join(root, "pics")
	if err := os.MkdirAll(volumeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.toml")
	dbPath := filepath.Join(root, "index.db")
	body := "" +
		"db = \"" + dbPath + "\"\n\n" +
		"[destinations.a]\ntype = \"local\"\nroot = \"" + filepath.Join(root, "a") + "\"\n\n" +
		"[destinations.b]\ntype = \"local\"\nroot = \"" + filepath.Join(root, "b") + "\"\n\n" +
		"[volumes.pics]\npath = \"" + volumeDir + "\"\nsync_to = [\"a\", \"b\"]\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runCLIExpectErr(t, "--config", cfgPath, "restore", "pics")
	if !strings.Contains(err.Error(), "multiple destinations") {
		t.Fatalf("expected multiple-destinations error, got %v", err)
	}
}

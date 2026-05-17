package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

// sourceFixture lays out a config with a `pics` volume, indexes a few
// files, then injects synthetic peer-attribution rows directly into
// the store so the read-side CLI can be exercised without a full
// daemon round-trip. The peer's name is "peer-a"; the self-node's
// name comes from the config's `node_name = "self-host"`.
type sourceFixture struct {
	configPath string
	dbPath     string
	volumeDir  string
	selfName   string
	peerAName  string
	peerBName  string
	peerBID    int64
	localPath  string
	peerAPath  string
	peerBPath  string
}

func writeSourceFixture(t *testing.T) sourceFixture {
	t.Helper()
	root := t.TempDir()
	volumeDir := filepath.Join(root, "pics")
	if err := os.MkdirAll(volumeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	localPath := "local.txt"
	peerAPath := "from-a.txt"
	peerBPath := "from-b.txt"
	writeTestFile(t, filepath.Join(volumeDir, localPath), "local content")
	writeTestFile(t, filepath.Join(volumeDir, peerAPath), "from-a content")
	writeTestFile(t, filepath.Join(volumeDir, peerBPath), "from-b content")

	dbPath := filepath.Join(root, "index.db")
	configPath := filepath.Join(root, "config.toml")
	body := "" +
		"db = \"" + dbPath + "\"\n" +
		"node_name = \"self-host\"\n\n" +
		"[volumes.pics]\npath = \"" + volumeDir + "\"\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Index once so the DB has rows; provenance is initially NULL for all.
	runCLI(t, "--config", configPath, "index", "pics")

	// Promote the from-a / from-b rows to peer attribution by re-upserting
	// with an explicit Provenance pointer. Upsert preserves blake3 and only
	// rewrites the mutable columns; we keep first/last_seen_run_id and
	// blake3 the same to avoid disturbing the prior state.
	s, err := store.OpenWithOptions(dbPath, store.OpenOptions{NodeName: "self-host"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

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
	// Synthetic peer-sync runs to serve as the FK target for SourceRunID.
	peerARun, err := s.BeginPeerSyncRun(ctx, vol.ID, peerA.ID, 901, peerA.Name)
	if err != nil {
		t.Fatalf("BeginPeerSyncRun a: %v", err)
	}
	peerBRun, err := s.BeginPeerSyncRun(ctx, vol.ID, peerB.ID, 902, peerB.Name)
	if err != nil {
		t.Fatalf("BeginPeerSyncRun b: %v", err)
	}
	if err := s.FinishRun(ctx, peerARun, store.RunStatusSuccess, "", 1); err != nil {
		t.Fatalf("FinishRun a: %v", err)
	}
	if err := s.FinishRun(ctx, peerBRun, store.RunStatusSuccess, "", 1); err != nil {
		t.Fatalf("FinishRun b: %v", err)
	}

	stamp := func(relPath string, prov *store.Provenance) {
		t.Helper()
		row, err := s.GetByPath(ctx, vol.ID, relPath)
		if err != nil {
			t.Fatalf("GetByPath %s: %v", relPath, err)
		}
		if err := s.Upsert(ctx, row, prov); err != nil {
			t.Fatalf("Upsert %s with prov: %v", relPath, err)
		}
	}
	stamp(peerAPath, &store.Provenance{NodeID: peerA.ID, RunID: peerARun})
	stamp(peerBPath, &store.Provenance{NodeID: peerB.ID, RunID: peerBRun})

	return sourceFixture{
		configPath: configPath,
		dbPath:     dbPath,
		volumeDir:  volumeDir,
		selfName:   "self-host",
		peerAName:  "peer-a",
		peerBName:  "peer-b",
		peerBID:    peerB.ID,
		localPath:  localPath,
		peerAPath:  peerAPath,
		peerBPath:  peerBPath,
	}
}

// TestCLIQueryFromNodeListsAttributedRows is the acceptance test for
// `squirrel query --from <peer>`: only that peer's source-attributed
// rows appear, regardless of whether other rows live in the same
// volume.
func TestCLIQueryFromNodeListsAttributedRows(t *testing.T) {
	f := writeSourceFixture(t)
	out := runCLI(t, "--config", f.configPath, "query", "--from", f.peerAName)
	if !strings.Contains(out, f.peerAPath) {
		t.Fatalf("missing peer-a path:\n%s", out)
	}
	if strings.Contains(out, f.peerBPath) || strings.Contains(out, f.localPath) {
		t.Fatalf("output contains rows that are not peer-a's:\n%s", out)
	}
}

// TestCLIQueryFromSelfListsLocalWrites checks the special case: the
// self-node's name resolves to "NULL source_node_id" — i.e. rows
// without peer attribution.
func TestCLIQueryFromSelfListsLocalWrites(t *testing.T) {
	f := writeSourceFixture(t)
	out := runCLI(t, "--config", f.configPath, "query", "--from", f.selfName)
	if !strings.Contains(out, f.localPath) {
		t.Fatalf("missing local row:\n%s", out)
	}
	if strings.Contains(out, f.peerAPath) || strings.Contains(out, f.peerBPath) {
		t.Fatalf("self filter leaked peer rows:\n%s", out)
	}
}

// TestCLIQueryHashAndFromComposes: `squirrel query <hash> --from <peer>`
// returns the row only when the supplied node is its source.
func TestCLIQueryHashAndFromComposes(t *testing.T) {
	f := writeSourceFixture(t)
	// Lift the from-a row's blake3 hex out via a plain path query.
	hash := extractField(t,
		runCLI(t, "--config", f.configPath, "query", filepath.Join(f.volumeDir, f.peerAPath)),
		"blake3:")
	// peer-a matches — the row's source.
	out := runCLI(t, "--config", f.configPath, "query", hash, "--from", f.peerAName)
	if !strings.Contains(out, f.peerAPath) {
		t.Fatalf("hash+--from peer-a missing row:\n%s", out)
	}
	// peer-b doesn't — the row's source is peer-a.
	_, err := runCLIExpectErr(t, "--config", f.configPath, "query", hash, "--from", f.peerBName)
	if !strings.Contains(err.Error(), "no rows for blake3") {
		t.Fatalf("hash+--from peer-b should report no rows, got %v", err)
	}
}

// TestCLIQueryUnknownFromName rejects an unknown name with a message
// that nudges the user toward the self-node convention.
func TestCLIQueryUnknownFromName(t *testing.T) {
	f := writeSourceFixture(t)
	_, err := runCLIExpectErr(t, "--config", f.configPath, "query", "--from", "ghost")
	if !strings.Contains(err.Error(), "no node named") {
		t.Fatalf("expected no-node-named error, got %v", err)
	}
}

// TestCLIRunsRendersPeerAndCorrelated checks the runs listing surfaces
// peer name + correlated_run_id for peer-sync rows. The synthetic
// fixture creates two such runs (one per peer) via BeginPeerSyncRun.
func TestCLIRunsRendersPeerAndCorrelated(t *testing.T) {
	f := writeSourceFixture(t)
	out := runCLI(t, "--config", f.configPath, "runs")
	if !strings.Contains(out, "PEER") {
		t.Fatalf("runs header missing PEER column:\n%s", out)
	}
	if !strings.Contains(out, f.peerAName+" correlated=901") {
		t.Fatalf("missing peer-a + correlated_run_id row:\n%s", out)
	}
	if !strings.Contains(out, f.peerBName+" correlated=902") {
		t.Fatalf("missing peer-b + correlated_run_id row:\n%s", out)
	}
}

// TestCLIQueryDuplicatesAndFromComposes verifies the post-filter
// against --duplicates: rows that share a hash are filtered down to
// the ones whose source matches.
func TestCLIQueryDuplicatesAndFromComposes(t *testing.T) {
	f := writeSourceFixture(t)
	// Add a duplicate of from-a content under a new path attributed to
	// peer-b — that creates two hash-matched rows with different
	// sources, which is exactly what --from must disambiguate.
	dupRel := "dup.txt"
	writeTestFile(t, filepath.Join(f.volumeDir, dupRel), "from-a content")
	runCLI(t, "--config", f.configPath, "index", "pics")
	// Stamp the new row with peer-b provenance.
	s, _ := store.OpenWithOptions(f.dbPath, store.OpenOptions{NodeName: f.selfName})
	defer s.Close()
	ctx := context.Background()
	vol, _ := s.GetVolumeByName(ctx, "pics")
	row, _ := s.GetByPath(ctx, vol.ID, dupRel)
	if err := s.Upsert(ctx, row, &store.Provenance{NodeID: f.peerBID, RunID: mustLatestPeerRun(t, s, ctx, f.peerBID)}); err != nil {
		t.Fatalf("Upsert dup with peer-b prov: %v", err)
	}

	out := runCLI(t, "--config", f.configPath, "query", "--duplicates", "--from", f.peerBName)
	if !strings.Contains(out, dupRel) {
		t.Fatalf("duplicates --from peer-b missing %s:\n%s", dupRel, out)
	}
	if strings.Contains(out, f.peerAPath) {
		t.Fatalf("duplicates --from peer-b leaked peer-a row:\n%s", out)
	}
}

// mustLatestPeerRun returns the highest runs.id for kind='sync' with
// the given peer_node_id. Used to satisfy the Provenance.RunID FK
// when stamping a synthetic row in tests.
func mustLatestPeerRun(t *testing.T, s *store.Store, ctx context.Context, peerNodeID int64) int64 {
	t.Helper()
	runs, err := s.ListRunsByPeer(ctx, peerNodeID, 1)
	if err != nil {
		t.Fatalf("ListRunsByPeer: %v", err)
	}
	if len(runs) == 0 {
		t.Fatalf("no peer-sync runs for peer id=%d", peerNodeID)
	}
	return runs[0].ID
}

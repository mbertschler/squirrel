package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/store"
)

// sourceFixture lays out a config with a `pics` volume, indexes one
// local file, then introduces two peer-origin files directly into the
// store (the way a receiver's /close records freshly transferred
// content) so the read-side CLI can be exercised without a full agent
// round-trip. The peers are "peer-a" / "peer-b"; the self-node's name
// comes from the config's `node_name = "self-host"`.
type sourceFixture struct {
	configPath string
	volumeDir  string
	selfName   string
	peerAName  string
	peerBName  string
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

	dbPath := filepath.Join(root, "index.db")
	configPath := filepath.Join(root, "config.toml")
	body := "" +
		"db = \"" + dbPath + "\"\n" +
		"node_name = \"self-host\"\n\n" +
		"[volumes.pics]\npath = \"" + volumeDir + "\"\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Index the local file so its content carries a NULL (local) origin.
	runCLI(t, "--config", configPath, "index", "pics")

	// Introduce the from-a / from-b files as peer-origin content: bytes
	// on disk plus an Upsert with an explicit Provenance, the same shape
	// a receiver's /close write has. Origin is recorded on the contents
	// row at first introduction, so these files must enter the store via
	// the peer write, not via a local index run. The store handle is
	// opened and closed here, before any subsequent runCLI runs, so the
	// test CLI has the DB to itself when it opens its own store.
	writeTestFile(t, filepath.Join(volumeDir, peerAPath), "from-a content")
	writeTestFile(t, filepath.Join(volumeDir, peerBPath), "from-b content")
	stampSourceFixture(t, dbPath, peerAPath, peerBPath)

	return sourceFixture{
		configPath: configPath,
		volumeDir:  volumeDir,
		selfName:   "self-host",
		peerAName:  "peer-a",
		peerBName:  "peer-b",
		localPath:  localPath,
		peerAPath:  peerAPath,
		peerBPath:  peerBPath,
	}
}

// stampSourceFixture creates peer-a / peer-b nodes plus a sync run
// for each and records peerAPath as content introduced by peer-a and
// peerBPath by peer-b (hashing the on-disk bytes so a later index run
// re-observes them unchanged). The store handle is opened, used, and
// closed within the function so concurrent connections from runCLI
// calls can't race with this fixture phase.
func stampSourceFixture(t *testing.T, dbPath, peerAPath, peerBPath string) {
	t.Helper()
	s, err := store.OpenWithOptions(dbPath, store.OpenOptions{NodeName: "self-host"})
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
	for _, c := range []struct {
		path  string
		runID int64
		prov  *store.Provenance
	}{
		{peerAPath, peerARun, &store.Provenance{NodeID: peerA.ID, RunID: peerARun}},
		{peerBPath, peerBRun, &store.Provenance{NodeID: peerB.ID, RunID: peerBRun}},
	} {
		data, err := os.ReadFile(filepath.Join(vol.Path, c.path))
		if err != nil {
			t.Fatalf("read %s: %v", c.path, err)
		}
		digest := blake3.Sum256(data)
		if err := s.Upsert(ctx, store.FileRow{
			VolumeID: vol.ID, Path: c.path, Blake3: digest[:],
			SizeBytes: int64(len(data)), MtimeNs: 1, Status: store.StatusPresent,
			FirstSeenRunID: c.runID, LastSeenRunID: c.runID, IndexedAtNs: 1,
		}, c.prov); err != nil {
			t.Fatalf("Upsert %s: %v", c.path, err)
		}
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
// against --duplicates under content-level origin: every path sharing
// a hash points at one contents row, so --from <peer> lists all of the
// duplicate paths when the content originates at that peer and none of
// them otherwise.
func TestCLIQueryDuplicatesAndFromComposes(t *testing.T) {
	f := writeSourceFixture(t)
	// Duplicate the peer-a content under a new path. The new path's row
	// resolves to the existing contents row, so it inherits peer-a's
	// origin even though a local index run observed it.
	dupRel := "dup.txt"
	writeTestFile(t, filepath.Join(f.volumeDir, dupRel), "from-a content")
	runCLI(t, "--config", f.configPath, "index", "pics")

	out := runCLI(t, "--config", f.configPath, "query", "--duplicates", "--from", f.peerAName)
	for _, want := range []string{f.peerAPath, dupRel} {
		if !strings.Contains(out, want) {
			t.Fatalf("duplicates --from peer-a missing %s:\n%s", want, out)
		}
	}
	outB := runCLI(t, "--config", f.configPath, "query", "--duplicates", "--from", f.peerBName)
	if strings.Contains(outB, dupRel) || strings.Contains(outB, f.peerAPath) {
		t.Fatalf("duplicates --from peer-b leaked peer-a-origin content:\n%s", outB)
	}
}

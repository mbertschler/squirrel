package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/agent"
	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// pullDurabilityFixture wires an in-process receiver agent (own store,
// seeded destination vector) and an initiator config whose [nodes.nas]
// block dials the httptest URL, so the CLI command runs end-to-end
// without a network listener.
type pullDurabilityFixture struct {
	configPath string
	dbPath     string
	recvStore  *store.Store
}

func newPullDurabilityFixture(t *testing.T) pullDurabilityFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	recvVolPath := filepath.Join(root, "recv", "pics")
	if err := os.MkdirAll(recvVolPath, 0o755); err != nil {
		t.Fatalf("mkdir receiver volume: %v", err)
	}
	recvStore, err := store.OpenWithOptions(filepath.Join(root, "recv.db"), store.OpenOptions{NodeName: "nas"})
	if err != nil {
		t.Fatalf("open receiver store: %v", err)
	}
	t.Cleanup(func() { _ = recvStore.Close() })
	srv, err := agent.New(agent.Config{
		Listen:  "127.0.0.1:0",
		Token:   "test-token",
		Version: "test",
		Volumes: map[string]*config.Volume{"pics": {Name: "pics", Path: recvVolPath}},
	}, recvStore)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	v, err := recvStore.CreateVolume(ctx, "pics", recvVolPath)
	if err != nil {
		t.Fatalf("seed receiver volume: %v", err)
	}
	self, err := recvStore.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	if err := recvStore.UpsertDestinationRunID(ctx, v.ID, "offsite-a", self.ID, 12, false); err != nil {
		t.Fatalf("seed receiver component: %v", err)
	}

	srcVol := filepath.Join(root, "src")
	if err := os.MkdirAll(srcVol, 0o755); err != nil {
		t.Fatalf("mkdir source volume: %v", err)
	}
	writeTestFile(t, filepath.Join(srcVol, "a.txt"), "alpha")
	dbPath := filepath.Join(root, "index.db")
	configPath := filepath.Join(root, "config.toml")
	body := fmt.Sprintf(`db = %q

[volumes.pics]
path = %q
offload_requires = ["offsite-a"]

[nodes.nas]
endpoint = %q
path     = %q
auth     = { bearer = "test-token" }
`, dbPath, srcVol, ts.URL, filepath.Join(root, "recv"))
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return pullDurabilityFixture{configPath: configPath, dbPath: dbPath, recvStore: recvStore}
}

// TestCLIPeerSyncPullDurability drives the standalone pull end-to-end:
// the peer's component lands in the local destination_run_ids and the
// summary line reports it. A locally higher component then makes the
// re-pull fail with a rewind refusal, and --allow-rewind accepts it.
func TestCLIPeerSyncPullDurability(t *testing.T) {
	f := newPullDurabilityFixture(t)
	ctx := context.Background()
	runCLI(t, "--config", f.configPath, "index", "pics")

	out := runCLI(t, "--config", f.configPath, "peer-sync", "pull-durability", "pics", "nas")
	if !strings.Contains(out, "fetched=1 applied=1") {
		t.Fatalf("output = %q, want fetched=1 applied=1", out)
	}

	s, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatalf("open local store: %v", err)
	}
	v, err := s.GetVolumeByName(ctx, "pics")
	if err != nil {
		t.Fatalf("local volume: %v", err)
	}
	origin, err := s.GetNodeByName(ctx, "nas")
	if err != nil {
		t.Fatalf("local origin row for nas: %v", err)
	}
	got, err := s.GetDestinationRunID(ctx, v.ID, "offsite-a", origin.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.OriginRunID != 12 {
		t.Fatalf("offsite-a component = %d, want 12", got.OriginRunID)
	}
	// Raise the local floor above the peer's value, then close so the
	// CLI invocations below see the row (the store serialises on one
	// connection).
	if err := s.UpsertDestinationRunID(ctx, v.ID, "offsite-a", origin.ID, 20, false); err != nil {
		t.Fatalf("raise local floor: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close local store: %v", err)
	}

	out, err = runCLIExpectErr(t, "--config", f.configPath, "peer-sync", "pull-durability", "pics", "nas")
	if !strings.Contains(err.Error(), "--allow-rewind") {
		t.Fatalf("err = %v, want a pointer at --allow-rewind", err)
	}
	if !strings.Contains(out, "refused rewind") {
		t.Fatalf("output = %q, want a refused-rewind line", out)
	}

	out = runCLI(t, "--config", f.configPath, "peer-sync", "pull-durability", "pics", "nas", "--allow-rewind")
	if !strings.Contains(out, "fetched=1 applied=1") {
		t.Fatalf("override output = %q, want fetched=1 applied=1", out)
	}
}

// TestCLIPeerSyncPullDurabilityUnknownNames: unknown volume and node
// names fail fast with the same diagnostics the sync pairing uses.
func TestCLIPeerSyncPullDurabilityUnknownNames(t *testing.T) {
	f := newPullDurabilityFixture(t)

	_, err := runCLIExpectErr(t, "--config", f.configPath, "peer-sync", "pull-durability", "ghost", "nas")
	if !strings.Contains(err.Error(), `unknown volume "ghost"`) {
		t.Fatalf("err = %v, want unknown volume", err)
	}
	_, err = runCLIExpectErr(t, "--config", f.configPath, "peer-sync", "pull-durability", "pics", "ghost")
	if !strings.Contains(err.Error(), `unknown node "ghost"`) {
		t.Fatalf("err = %v, want unknown node", err)
	}
}

package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/mbertschler/squirrel/agent"
	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/syncproto"
)

// walkStats counts the wire calls a sync session makes against the
// receiver, plus the largest /plan entry-count seen. The Merkle walk's
// asymptotic contract — O(depth) folder requests and O(differing-leaf-
// files) /plan entries — is asserted by reading these counters after
// the sync returns.
type walkStats struct {
	planFoldersCalls atomic.Int64
	planFolderPaths  atomic.Int64
	planCalls        atomic.Int64
	maxPlanEntries   atomic.Int64
}

// instrumentAgent wraps the agent's HTTP handler with a counting
// middleware that snoops on /v1/sync/plan-folders and /v1/sync/plan.
// Bodies are buffered, decoded, counted, then handed off unchanged —
// the agent sees byte-for-byte the same request.
func instrumentAgent(h http.Handler, stats *walkStats) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sync/plan-folders":
			body, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			var req syncproto.PlanFoldersRequest
			if err := json.Unmarshal(body, &req); err == nil {
				stats.planFoldersCalls.Add(1)
				stats.planFolderPaths.Add(int64(len(req.Paths)))
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		case "/v1/sync/plan":
			body, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			var req syncproto.PlanRequest
			if err := json.Unmarshal(body, &req); err == nil {
				stats.planCalls.Add(1)
				if cur, n := stats.maxPlanEntries.Load(), int64(len(req.Entries)); n > cur {
					stats.maxPlanEntries.Store(n)
				}
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		h.ServeHTTP(w, r)
	})
}

// buildInstrumentedFixture is a near-clone of buildNodeFixture that
// also wires a walkStats counter through an http handler middleware.
// Kept separate so the unrelated nodeFixture stays simple.
func buildInstrumentedFixture(t *testing.T) (*nodeFixture, *walkStats) {
	t.Helper()
	root := t.TempDir()
	initVolPath := filepath.Join(root, "init", "pics")
	recvVolRoot := filepath.Join(root, "recv")
	recvVolPath := filepath.Join(recvVolRoot, "pics")
	for _, p := range []string{initVolPath, recvVolPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	initStore := openStoreWithName(t, filepath.Join(root, "init.db"), "init")
	recvStore := openStoreWithName(t, filepath.Join(root, "recv.db"), "nas")

	initVol := &config.Volume{Name: "pics", Path: initVolPath}
	recvVol := &config.Volume{Name: "pics", Path: recvVolPath}

	srv, err := agent.New(agent.Config{
		Listen:  "127.0.0.1:0",
		Token:   "test-token",
		Version: "test",
		Volumes: map[string]*config.Volume{"pics": recvVol},
	}, recvStore)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	stats := &walkStats{}
	ts := httptest.NewServer(instrumentAgent(srv.Handler(), stats))
	t.Cleanup(ts.Close)

	endpoint, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test URL: %v", err)
	}
	node := &config.Node{
		Name:     "nas",
		Endpoint: endpoint,
		Token:    "test-token",
		Path:     recvVolRoot,
	}
	return &nodeFixture{
		initStore: initStore,
		recvStore: recvStore,
		initVol:   initVol,
		recvVol:   recvVol,
		node:      node,
		server:    ts,
	}, stats
}

// TestNodeSyncMerkleWalkSecondSyncIsScoped is the issue's acceptance
// test for the protocol change: after one round of full sync, modifying
// a handful of files in a single leaf folder must produce a /plan
// request whose entries are scoped to that leaf — not the whole volume.
// The Merkle walk's promise is that incremental sync cost is decoupled
// from volume size; the assertion that the entry count equals the leaf
// folder's file count, not the volume's, is what holds that promise.
func TestNodeSyncMerkleWalkSecondSyncIsScoped(t *testing.T) {
	f, stats := buildInstrumentedFixture(t)
	rcl := requireRclone(t)
	rcl.Config = filepath.Join(filepath.Dir(f.initVol.Path), "rclone.conf")
	if err := os.WriteFile(rcl.Config, []byte{}, 0o600); err != nil {
		t.Fatalf("write rclone.conf: %v", err)
	}
	f.rcl = rcl

	const filesPerLeaf = 32
	leaves := []string{
		"a/b/c1", "a/b/c2",
		"a/d/c3", "a/d/c4",
		"x/y/c5", "x/y/c6",
		"x/z/c7", "x/z/c8",
	}
	for _, leaf := range leaves {
		if err := os.MkdirAll(filepath.Join(f.initVol.Path, leaf), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", leaf, err)
		}
		for i := range filesPerLeaf {
			body := fmt.Sprintf("%s-file-%d", leaf, i)
			full := filepath.Join(f.initVol.Path, leaf, fmt.Sprintf("f%02d.txt", i))
			if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
				t.Fatalf("write %s: %v", full, err)
			}
		}
	}
	f.indexInitiator(t)
	totalFiles := len(leaves) * filesPerLeaf

	// First sync: every file transfers and lands on the receiver.
	rep1, err := SyncNode(context.Background(), f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true})
	if err != nil {
		t.Fatalf("first SyncNode: %v (rep=%+v)", err, rep1)
	}
	if rep1.Status != store.RunStatusSuccess {
		t.Fatalf("first sync status = %q, want success", rep1.Status)
	}
	if int(stats.maxPlanEntries.Load()) != totalFiles {
		t.Fatalf("first sync /plan entries = %d, want %d (full volume)", stats.maxPlanEntries.Load(), totalFiles)
	}

	// Reset counters; modify two files in one leaf and re-sync.
	stats.planFoldersCalls.Store(0)
	stats.planFolderPaths.Store(0)
	stats.planCalls.Store(0)
	stats.maxPlanEntries.Store(0)

	dirtyLeaf := "a/b/c1"
	modifiedFiles := []string{"f00.txt", "f01.txt"}
	for _, name := range modifiedFiles {
		full := filepath.Join(f.initVol.Path, dirtyLeaf, name)
		if err := os.WriteFile(full, []byte("changed-"+name), 0o644); err != nil {
			t.Fatalf("rewrite %s: %v", name, err)
		}
	}
	f.indexInitiator(t)

	rep2, err := SyncNode(context.Background(), f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true})
	if err != nil {
		t.Fatalf("second SyncNode: %v (rep=%+v)", err, rep2)
	}
	if rep2.Status != store.RunStatusSuccess {
		t.Fatalf("second sync status = %q, want success", rep2.Status)
	}

	// The walk must have used /plan-folders at all (protocol_version >= 2
	// negotiated) — otherwise the asymptotic property doesn't hold.
	if stats.planFoldersCalls.Load() == 0 {
		t.Fatalf("no /plan-folders calls — Merkle walk did not run; protocol negotiation likely fell back to flat")
	}

	// /plan entries == files in the differing leaf, not the whole
	// volume. This is the headline acceptance assertion from #44.
	wantEntries := int64(filesPerLeaf)
	if stats.maxPlanEntries.Load() != wantEntries {
		t.Fatalf("second sync /plan entries = %d, want %d (only the dirty leaf's files)",
			stats.maxPlanEntries.Load(), wantEntries)
	}

	// /plan-folders requests should be O(depth) per round-trip — i.e.
	// at most one per level of the tree. The tree above is 4 deep
	// (root → a → a/b → a/b/c1) so the walk makes 4 round-trips. We
	// allow a small slack (≤ depth + 1) so an extra empty-leaf probe
	// doesn't make the test flake on unrelated refactors.
	if got := stats.planFoldersCalls.Load(); got < 1 || got > 5 {
		t.Fatalf("/plan-folders calls = %d, want 1..5 for a depth-4 tree", got)
	}

	// And the receiver actually got the new content.
	for _, name := range modifiedFiles {
		got, err := os.ReadFile(filepath.Join(f.recvVol.Path, dirtyLeaf, name))
		if err != nil {
			t.Fatalf("read %s/%s: %v", dirtyLeaf, name, err)
		}
		if want := "changed-" + name; string(got) != want {
			t.Fatalf("%s/%s = %q, want %q", dirtyLeaf, name, got, want)
		}
	}
}

// TestNodeSyncFallsBackToFlatPlanForLegacyReceiver simulates a
// legacy receiver that doesn't advertise ProtocolVersionMerkleWalk:
// the initiator must transparently fall back to the v1 full-volume
// /plan exchange. We force the legacy behaviour by stripping the
// receiver's ProtocolVersion field from /v1/sync/begin responses.
func TestNodeSyncFallsBackToFlatPlanForLegacyReceiver(t *testing.T) {
	f, stats := buildInstrumentedFixture(t)
	rcl := requireRclone(t)
	rcl.Config = filepath.Join(filepath.Dir(f.initVol.Path), "rclone.conf")
	if err := os.WriteFile(rcl.Config, []byte{}, 0o600); err != nil {
		t.Fatalf("write rclone.conf: %v", err)
	}
	f.rcl = rcl

	// Wrap the existing test server's handler with a /begin response
	// rewriter that zeroes ProtocolVersion. httptest doesn't expose
	// the existing handler, so we stand up a parallel server here.
	originalURL := f.server.URL
	directProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sync/begin" {
			rec := httptest.NewRecorder()
			forwardRequest(t, originalURL, r, rec)
			body := rec.Body.Bytes()
			var br syncproto.BeginResponse
			if err := json.Unmarshal(body, &br); err == nil {
				br.ProtocolVersion = 0
				body, _ = json.Marshal(br)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.Code)
			_, _ = w.Write(body)
			return
		}
		// Everything else passes straight through to the real server.
		forwardRequest(t, originalURL, r, w)
	}))
	t.Cleanup(directProxy.Close)
	proxyURL, _ := url.Parse(directProxy.URL)
	f.node.Endpoint = proxyURL

	if err := os.WriteFile(filepath.Join(f.initVol.Path, "single.txt"), []byte("once"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	f.indexInitiator(t)
	if _, err := SyncNode(context.Background(), f.initStore, f.rcl, f.initVol, f.node, Options{Shallow: true}); err != nil {
		t.Fatalf("SyncNode: %v", err)
	}
	if stats.planFoldersCalls.Load() != 0 {
		t.Fatalf("/plan-folders called %d times; legacy receiver should have forced the flat fallback",
			stats.planFoldersCalls.Load())
	}
	if stats.planCalls.Load() == 0 {
		t.Fatalf("/plan never called; the flat fallback should still drive at least one exchange")
	}
}

// forwardRequest copies r to baseURL+path and writes the response into
// w. Used by TestNodeSyncFallsBackToFlatPlanForLegacyReceiver to splice
// a response rewriter in front of the agent without re-implementing the
// agent handler.
func forwardRequest(t *testing.T, baseURL string, r *http.Request, w http.ResponseWriter) {
	t.Helper()
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, baseURL+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	maps.Copy(req.Header, r.Header)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	maps.Copy(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

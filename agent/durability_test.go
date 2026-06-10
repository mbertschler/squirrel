package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/syncproto"
)

// postDurability drives POST /v1/sync/durability against the server's
// handler and decodes the response into out. Returns the HTTP status.
func postDurability(t *testing.T, srv *Server, body syncproto.DurabilityRequest, out any) int {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/sync/durability", bytes.NewReader(encoded))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code
}

// TestDurabilityEndpointListsComponents: the endpoint returns every
// recorded vector component for the volume with origin nodes resolved
// to names — the cross-node identity the caller can map locally.
func TestDurabilityEndpointListsComponents(t *testing.T) {
	ctx := context.Background()
	vol := &config.Volume{Name: "pics", Path: t.TempDir()}
	srv := newTestServer(t, Config{Volumes: map[string]*config.Volume{vol.Name: vol}})

	v, err := srv.store.CreateVolume(ctx, vol.Name, vol.Path)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	self, err := srv.store.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	ext, err := srv.store.CreateNode(ctx, "ext", "peer://ext")
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	for _, seed := range []struct {
		dest   string
		nodeID int64
		run    int64
	}{
		{"offsite-a", self.ID, 12},
		{"offsite-a", ext.ID, 4},
		{"mirror", self.ID, 9},
	} {
		if err := srv.store.UpsertDestinationRunID(ctx, v.ID, seed.dest, seed.nodeID, seed.run, false); err != nil {
			t.Fatalf("seed %+v: %v", seed, err)
		}
	}

	var resp syncproto.DurabilityResponse
	if code := postDurability(t, srv, syncproto.DurabilityRequest{Volume: "pics"}, &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(resp.Components) != 3 {
		t.Fatalf("components = %d, want 3: %+v", len(resp.Components), resp.Components)
	}
	got := map[string]int64{}
	for _, c := range resp.Components {
		if c.UpdatedAtNs == 0 {
			t.Fatalf("component %+v has zero updated_at_ns", c)
		}
		got[c.Destination+"/"+c.OriginNode] = c.OriginRun
	}
	want := map[string]int64{
		"offsite-a/" + self.Name: 12,
		"offsite-a/ext":          4,
		"mirror/" + self.Name:    9,
	}
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("component %s = %d, want %d (full: %+v)", k, got[k], w, got)
		}
	}
}

// TestDurabilityEndpointGuards: an undeclared volume 404s, a missing
// volume name 400s, and a declared volume with no store row answers
// with an empty component list (a valid "nothing recorded yet").
func TestDurabilityEndpointGuards(t *testing.T) {
	vol := &config.Volume{Name: "pics", Path: t.TempDir()}
	srv := newTestServer(t, Config{Volumes: map[string]*config.Volume{vol.Name: vol}})

	if code := postDurability(t, srv, syncproto.DurabilityRequest{Volume: "ghost"}, nil); code != http.StatusNotFound {
		t.Fatalf("undeclared volume status = %d, want 404", code)
	}
	if code := postDurability(t, srv, syncproto.DurabilityRequest{}, nil); code != http.StatusBadRequest {
		t.Fatalf("missing volume status = %d, want 400", code)
	}
	var resp syncproto.DurabilityResponse
	if code := postDurability(t, srv, syncproto.DurabilityRequest{Volume: "pics"}, &resp); code != http.StatusOK {
		t.Fatalf("declared-but-unmaterialised volume status = %d, want 200", code)
	}
	if len(resp.Components) != 0 {
		t.Fatalf("components = %+v, want empty", resp.Components)
	}
}

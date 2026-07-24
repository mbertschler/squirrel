package sync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/mbertschler/squirrel/agent"
	"github.com/mbertschler/squirrel/config"
)

// capabilityTestNode spins an in-process agent backing volume "pics" with
// the given destinations and sync_to list, and returns a config.Node
// pointing at it (over httptest, no TCP port). It is the minimal receiver
// the capability probe needs — no rclone, no seeded index.
func capabilityTestNode(t *testing.T, dests map[string]*config.Destination, syncTo []string) *config.Node {
	t.Helper()
	root := t.TempDir()
	recvStore := openStoreWithName(t, filepath.Join(root, "recv.db"), "nas")
	recvVol := &config.Volume{Name: "pics", Path: filepath.Join(root, "pics"), SyncTo: syncTo}
	srv, err := agent.New(agent.Config{
		Listen:       "127.0.0.1:0",
		Token:        "test-token",
		Version:      "test",
		Volumes:      map[string]*config.Volume{"pics": recvVol},
		Destinations: dests,
	}, recvStore)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	endpoint, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test URL: %v", err)
	}
	return &config.Node{Name: "nas", Endpoint: endpoint, Token: "test-token"}
}

// TestGatherRelayedCapabilitiesReadsPeer: the probe reads the peer's
// advertised destination capabilities, keeps only the ones whose name is
// in the requested target set, and tags each with the owning peer (#145).
func TestGatherRelayedCapabilitiesReadsPeer(t *testing.T) {
	node := capabilityTestNode(t, map[string]*config.Destination{
		"cloudbox":  {Name: "cloudbox", Type: "sftp", Layout: config.LayoutMirror, Crypt: &config.Crypt{Password: "obscured"}},
		"s3archive": {Name: "s3archive", Type: "s3", Layout: config.LayoutPacked},
	}, []string{"cloudbox", "s3archive"})

	targets := map[string]struct{}{"cloudbox": {}, "s3archive": {}}
	caps, softErrs := GatherRelayedCapabilities(context.Background(), "pics", []*config.Node{node}, targets)
	if len(softErrs) != 0 {
		t.Fatalf("softErrs = %v, want none", softErrs)
	}
	byTarget := map[string]RelayedCapability{}
	for _, c := range caps {
		byTarget[c.Target] = c
	}
	if len(byTarget) != 2 {
		t.Fatalf("caps = %+v, want cloudbox and s3archive", caps)
	}
	if c := byTarget["cloudbox"]; c.CanGate || c.Peer != node.Name || c.Reason == "" {
		t.Fatalf("cloudbox verdict = %+v, want incapable, peer=%s, with reason", c, node.Name)
	}
	if c := byTarget["s3archive"]; !c.CanGate || c.Peer != node.Name {
		t.Fatalf("s3archive verdict = %+v, want capable, peer=%s", c, node.Name)
	}
}

// TestGatherRelayedCapabilitiesFiltersToTargets: capabilities the peer
// advertises for destinations outside the requested target set are dropped.
func TestGatherRelayedCapabilitiesFiltersToTargets(t *testing.T) {
	node := capabilityTestNode(t, map[string]*config.Destination{
		"cloudbox":  {Name: "cloudbox", Type: "sftp", Layout: config.LayoutMirror, Crypt: &config.Crypt{Password: "obscured"}},
		"s3archive": {Name: "s3archive", Type: "s3", Layout: config.LayoutPacked},
	}, []string{"cloudbox", "s3archive"})

	caps, _ := GatherRelayedCapabilities(context.Background(), "pics",
		[]*config.Node{node}, map[string]struct{}{"s3archive": {}})
	if len(caps) != 1 || caps[0].Target != "s3archive" {
		t.Fatalf("caps = %+v, want only s3archive (cloudbox is not a requested target)", caps)
	}
}

// TestGatherRelayedCapabilitiesUnreachablePeerIsSoft: an unreachable peer
// yields a soft advisory and no capability, never a hard error — the caller
// falls back to the per-file gate. The unreachable endpoint is a freshly
// started httptest server closed immediately: its address is guaranteed to
// be non-serving and OS-assigned, which is portable (unlike hardcoding a
// port that could happen to be in use).
func TestGatherRelayedCapabilitiesUnreachablePeerIsSoft(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test URL: %v", err)
	}
	ts.Close() // nothing listens on endpoint now

	dead := &config.Node{Name: "nas", Endpoint: endpoint, Token: "test-token"}
	caps, softErrs := GatherRelayedCapabilities(context.Background(), "pics",
		[]*config.Node{dead}, map[string]struct{}{"cloudbox": {}})
	if len(caps) != 0 {
		t.Fatalf("caps = %+v, want none from an unreachable peer", caps)
	}
	if len(softErrs) != 1 {
		t.Fatalf("softErrs = %v, want exactly one advisory", softErrs)
	}
}

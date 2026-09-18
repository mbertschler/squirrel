package config

import (
	"strings"
	"testing"
)

// TestNodeRejectsObsoleteBytePath is what is left of F34 once the error
// class is deleted rather than reported. A peer is reached by one address
// now, so `path` has no meaning — and an operator upgrading a working
// config must be told that in words, not left with the decoder's generic
// unrecognised-field complaint, which reads like a typo they should fix
// rather than a line they should delete.
func TestNodeRejectsObsoleteBytePath(t *testing.T) {
	p := writeConfig(t, `
[nodes.nas]
endpoint = "https://nas.local:8443"
path = "/mnt/nas-export"
auth = { bearer = "t0ken" }
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("Load accepted an obsolete node path; want a rejection")
	}
	for _, want := range []string{"path is obsolete", "delete the line"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want substring %q", err, want)
		}
	}
}

// TestNodeLoadsWithoutBytePath is the other half: a node block carrying
// only an endpoint and its bearer is now the complete shape, for a peer
// this machine syncs to as much as for one it only pulls durability
// evidence from. Nothing about moving bytes is configured separately.
func TestNodeLoadsWithoutBytePath(t *testing.T) {
	p := writeConfig(t, `
[volumes.pictures]
path = "/tmp/pictures"
sync_to = ["nas"]

[nodes.nas]
endpoint = "https://nas.local:8443"
auth = { bearer = "t0ken" }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Nodes["nas"]; got == nil || got.Endpoint.Host != "nas.local:8443" {
		t.Fatalf("nodes.nas = %+v", cfg.Nodes["nas"])
	}
}

package config

import (
	"strings"
	"testing"
)

// TestCanEverGateOffload pins the structural capability predicate the
// offload fail-fast reads: the incapable shape is an rclone mirror, plain or
// crypt (checksum or size+mtime, no fingerprint to upgrade it); every other
// shape can eventually contribute a content-verified component.
func TestCanEverGateOffload(t *testing.T) {
	crypt := &Crypt{Password: "obscured"}
	cases := []struct {
		name        string
		dest        Destination
		wantCapable bool
	}{
		{"mirror plain", Destination{Type: "sftp", Layout: LayoutMirror}, false},
		{"mirror crypt", Destination{Type: "sftp", Layout: LayoutMirror, Crypt: crypt}, false},
		{"content-addressed plain", Destination{Type: "sftp", Layout: LayoutContentAddressed}, true},
		{"content-addressed crypt", Destination{Type: "s3", Layout: LayoutContentAddressed, Crypt: crypt}, true},
		{"packed plain", Destination{Type: "s3", Layout: LayoutPacked}, true},
		{"packed crypt", Destination{Type: "s3", Layout: LayoutPacked, Crypt: crypt}, true},
		{"local mirror", Destination{Type: "local", Layout: LayoutMirror}, false},
		{"kopia mirror", Destination{Type: "kopia", Layout: LayoutMirror}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			capable, reason := c.dest.CanEverGateOffload()
			if capable != c.wantCapable {
				t.Fatalf("CanEverGateOffload() = %v (%q), want %v", capable, reason, c.wantCapable)
			}
			switch {
			case capable && reason != "":
				t.Fatalf("capable destination returned non-empty reason %q", reason)
			case !capable && !strings.Contains(reason, "mirror"):
				t.Fatalf("incapable reason %q should name the mirror layout", reason)
			}
		})
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nodeConfig builds a config with one peer node, whose `path` line is
// supplied by the caller (empty string omits the key entirely), and a
// volume whose sync_to is the caller's to choose. The two knobs are exactly
// the two halves of F34: whether a byte-path is declared, and whether any
// bytes travel through it.
func nodeConfig(t *testing.T, pathLine, syncTo string) string {
	t.Helper()
	return writeConfig(t, `
[volumes.pictures]
path = "/tmp/pictures"
`+syncTo+`

[nodes.nas]
endpoint = "https://nas.local:8443"
auth = { bearer = "t0ken" }
`+pathLine+`
`)
}

// TestNodeWithoutBytePathLoadsWhenNoBytesTravel is the half of F34 that
// deletes the error class rather than reporting it: a node that exists only
// so this machine can pull durability evidence from it moves no bytes, so
// demanding a byte-path would make the operator invent one to satisfy the
// validator — teaching them the field is decorative, about a field that
// silently eats bytes when it is wrong.
func TestNodeWithoutBytePathLoadsWhenNoBytesTravel(t *testing.T) {
	cfg, err := Load(nodeConfig(t, "", ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	node, ok := cfg.Nodes["nas"]
	if !ok {
		t.Fatalf("missing node 'nas': %#v", cfg.Nodes)
	}
	if node.Path != "" {
		t.Fatalf("Path = %q, want empty", node.Path)
	}
	if state, _ := node.CheckBytePath(); state != BytePathNone {
		t.Fatalf("CheckBytePath = %v, want BytePathNone", state)
	}
}

// TestNodeWithoutBytePathRejectedWhenSyncedTo is the other side: the
// requirement did not go away, it moved to where it is actually true.
func TestNodeWithoutBytePathRejectedWhenSyncedTo(t *testing.T) {
	_, err := Load(nodeConfig(t, "", `sync_to = ["nas"]`))
	if err == nil {
		t.Fatal("Load accepted a sync_to node with no byte-path, want an error")
	}
	// The message has to name both ends, because neither block is wrong on
	// its own — it is the pairing that is.
	for _, want := range []string{"nodes.nas", "volumes.pictures", "path is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestNodeWithBytePathLoadsWhenSyncedTo keeps the ordinary case honest.
func TestNodeWithBytePathLoadsWhenSyncedTo(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(nodeConfig(t, `path = "`+dir+`"`, `sync_to = ["nas"]`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state, reason := cfg.Nodes["nas"].CheckBytePath(); state != BytePathOK {
		t.Fatalf("CheckBytePath = %v (%s), want BytePathOK", state, reason)
	}
}

// TestCheckBytePathClassifies covers the states the status surfaces and
// `config check` both render. The unavailable cases are the ones that
// matter: each is a byte-path that looks configured but into which no byte
// will ever land.
func TestCheckBytePathClassifies(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	for _, tc := range []struct {
		name string
		path string
		want BytePathState
	}{
		{"existing directory", dir, BytePathOK},
		{"absent", filepath.Join(dir, "no-such-dir"), BytePathUnavailable},
		{"a file, not a directory", file, BytePathUnavailable},
		{"rclone remote spec", "nasremote:backups/squirrel", BytePathRemote},
		{"unset", "", BytePathNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &Node{Name: "nas", Path: tc.path}
			state, reason := n.CheckBytePath()
			if state != tc.want {
				t.Fatalf("CheckBytePath(%q) = %v (%s), want %v", tc.path, state, reason, tc.want)
			}
			if state != BytePathOK && reason == "" {
				t.Errorf("state %v carries no reason, so no surface can explain it", state)
			}
		})
	}
}

// TestBytePathRequirementReportsDeterministically pins the walk order: two
// volumes each missing the same node's path must always name the same one,
// or the error text flaps between loads of an unchanged file.
func TestBytePathRequirementReportsDeterministically(t *testing.T) {
	body := `
[volumes.aaa]
path = "/tmp/aaa"
sync_to = ["nas"]

[volumes.zzz]
path = "/tmp/zzz"
sync_to = ["nas"]

[nodes.nas]
endpoint = "https://nas.local:8443"
auth = { bearer = "t0ken" }
`
	p := writeConfig(t, body)
	first, err := Load(p)
	if err == nil {
		t.Fatalf("Load accepted the config: %#v", first)
	}
	for i := 0; i < 20; i++ {
		_, again := Load(p)
		if again == nil || again.Error() != err.Error() {
			t.Fatalf("error text flapped between loads: %q then %q", err, again)
		}
	}
	if !strings.Contains(err.Error(), "volumes.aaa") {
		t.Errorf("error %q should name the first volume in name order", err)
	}
}

package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hashFixture serves root from a test server that runs commands, and
// plants one artifact, objects/<key>, holding "alpha".
func hashFixture(t *testing.T, commands map[string]string) (*sftpTransport, string) {
	t.Helper()
	srv := startSFTPServer(t)
	srv.runsHashCommands(commands)
	root := t.TempDir()
	key := strings.Repeat("ab", 32)
	if err := os.MkdirAll(filepath.Join(root, ObjectsDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ObjectsDirName, key), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := srv.destination(t, root)
	dest.HashAlgo = "sha256"
	tr := mustDialSFTP(t, dest)
	t.Cleanup(func() { _ = tr.Close() })
	return tr, ObjectsDirName + "/" + key
}

// TestServerHashRunsTheCommand: a server with the command hashes an
// artifact where it lies, and the value is what this machine computes.
func TestServerHashRunsTheCommand(t *testing.T) {
	tr, name := hashFixture(t, map[string]string{"sha256sum": "sha256"})
	got, err := tr.ServerHash(context.Background(), name)
	if err != nil {
		t.Fatalf("ServerHash: %v", err)
	}
	want := sha256.Sum256([]byte("alpha"))
	if got.Algo != "sha256" || got.Value != hex.EncodeToString(want[:]) {
		t.Fatalf("ServerHash = %+v, want sha256 %x", got, want)
	}
}

// TestServerHashRefusesWhatItCannotTrust: a server that runs no programs,
// or whose command computes another hash, offers no server hash; neither
// does a name that is no hex artifact name, a root the command line may not
// carry, or a local disk.
func TestServerHashRefusesWhatItCannotTrust(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		commands map[string]string
	}{
		{"no programs", nil},
		{"a command computing another hash", map[string]string{"sha256sum": "blake3"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr, name := hashFixture(t, c.commands)
			if _, err := tr.ServerHash(ctx, name); !errors.Is(err, errNoServerHash) {
				t.Fatalf("ServerHash = %v, want errNoServerHash", err)
			}
		})
	}
	tr, _ := hashFixture(t, map[string]string{"sha256sum": "sha256"})
	if err := os.WriteFile(filepath.Join(tr.root, "cat; rm -rf x.jpg"), []byte("meow"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.ServerHash(ctx, "cat; rm -rf x.jpg"); !errors.Is(err, errNoServerHash) {
		t.Fatalf("ServerHash of a user's file name = %v, want errNoServerHash", err)
	}
	tr.root += "/with space"
	unsafe := filepath.Join(tr.root, ObjectsDirName)
	if err := os.MkdirAll(unsafe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unsafe, strings.Repeat("ab", 32)), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.ServerHash(ctx, ObjectsDirName+"/"+strings.Repeat("ab", 32)); !errors.Is(err, errNoServerHash) {
		t.Fatalf("ServerHash under an unsafe root = %v, want errNoServerHash", err)
	}
	local, err := openLocalTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = local.Close() }()
	if _, err := local.ServerHash(ctx, "objects/x"); !errors.Is(err, errNoServerHash) {
		t.Fatalf("local ServerHash = %v, want errNoServerHash", err)
	}
}

// TestServerHashProbesOnce: the probe runs once per session, so a server
// that stops running the command afterwards fails the hash itself.
func TestServerHashProbesOnce(t *testing.T) {
	srvCommands := map[string]string{"sha256sum": "sha256"}
	tr, name := hashFixture(t, srvCommands)
	ctx := context.Background()
	if _, err := tr.ServerHash(ctx, name); err != nil {
		t.Fatalf("first ServerHash: %v", err)
	}
	tr.hashAlgo = "sha1"
	if _, err := tr.ServerHash(ctx, name); err == nil || errors.Is(err, errNoServerHash) {
		t.Fatalf("ServerHash after the probe = %v, want the command's own failure", err)
	}
}

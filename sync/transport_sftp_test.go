package sync

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/mbertschler/squirrel/config"
)

// sftpTestServer is an in-process ssh server whose sftp subsystem serves
// the real filesystem, with password "p" for user "u" and one client key
// allowed.
type sftpTestServer struct {
	addr     string
	hostKeys []ssh.Signer
	// clientKey is the private half of the one client key the server
	// accepts.
	clientKey ed25519.PrivateKey
}

func newKey(t *testing.T) (ed25519.PrivateKey, ssh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return priv, s
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, s := newKey(t)
	return s
}

// startSFTPServer serves until the test ends. With no host keys given it
// presents one fresh ed25519 key.
func startSFTPServer(t *testing.T, hostKeys ...ssh.Signer) *sftpTestServer {
	t.Helper()
	if len(hostKeys) == 0 {
		hostKeys = []ssh.Signer{newSigner(t)}
	}
	clientKey, clientSigner := newKey(t)
	srv := &sftpTestServer{hostKeys: hostKeys, clientKey: clientKey}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == "u" && string(pw) == "p" {
				return nil, nil
			}
			return nil, errors.New("wrong password")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(clientSigner.PublicKey().Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	for _, k := range hostKeys {
		cfg.AddHostKey(k)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	srv.addr = l.Addr().String()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go serveSSH(c, cfg)
		}
	}()
	return srv
}

func serveSSH(c net.Conn, cfg *ssh.ServerConfig) {
	defer func() { _ = c.Close() }()
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			for r := range chReqs {
				ok := r.Type == "subsystem" && len(r.Payload) > 4 && string(r.Payload[4:]) == "sftp"
				_ = r.Reply(ok, nil)
				if ok {
					go serveSFTP(ch)
				}
			}
		}()
	}
}

func serveSFTP(ch ssh.Channel) {
	defer func() { _ = ch.Close() }()
	s, err := sftp.NewServer(ch)
	if err != nil {
		return
	}
	_ = s.Serve()
}

// knownHosts writes a known_hosts file pinning keys for the server.
func (s *sftpTestServer) knownHosts(t *testing.T, keys ...ssh.PublicKey) string {
	t.Helper()
	var lines []string
	for _, k := range keys {
		lines = append(lines, knownhosts.Line([]string{knownhosts.Normalize(s.addr)}, k))
	}
	p := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// destination is an sftp destination onto root on the server, logging in
// with password "p" and pinning the server's first host key.
func (s *sftpTestServer) destination(t *testing.T, root string) *config.Destination {
	t.Helper()
	host, port, _ := net.SplitHostPort(s.addr)
	return &config.Destination{Name: "box", Type: "sftp", Root: root, Layout: config.LayoutMirror, Params: map[string]string{
		"host": host, "port": port, "user": "u", "password": "p",
		"known_hosts_file": s.knownHosts(t, s.hostKeys[0].PublicKey()),
	}}
}

func mustDialSFTP(t *testing.T, dest *config.Destination) *sftpTransport {
	t.Helper()
	tr, err := dialSFTP(context.Background(), dest)
	if err != nil {
		t.Fatalf("dialSFTP: %v", err)
	}
	return tr
}

func plantSymlink(t *testing.T, dir string) func(target, name string) {
	return func(target, name string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
}

func TestSFTPTransportContract(t *testing.T) {
	srv := startSFTPServer(t)
	runTransportContract(t, func(t *testing.T) (transport, func(target, name string)) {
		dir := t.TempDir()
		return mustDialSFTP(t, srv.destination(t, dir)), plantSymlink(t, dir)
	})
}

// TestSFTPServerRenameReplaces pins why the transport checks a rename's
// target itself: the in-process server's rename replaces an existing
// name, as some real servers do.
func TestSFTPServerRenameReplaces(t *testing.T) {
	srv := startSFTPServer(t)
	dir := t.TempDir()
	tr := mustDialSFTP(t, srv.destination(t, dir))
	t.Cleanup(func() { _ = tr.Close() })
	mustPut(t, tr, "x", "from")
	mustPut(t, tr, "y", "to")
	if err := tr.client.Rename(tr.full("x"), tr.full("y")); err != nil {
		t.Skipf("this server refuses the replacing rename itself: %v", err)
	}
	if got := mustRead(t, tr, "y"); got != "from" {
		t.Fatalf("y after the raw rename = %q, want from", got)
	}
}

func TestSFTPRefusesUnknownHostKey(t *testing.T) {
	srv := startSFTPServer(t)
	dest := srv.destination(t, t.TempDir())
	dest.Params["known_hosts_file"] = srv.knownHosts(t)
	_, err := dialSFTP(context.Background(), dest)
	if !errors.Is(err, errUnknownHostKey) {
		t.Fatalf("dial with an empty known_hosts = %v, want errUnknownHostKey", err)
	}
	want := knownhosts.Line([]string{knownhosts.Normalize(srv.addr)}, srv.hostKeys[0].PublicKey())
	for _, s := range []string{ssh.FingerprintSHA256(srv.hostKeys[0].PublicKey()), want} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("refusal lacks %q:\n%v", s, err)
		}
	}

	dest.Params["known_hosts_file"] = filepath.Join(t.TempDir(), "absent")
	if _, err := dialSFTP(context.Background(), dest); !errors.Is(err, errUnknownHostKey) {
		t.Fatalf("dial with no known_hosts file = %v, want errUnknownHostKey", err)
	}
}

func TestSFTPRefusesChangedHostKey(t *testing.T) {
	srv := startSFTPServer(t)
	dest := srv.destination(t, t.TempDir())
	dest.Params["known_hosts_file"] = srv.knownHosts(t, newSigner(t).PublicKey())
	if _, err := dialSFTP(context.Background(), dest); !errors.Is(err, errHostKeyChanged) {
		t.Fatalf("dial with another pinned key = %v, want errHostKeyChanged", err)
	}
}

// TestSFTPNegotiatesThePinnedHostKeyType: a server offering an ed25519 and
// an RSA key, pinned only by its RSA key, is still accepted — the
// handshake asks for the key type known_hosts holds.
func TestSFTPNegotiatesThePinnedHostKeyType(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaSigner, err := ssh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := startSFTPServer(t, newSigner(t), rsaSigner)
	dest := srv.destination(t, t.TempDir())
	dest.Params["known_hosts_file"] = srv.knownHosts(t, rsaSigner.PublicKey())
	tr := mustDialSFTP(t, dest)
	_ = tr.Close()
}

func TestSFTPAuthenticatesByKeyFile(t *testing.T) {
	srv := startSFTPServer(t)
	dest := srv.destination(t, t.TempDir())
	delete(dest.Params, "password")
	block, err := ssh.MarshalPrivateKey(srv.clientKey, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	dest.Params["key_file"] = keyFile
	tr := mustDialSFTP(t, dest)
	_ = tr.Close()
}

func TestSFTPAuthenticatesThroughAgent(t *testing.T) {
	srv := startSFTPServer(t)
	dest := srv.destination(t, t.TempDir())
	delete(dest.Params, "password")
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: srv.clientKey}); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(shortTempDir(t), "agent.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _ = agent.ServeAgent(keyring, c); _ = c.Close() }()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", sock)
	tr := mustDialSFTP(t, dest)
	_ = tr.Close()

	t.Setenv("SSH_AUTH_SOCK", "")
	if _, err := dialSFTP(context.Background(), dest); err == nil || !strings.Contains(err.Error(), "SSH_AUTH_SOCK") {
		t.Fatalf("dial with no credentials and no agent = %v, want a refusal naming SSH_AUTH_SOCK", err)
	}
}

// shortTempDir is a temporary directory with a path short enough for a
// unix socket.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestSFTPTransportContractAgainstRcloneServe runs the contract against
// the testbed's server, `rclone serve sftp`. That server hides symlinks,
// so the symlink cases skip there.
func TestSFTPTransportContractAgainstRcloneServe(t *testing.T) {
	rclone, err := exec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone not on PATH")
	}
	hostPriv, hostKey := newKey(t)
	keyFile := filepath.Join(t.TempDir(), "host_key")
	block, err := ssh.MarshalPrivateKey(hostPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	runTransportContract(t, func(t *testing.T) (transport, func(target, name string)) {
		dir := t.TempDir()
		srv := startRcloneServe(t, rclone, dir, keyFile, hostKey)
		dest := srv.destination(t, "/")
		return mustDialSFTP(t, dest), func(string, string) { t.Skip("rclone serve sftp hides symlinks") }
	})
}

// startRcloneServe serves dir with `rclone serve sftp` on a free loopback
// port until the test ends.
func startRcloneServe(t *testing.T, rclone, dir, keyFile string, hostKey ssh.Signer) *sftpTestServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	cmd := exec.Command(rclone, "serve", "sftp", dir, "--addr", addr, "--user", "u", "--pass", "p",
		"--key", keyFile, "--config", filepath.Join(t.TempDir(), "rclone.conf"))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start rclone serve sftp: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for i := 0; ; i++ {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.Close()
			break
		}
		if i == 100 {
			t.Fatalf("rclone serve sftp never listened on %s: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &sftpTestServer{addr: addr, hostKeys: []ssh.Signer{hostKey}}
}

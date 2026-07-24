package agent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/store"
)

// openTestStore builds a fresh on-disk SQLite database in t.TempDir.
// The agent's health endpoint reads schema_version from it, so we need
// a migrated DB even when the test doesn't write file rows.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "index.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	if cfg.Token == "" {
		cfg.Token = "test-token"
	}
	if cfg.Version == "" {
		cfg.Version = "test"
	}
	s, err := New(cfg, openTestStore(t))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return s
}

func TestNewRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"no token", Config{Listen: ":0", Version: "v"}, "Token is required"},
		{"half tls", Config{Listen: ":0", Token: "t", TLSCert: "c", Version: "v"}, "must be set together"},
		{"no version", Config{Listen: ":0", Token: "t"}, "Version is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(c.cfg, openTestStore(t))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected %q in error, got %v", c.want, err)
			}
		})
	}
}

// TestNewListenerLess covers F35: an empty Listen (and no Token) is a
// valid scheduler-only agent, and the server reports no listener/TLS.
func TestNewListenerLess(t *testing.T) {
	srv, err := New(Config{Version: "v"}, openTestStore(t))
	if err != nil {
		t.Fatalf("New (listener-less): %v", err)
	}
	if srv.Addr() != "" {
		t.Fatalf("Addr = %q, want empty for a listener-less agent", srv.Addr())
	}
	if srv.HasTLS() {
		t.Fatal("listener-less agent unexpectedly reports TLS")
	}
}

// TestRunSchedulersReturnsOnCancel pins that the listener-less run path
// blocks until its context is cancelled and then returns cleanly, without
// ever binding a listener.
func TestRunSchedulersReturnsOnCancel(t *testing.T) {
	srv, err := New(Config{Version: "v"}, openTestStore(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.RunSchedulers(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunSchedulers: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunSchedulers did not return after context cancel")
	}
}

// TestNewDefaultsLoggerToDiscard pins the contract that callers may
// leave Config.Logger nil and the agent will still be safe to use:
// New() substitutes a discard logger so the future scheduler can
// always log unconditionally.
func TestNewDefaultsLoggerToDiscard(t *testing.T) {
	srv, err := New(Config{Listen: ":0", Token: "t", Version: "v"}, openTestStore(t))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	if srv.Logger() == nil {
		t.Fatalf("Logger() returned nil; want a usable logger")
	}
	srv.Logger().Info("smoke", "ok", true) // must not panic
}

// TestNewKeepsConfiguredLogger pins the converse: a caller-supplied
// logger is what Server.Logger() returns, so log output reaches the
// destination the CLI (or a test) wired up.
func TestNewKeepsConfiguredLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	srv, err := New(Config{Listen: ":0", Token: "t", Version: "v", Logger: logger}, openTestStore(t))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	srv.Logger().Info("marker", "k", "v")
	if !strings.Contains(buf.String(), `msg=marker`) || !strings.Contains(buf.String(), "k=v") {
		t.Fatalf("logger output = %q, want marker + k=v", buf.String())
	}
}

func TestHealthIsUnauthenticated(t *testing.T) {
	srv := newTestServer(t, Config{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeHealth(t, resp.Body)
	if body.Version != "test" {
		t.Fatalf("version = %q, want test", body.Version)
	}
	if body.SchemaVersion != store.SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", body.SchemaVersion, store.SchemaVersion)
	}
}

func TestSyncBeginRequiresBearerToken(t *testing.T) {
	srv := newTestServer(t, Config{Token: "right-token"})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// /v1/sync/begin with an empty body and a valid bearer token gets
	// past auth and hits the JSON decoder, which rejects with 400.
	// Anything that fails auth must return 401 before reaching the
	// decoder.
	cases := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic Zm9vOmJhcg==", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong-token", http.StatusUnauthorized},
		{"empty token after scheme", "Bearer ", http.StatusUnauthorized},
		{"correct token", "Bearer right-token", http.StatusBadRequest},
		// RFC 7235 §2.1: the scheme name is case-insensitive.
		{"lowercase scheme", "bearer right-token", http.StatusBadRequest},
		{"mixed-case scheme", "BeArEr right-token", http.StatusBadRequest},
		// Tolerant of an extra space between scheme and token.
		{"extra whitespace", "Bearer  right-token", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/sync/begin", nil)
			if c.authHeader != "" {
				req.Header.Set("Authorization", c.authHeader)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != c.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.wantStatus)
			}
		})
	}
}

// TestSyncBeginRejectsBearerWithLengthDifference pins the behaviour that
// shorter/longer-than-configured tokens are rejected. The implementation
// hashes both sides before comparing (so the compare itself sees equal-
// length digests) and this test guards against a regression where a
// future refactor reintroduces a prefix-match or substring-match path.
func TestSyncBeginRejectsBearerWithLengthDifference(t *testing.T) {
	srv := newTestServer(t, Config{Token: "right-token"})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, tok := range []string{"right", "right-token-with-trailing"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/sync/begin", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401", tok, resp.StatusCode)
		}
	}
}

// TestAuthenticatorResolvesCaller (#110a/#110d): a per-peer token
// resolves to its node name; the shared token authenticates with an
// empty identity; an unknown token is rejected. The empty-identity case
// is what keeps the session binding a no-op for shared-token callers.
func TestAuthenticatorResolvesCaller(t *testing.T) {
	auth := newAuthenticator("shared", map[string]string{
		"laptop-token": "laptop",
		"nas-token":    "nas",
	})
	cases := []struct {
		token    string
		wantNode string
		wantOK   bool
	}{
		{"laptop-token", "laptop", true},
		{"nas-token", "nas", true},
		{"shared", "", true},
		{"unknown", "", false},
	}
	for _, c := range cases {
		t.Run(c.token, func(t *testing.T) {
			node, ok := auth.authenticate(c.token)
			if node != c.wantNode || ok != c.wantOK {
				t.Fatalf("authenticate(%q) = (%q, %v), want (%q, %v)", c.token, node, ok, c.wantNode, c.wantOK)
			}
		})
	}
}

// TestAuthenticatorNoPeerTokensSharedOnly: with no per-peer tokens the
// authenticator behaves exactly as the single-shared-token agent did —
// the shared token authenticates (empty identity), everything else is
// rejected.
func TestAuthenticatorNoPeerTokensSharedOnly(t *testing.T) {
	auth := newAuthenticator("only-shared", nil)
	if node, ok := auth.authenticate("only-shared"); !ok || node != "" {
		t.Fatalf("shared token = (%q, %v), want (\"\", true)", node, ok)
	}
	if _, ok := auth.authenticate("nope"); ok {
		t.Fatalf("unknown token authenticated, want rejection")
	}
}

func TestServePlainHTTP(t *testing.T) {
	srv := newTestServer(t, Config{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	url := "http://" + ln.Addr().String() + "/v1/health"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	resp := getWithRetry(t, http.DefaultClient, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve returned: %v", err)
	}
}

func TestServeTLS(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t)
	srv := newTestServer(t, Config{TLSCert: certPath, TLSKey: keyPath})
	if !srv.HasTLS() {
		t.Fatalf("HasTLS = false, want true")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	url := "https://" + ln.Addr().String() + "/v1/health"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	// The cert is self-signed (no trust chain a default client would
	// accept) so we set InsecureSkipVerify on the test client. The cert
	// itself does carry loopback SANs — that's how production trust will
	// work via fingerprint pinning on the initiator side in PR 3.
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	resp := getWithRetry(t, client, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := decodeHealth(t, resp.Body)
	_ = resp.Body.Close()
	if body.SchemaVersion != store.SchemaVersion {
		t.Fatalf("schema_version = %d", body.SchemaVersion)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve returned: %v", err)
	}
}

// getWithRetry tolerates the race between Serve starting the listener
// goroutine and the client's first dial. Without it the TLS test
// occasionally races on slower CI.
func getWithRetry(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	var lastErr error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			return resp
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("GET %s never succeeded: %v", url, lastErr)
	return nil
}

func decodeHealth(t *testing.T, r io.Reader) healthResponse {
	t.Helper()
	var h healthResponse
	if err := json.NewDecoder(r).Decode(&h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return h
}

// writeSelfSignedCert builds a minimal P-256 ECDSA self-signed cert
// valid for localhost / 127.0.0.1 and writes both files to t.TempDir.
// Done in-process so the test doesn't depend on `openssl` or any host
// PKI state.
func writeSelfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "squirrel-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	writePEM(t, certPath, "CERTIFICATE", derBytes)
	writePEM(t, keyPath, "PRIVATE KEY", keyBytes)
	return certPath, keyPath
}

func writePEM(t *testing.T, path, blockType string, bytes []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: bytes}); err != nil {
		t.Fatalf("pem encode: %v", err)
	}
}

// TestValidateRelPathRejectsTraversal pins the receiver-side wire
// path sanitisation. A buggy or hostile initiator must not be able
// to talk the receiver into mv/Upserting outside the volume root, or
// into the receiver's own reserved subtrees.
func TestValidateRelPathRejectsTraversal(t *testing.T) {
	bad := []string{
		"",
		"/etc/passwd",
		`\windows\system32`,
		"..",
		"../etc/passwd",
		"a/../../etc/passwd",
		"a/../..",
		"with\x00null",
		HistoryDirName,
		HistoryDirName + "/run-1/doc.md",
		ConflictsDirName,
		ConflictsDirName + "/run-1/doc.md",
	}
	for _, p := range bad {
		t.Run(p, func(t *testing.T) {
			if err := validateRelPath(p); err == nil {
				t.Fatalf("validateRelPath(%q) accepted; want rejection", p)
			}
		})
	}

	good := []string{
		"a.txt",
		"sub/dir/file",
		"a/b/c.bin",
		"a..b",
		"....",
	}
	for _, p := range good {
		t.Run(p, func(t *testing.T) {
			if err := validateRelPath(p); err != nil {
				t.Fatalf("validateRelPath(%q) rejected: %v", p, err)
			}
		})
	}
}

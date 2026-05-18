package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/store"
)

// syncBuf is a tiny mutex-wrapped bytes.Buffer for capturing the agent
// CLI's stdout/stderr. cobra runs the agent's RunE on a goroutine while
// the test polls the buffer for the startup line, and bytes.Buffer is
// explicitly not safe for concurrent use — direct sharing trips `go
// test -race`.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// reservePort claims a free localhost port via the kernel then releases
// it; we feed the resulting "127.0.0.1:N" string to the agent's `listen`
// config. There is a tiny race between the close here and the agent's
// bind, but it's the standard pattern and is good enough for these
// end-to-end smoke tests.
func reservePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close reserve listener: %v", err)
	}
	return addr
}

// startAgentCLI runs the cobra root with `agent` in a goroutine. It
// returns a cancel function (which terminates the agent and waits for
// graceful shutdown) and the listen address derived from the supplied
// config. The captured stdout/stderr buffer is kept inside the closure
// so failure messages still surface the agent's own output, but
// callers don't need it directly.
func startAgentCLI(t *testing.T, configPath string) (cancel func(), addr string) {
	t.Helper()
	isolateConfig(t)
	ctx, c := context.WithCancel(context.Background())
	buf := &syncBuf{}
	root := newRootCmd()
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"--config", configPath, "agent"})

	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	addr = waitForBanner(t, buf)
	return func() {
		c()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("agent exited with error: %v\noutput:\n%s", err, buf.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("agent did not shut down within timeout\noutput:\n%s", buf.String())
		}
	}, addr
}

func waitForBanner(t *testing.T, buf *syncBuf) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if addr := parseStartupAddr(buf.String()); addr != "" {
			return addr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("agent banner never appeared:\n%s", buf.String())
	return ""
}

// parseStartupAddr extracts the listen address from the slog TextHandler
// line emitted at agent startup. Format:
//
//	time=... level=INFO msg="agent listening" addr=127.0.0.1:54321 scheme=http version=...
//
// We scan line by line for the "agent listening" message rather than
// pattern-matching the whole buffer so unrelated log lines that may
// appear later don't confuse the parser.
func parseStartupAddr(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, `msg="agent listening"`) {
			continue
		}
		for _, field := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(field, "addr="); ok {
				return v
			}
		}
	}
	return ""
}

func writeAgentConfig(t *testing.T, listen, token string) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.db")
	configPath := filepath.Join(dir, "config.toml")
	body := fmt.Sprintf("db = %q\n\n[agent]\nlisten = %q\nauth = { token = %q }\n", dbPath, listen, token)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return configPath
}

func TestCLIAgentHealthEndpoint(t *testing.T) {
	listen := reservePort(t)
	cfgPath := writeAgentConfig(t, listen, "the-token")
	stop, addr := startAgentCLI(t, cfgPath)
	defer stop()

	resp, err := http.Get("http://" + addr + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Version       string `json:"version"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Version == "" {
		t.Fatalf("empty version in health response: %+v", body)
	}
	if body.SchemaVersion <= 0 {
		t.Fatalf("schema_version = %d, want > 0", body.SchemaVersion)
	}
}

func TestCLIAgentSyncRequiresBearer(t *testing.T) {
	listen := reservePort(t)
	cfgPath := writeAgentConfig(t, listen, "secret-token")
	stop, addr := startAgentCLI(t, cfgPath)
	defer stop()

	// No auth → 401.
	resp, err := http.Post("http://"+addr+"/v1/sync/begin", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/sync/begin: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d, want 401", resp.StatusCode)
	}

	// With the correct bearer + an empty body → 400 (request validation
	// past the auth wall).
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/sync/begin", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/sync/begin: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, body)
	}
}

func TestCLIAgentErrorsWhenBlockMissing(t *testing.T) {
	// Config exists but lacks [agent]: the subcommand surfaces the
	// config path so the user knows where to add it.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.db")
	configPath := filepath.Join(dir, "config.toml")
	body := fmt.Sprintf("db = %q\n\n[volumes.x]\npath = %q\n", dbPath, dir)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runCLIExpectErr(t, "--config", configPath, "agent")
	if !strings.Contains(err.Error(), "no [agent] block in") {
		t.Fatalf("expected missing-agent-block error, got %v", err)
	}
}

func TestCLIAgentRequiresConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-config.toml")
	_, err := runCLIExpectErr(t, "--config", missing, "agent")
	if !strings.Contains(err.Error(), "no config at") {
		t.Fatalf("expected missing-config error, got %v", err)
	}
}

// TestCLIAgentSeedsConfiguredNodeName guards openAgentStore plumbing
// `node_name` through to the store on first-DB seed. A fresh agent
// run against a configured DB used to drop the name (calling
// store.Open instead of OpenWithOptions) and seed the self row from
// os.Hostname(), so the receiver advertised the wrong identity on
// /v1/sync/begin.
func TestCLIAgentSeedsConfiguredNodeName(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.db")
	cfgPath := filepath.Join(dir, "config.toml")
	listen := reservePort(t)
	body := fmt.Sprintf(
		"db = %q\nnode_name = %q\n\n[agent]\nlisten = %q\nauth = { token = %q }\n",
		dbPath, "configured-name", listen, "tok")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	stop, _ := startAgentCLI(t, cfgPath)
	// Cancel the CLI so its defer s.Close() runs before we reopen.
	stop()

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	self, err := s.GetSelfNode(context.Background())
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	if self.Name != "configured-name" {
		t.Fatalf("self node name = %q, want %q (os.Hostname leak from store.Open path)",
			self.Name, "configured-name")
	}
}

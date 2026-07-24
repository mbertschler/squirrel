package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/agent"
)

// bearerValues pulls the quoted value from every `bearer = "..."` line in
// the pairing output.
func bearerValues(out string) []string {
	var vals []string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "bearer = "); ok {
			vals = append(vals, strings.Trim(rest, `"`))
		}
	}
	return vals
}

func TestCLINodePairEmitsBothHalves(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("node_name = \"nas\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runCLI(t, "--config", cfgPath, "node", "pair", "laptop")

	for _, want := range []string{
		"# ===== on nas", "# ===== on laptop",
		"[nodes.laptop]", "[nodes.nas]",
		"[agent.auth.peers.laptop]", "[agent.auth.peers.nas]",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("pairing output missing %q:\n%s", want, out)
		}
	}

	// The four bearer bindings must reduce to exactly two distinct tokens,
	// each used twice — the cross-match that kills the F3 error class.
	vals := bearerValues(out)
	if len(vals) != 4 {
		t.Fatalf("expected 4 bearer lines, got %d:\n%s", len(vals), out)
	}
	counts := map[string]int{}
	for _, v := range vals {
		if len(v) < 20 {
			t.Fatalf("bearer token looks too short: %q", v)
		}
		counts[v]++
	}
	if len(counts) != 2 {
		t.Fatalf("expected 2 distinct tokens, got %d: %v", len(counts), counts)
	}
	for tok, n := range counts {
		if n != 2 {
			t.Fatalf("token %q appears %d times, want 2 (cross-match broken)", tok, n)
		}
	}
}

func TestCLINodePairRequiresNodeName(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[volumes.x]\npath = \"/tmp\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runCLIExpectErr(t, "--config", cfgPath, "node", "pair", "laptop")
	if !strings.Contains(err.Error(), "node_name is not set") {
		t.Fatalf("expected node_name error, got %v", err)
	}
}

func TestCLINodePairRejectsSelf(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("node_name = \"nas\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runCLIExpectErr(t, "--config", cfgPath, "node", "pair", "nas")
	if !strings.Contains(err.Error(), "cannot pair with itself") {
		t.Fatalf("expected self-pair error, got %v", err)
	}
}

// TestCLINodePairEmbedsLocalFingerprint checks that when this node already
// has a cert, its real fingerprint is baked into the peer's half rather than
// a placeholder.
func TestCLINodePairEmbedsLocalFingerprint(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "agent.crt")
	keyPath := filepath.Join(dir, "agent.key")
	cfgPath := filepath.Join(dir, "config.toml")
	body := fmt.Sprintf(""+
		"node_name = \"nas\"\n\n"+
		"[agent]\nlisten = \"127.0.0.1:8443\"\nauth = { token = \"tok\" }\n\n"+
		"[agent.tls]\ncert = %q\nkey = %q\n", certPath, keyPath)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	runCLI(t, "--config", cfgPath, "agent", "cert")
	fp, err := agent.FingerprintCertFile(certPath)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	out := runCLI(t, "--config", cfgPath, "node", "pair", "laptop")
	if !strings.Contains(out, fp) {
		t.Fatalf("pairing output missing this node's real fingerprint %q:\n%s", fp, out)
	}
}

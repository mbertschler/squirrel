package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/agent"
)

// writeAgentCertConfig writes a config with an [agent] block whose tls
// cert/key point at paths inside a fresh temp dir, and returns the config
// path plus the two target paths.
func writeAgentCertConfig(t *testing.T) (cfgPath, certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	certPath = filepath.Join(dir, "agent.crt")
	keyPath = filepath.Join(dir, "agent.key")
	cfgPath = filepath.Join(dir, "config.toml")
	body := fmt.Sprintf(""+
		"node_name = \"nas\"\n\n"+
		"[agent]\nlisten = \"127.0.0.1:8443\"\nauth = { token = \"tok\" }\n\n"+
		"[agent.tls]\ncert = %q\nkey = %q\n", certPath, keyPath)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath, certPath, keyPath
}

func TestCLIAgentCertGenerates(t *testing.T) {
	cfgPath, certPath, keyPath := writeAgentCertConfig(t)

	out := runCLI(t, "--config", cfgPath, "agent", "cert")
	for _, p := range []string{certPath, keyPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s to be written: %v", p, err)
		}
	}
	want, err := agent.FingerprintCertFile(certPath)
	if err != nil {
		t.Fatalf("FingerprintCertFile: %v", err)
	}
	if !strings.Contains(out, want) {
		t.Fatalf("output missing generated fingerprint %q:\n%s", want, out)
	}
	if !strings.Contains(out, "[nodes.nas.tls]") || !strings.Contains(out, "cert_fingerprint") {
		t.Fatalf("output missing pasteable pin snippet:\n%s", out)
	}
}

func TestCLIAgentCertRefusesOverwrite(t *testing.T) {
	cfgPath, _, _ := writeAgentCertConfig(t)
	runCLI(t, "--config", cfgPath, "agent", "cert")

	out, err := runCLIExpectErr(t, "--config", cfgPath, "agent", "cert")
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected refuse-overwrite error, got %v\n%s", err, out)
	}
}

func TestCLIAgentCertForceRegenerates(t *testing.T) {
	cfgPath, certPath, _ := writeAgentCertConfig(t)
	runCLI(t, "--config", cfgPath, "agent", "cert")
	first, err := agent.FingerprintCertFile(certPath)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	runCLI(t, "--config", cfgPath, "agent", "cert", "--force")
	second, err := agent.FingerprintCertFile(certPath)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if first == second {
		t.Fatal("--force did not regenerate the certificate (fingerprint unchanged)")
	}
}

func TestCLIAgentCertNoTLSConfigured(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	body := "[agent]\nlisten = \"127.0.0.1:8443\"\nauth = { token = \"tok\" }\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runCLIExpectErr(t, "--config", cfgPath, "agent", "cert")
	if !strings.Contains(err.Error(), "no [agent.tls] cert/key configured") {
		t.Fatalf("expected no-tls error, got %v", err)
	}
}

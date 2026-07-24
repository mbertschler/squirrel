package agent

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// fingerprintShape mirrors config.fingerprintRE so the generated pin is
// guaranteed pasteable into [nodes.X.tls] cert_fingerprint.
var fingerprintShape = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func TestGenerateSelfSignedCert(t *testing.T) {
	c, err := GenerateSelfSignedCert("nas")
	if err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}
	if !fingerprintShape.MatchString(c.Fingerprint) {
		t.Fatalf("fingerprint %q does not match sha256:<64-hex>", c.Fingerprint)
	}
	// cert + key must form a usable TLS pair.
	if _, err := tls.X509KeyPair(c.CertPEM, c.KeyPEM); err != nil {
		t.Fatalf("cert/key do not form a valid pair: %v", err)
	}
	// The fingerprint must equal the SHA-256 over the leaf's DER — exactly
	// what the peer-sync client compares in VerifyConnection.
	block, _ := pem.Decode(c.CertPEM)
	if block == nil {
		t.Fatal("no PEM block in CertPEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	sum := sha256.Sum256(leaf.Raw)
	if want := "sha256:" + hex.EncodeToString(sum[:]); want != c.Fingerprint {
		t.Fatalf("fingerprint = %q, want %q", c.Fingerprint, want)
	}
	if leaf.Subject.CommonName != "nas" {
		t.Fatalf("CommonName = %q, want nas", leaf.Subject.CommonName)
	}
}

func TestGenerateSelfSignedCertUnique(t *testing.T) {
	a, _ := GenerateSelfSignedCert("nas")
	b, _ := GenerateSelfSignedCert("nas")
	if a.Fingerprint == b.Fingerprint {
		t.Fatal("two generations produced the same fingerprint")
	}
}

func TestFingerprintCertFile(t *testing.T) {
	c, err := GenerateSelfSignedCert("laptop")
	if err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}
	path := filepath.Join(t.TempDir(), "agent.crt")
	if err := os.WriteFile(path, c.CertPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	got, err := FingerprintCertFile(path)
	if err != nil {
		t.Fatalf("FingerprintCertFile: %v", err)
	}
	if got != c.Fingerprint {
		t.Fatalf("FingerprintCertFile = %q, want %q", got, c.Fingerprint)
	}
}

func TestFingerprintCertFileMissing(t *testing.T) {
	if _, err := FingerprintCertFile(filepath.Join(t.TempDir(), "nope.crt")); err == nil {
		t.Fatal("expected error for missing cert file")
	}
}

// TestServerCertFingerprint covers the accessor logAgentStartup uses to log
// the pin at startup: it returns the configured cert's fingerprint and
// errors for a plain-HTTP agent.
func TestServerCertFingerprint(t *testing.T) {
	c, err := GenerateSelfSignedCert("nas")
	if err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}
	path := filepath.Join(t.TempDir(), "agent.crt")
	if err := os.WriteFile(path, c.CertPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	tlsSrv := &Server{cfg: Config{TLSCert: path}}
	got, err := tlsSrv.CertFingerprint()
	if err != nil {
		t.Fatalf("CertFingerprint: %v", err)
	}
	if got != c.Fingerprint {
		t.Fatalf("CertFingerprint = %q, want %q", got, c.Fingerprint)
	}
	if _, err := (&Server{cfg: Config{}}).CertFingerprint(); err == nil {
		t.Fatal("expected error for a plain-HTTP agent with no cert")
	}
}

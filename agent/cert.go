package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// SelfSignedCert bundles a freshly generated self-signed certificate, its
// private key (both PEM-encoded), and the sha256: fingerprint pin peers
// paste into their [nodes.X.tls] cert_fingerprint.
type SelfSignedCert struct {
	CertPEM     []byte
	KeyPEM      []byte
	Fingerprint string
}

// certValidity is how long a generated agent certificate stays valid.
// Generous because these certs are pinned by fingerprint, not chained to a
// CA: a peer that pins the fingerprint trusts it regardless of expiry, and
// regenerating would change the fingerprint and force every peer to re-pin.
const certValidity = 10 * 365 * 24 * time.Hour

// GenerateSelfSignedCert mints an ECDSA P-256 self-signed certificate for a
// squirrel agent. nodeName becomes the subject common name and a DNS SAN;
// localhost and the loopback addresses are added so a locally-dialed agent
// still validates for clients that do check names. The peer-sync client
// pins the fingerprint and skips name checks, but nothing should depend on
// that. The returned Fingerprint is the sha256: pin over the certificate's
// DER bytes — the exact value the peer-sync verifier compares.
func GenerateSelfSignedCert(nodeName string) (SelfSignedCert, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return SelfSignedCert{}, fmt.Errorf("generate key: %w", err)
	}
	tmpl, err := certTemplate(nodeName)
	if err != nil {
		return SelfSignedCert{}, err
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return SelfSignedCert{}, fmt.Errorf("create certificate: %w", err)
	}
	keyPEM, err := encodeKeyPEM(key)
	if err != nil {
		return SelfSignedCert{}, err
	}
	return SelfSignedCert{
		CertPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:      keyPEM,
		Fingerprint: fingerprintDER(der),
	}, nil
}

// certTemplate builds the x509 template for a self-signed agent cert.
func certTemplate(nodeName string) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	cn := nodeName
	if cn == "" {
		cn = "squirrel-agent"
	}
	dns := []string{"localhost"}
	if nodeName != "" {
		dns = append([]string{nodeName}, dns...)
	}
	now := time.Now()
	return &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}, nil
}

func encodeKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// fingerprintDER renders the sha256: pin over a certificate's DER bytes,
// matching the peer-sync client's VerifyConnection comparison.
func fingerprintDER(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// FingerprintCertFile reads a PEM certificate file and returns its sha256:
// pin — the value the agent logs at startup and peers put in
// [nodes.X.tls] cert_fingerprint. The first CERTIFICATE block (the leaf)
// wins; trailing chain blocks are ignored.
func FingerprintCertFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read certificate %s: %w", path, err)
	}
	for {
		block, rest := pem.Decode(data)
		if block == nil {
			return "", fmt.Errorf("no CERTIFICATE block in %s", path)
		}
		if block.Type == "CERTIFICATE" {
			return fingerprintDER(block.Bytes), nil
		}
		data = rest
	}
}

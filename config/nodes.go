package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Node is a resolved `[nodes.X]` block — a peer destination running a
// squirrel agent. The relationship is symmetric: either side can
// initiate a sync of a shared volume against the other's HTTP API.
//
// Endpoint is the parsed URL the initiator dials for the sync API
// (https://nas.local:8443, http://...). Token is the resolved bearer
// literal. CertFingerprint is an optional sha256:hex pin for the
// receiver's TLS certificate — when set, the initiator verifies the
// presented cert's SHA-256 fingerprint against this value before
// trusting the connection (self-signed certs are normal for LAN
// agents; pinning is the trust anchor in lieu of a CA chain). Path
// is the rclone-URI prefix the initiator copies bytes into, joined
// with `/<volume>/<file-path>` per transfer; it lives in the node
// block (not derived from the endpoint) because the HTTP API and the
// rclone-reachable filesystem are independent concerns.
type Node struct {
	Name            string
	Endpoint        *url.URL
	Token           string
	CertFingerprint string
	Path            string
}

// rawNode mirrors the `[nodes.X]` TOML block. Token is `any` so the
// same resolveSecret path that handles destination credentials handles
// it transparently — accepting either a literal string or
// `{ env = "VAR" }`.
type rawNode struct {
	Endpoint string       `toml:"endpoint"`
	Path     string       `toml:"path"`
	Auth     *rawNodeAuth `toml:"auth"`
	TLS      *rawNodeTLS  `toml:"tls"`
}

type rawNodeAuth struct {
	Bearer any `toml:"bearer"`
}

type rawNodeTLS struct {
	CertFingerprint string `toml:"cert_fingerprint"`
}

// fingerprintRE pins the cert_fingerprint shape to `sha256:<hex>` so
// the initiator's TLS verifier can decode it without sniffing. The
// hex part is exactly 64 characters (the length of a SHA-256 digest);
// any other shape is a misconfiguration we'd rather reject at load
// time than at first sync.
var fingerprintRE = regexp.MustCompile(`^sha256:[a-fA-F0-9]{64}$`)

func resolveNode(name string, r *rawNode) (*Node, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("invalid node name (must match %s)", nameRE)
	}
	if r.Endpoint == "" {
		return nil, errors.New("endpoint is required")
	}
	u, err := url.Parse(r.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("endpoint: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("endpoint: scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("endpoint: host is required")
	}
	if r.Path == "" {
		return nil, errors.New("path is required (rclone target prefix the initiator copies bytes into)")
	}
	if r.Auth == nil || r.Auth.Bearer == nil {
		return nil, errors.New("auth.bearer is required")
	}
	tok, err := resolveSecret(map[string]any{"bearer": r.Auth.Bearer}, "bearer")
	if err != nil {
		return nil, fmt.Errorf("auth.%w", err)
	}
	if tok == "" {
		return nil, errors.New("auth.bearer must not be empty")
	}
	node := &Node{
		Name:     name,
		Endpoint: u,
		Token:    tok,
		Path:     r.Path,
	}
	if r.TLS != nil && r.TLS.CertFingerprint != "" {
		fp := strings.ToLower(r.TLS.CertFingerprint)
		if !fingerprintRE.MatchString(fp) {
			return nil, fmt.Errorf("tls.cert_fingerprint %q is not of the form sha256:<64-hex>", r.TLS.CertFingerprint)
		}
		node.CertFingerprint = fp
	}
	return node, nil
}

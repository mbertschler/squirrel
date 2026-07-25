package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mbertschler/squirrel/syncproto"
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
	// DedupStrategy is the initiator's preference for receiver-side
	// content-addressable dedup when syncing to this peer. Resolved
	// values are "copy" (default, lets the receiver materialise a
	// missing path by copying an existing same-blake3 file in the same
	// volume) or "off" (always Transfer). The literal travels in the
	// /v1/sync/begin payload; the receiver validates and applies it.
	DedupStrategy string
	// PullDurabilityEvery is the agent-scheduler cadence for pulling this
	// peer's destination durability vectors — the same metadata-only merge
	// that `squirrel peer-sync pull-durability` runs and that rides along
	// after a successful node sync. Giving evidence freshness its own clock
	// lets a receive-only node (one this machine never initiates a sync to)
	// keep its offload-gate evidence fresh with zero typed commands. Zero
	// means "no scheduled pull". The pull never rewinds a watermark — the
	// agent does not escalate.
	PullDurabilityEvery time.Duration
}

// rawNode mirrors the `[nodes.X]` TOML block. Token is `any` so the
// same resolveSecret path that handles destination credentials handles
// it transparently — accepting either a literal string or
// `{ env = "VAR" }`.
type rawNode struct {
	Endpoint            string       `toml:"endpoint"`
	Path                string       `toml:"path"`
	DedupStrategy       string       `toml:"dedup_strategy"`
	PullDurabilityEvery string       `toml:"pull_durability_every"`
	Auth                *rawNodeAuth `toml:"auth"`
	TLS                 *rawNodeTLS  `toml:"tls"`
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

// resolveDedupStrategy maps the raw TOML value (which may be the empty
// string when the user omitted the key) to the canonical syncproto
// constant. Unknown values are rejected at config-load time so a typo
// surfaces before the first sync rather than as a wire-level 400.
func resolveDedupStrategy(raw string) (string, error) {
	switch raw {
	case "", syncproto.DedupStrategyCopy:
		return syncproto.DedupStrategyCopy, nil
	case syncproto.DedupStrategyOff:
		return syncproto.DedupStrategyOff, nil
	}
	return "", fmt.Errorf("dedup_strategy %q is invalid (allowed: %q, %q)",
		raw, syncproto.DedupStrategyCopy, syncproto.DedupStrategyOff)
}

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
	strategy, err := resolveDedupStrategy(r.DedupStrategy)
	if err != nil {
		return nil, err
	}
	var pullEvery time.Duration
	if r.PullDurabilityEvery != "" {
		pullEvery, err = parseVolumeCadence("pull_durability_every", r.PullDurabilityEvery)
		if err != nil {
			return nil, err
		}
	}
	node := &Node{
		Name:                name,
		Endpoint:            u,
		Token:               tok,
		Path:                r.Path,
		DedupStrategy:       strategy,
		PullDurabilityEvery: pullEvery,
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

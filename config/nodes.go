package config

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
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
	// Path is required only of a node some volume actually syncs to —
	// the one relationship that moves bytes through it. A node this
	// machine only pulls durability evidence from carries no bytes, so
	// demanding a byte-path for it would make the operator invent one to
	// satisfy a validator, teaching them the field does not matter about
	// a field that silently eats bytes when it is wrong (F34). Empty
	// therefore means "no bytes traverse this relationship", and
	// resolve() rejects an empty Path only for a node named in some
	// volume's sync_to.
	Path string
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

// validateNodeBytePaths rejects a node that receives bytes but has no
// byte-path to receive them into. `path` is optional on the node block
// itself (F34) because whether bytes traverse a relationship is a property
// of the volumes, not of the node — so the requirement is enforced exactly
// where it is true: a node named in some volume's sync_to. Volumes are
// walked in name order so a config with several holes reports the same one
// first on every load.
func validateNodeBytePaths(cfg *Config) error {
	for _, vname := range slices.Sorted(maps.Keys(cfg.Volumes)) {
		for _, target := range cfg.Volumes[vname].SyncTo {
			node, ok := cfg.Nodes[target]
			if !ok || node.Path != "" {
				continue
			}
			return fmt.Errorf("nodes.%s: path is required because volumes.%s syncs to it "+
				"(rclone target prefix the initiator copies bytes into)", target, vname)
		}
	}
	return nil
}

// BytePathState classifies what squirrel can currently say about a node's
// byte-path. It exists so the one reader below can serve both `config
// check` and the status surfaces without either re-implementing the
// stat rules — and so neither has to guess what an empty Path means.
type BytePathState int

const (
	// BytePathOK: the path resolves to a directory on this machine.
	BytePathOK BytePathState = iota
	// BytePathNone: no byte-path is configured, and none is needed — no
	// volume syncs to this node, so nothing ever copies bytes into it.
	BytePathNone
	// BytePathRemote: an rclone remote spec (`remote:path`). Reaching it
	// means asking rclone, which is a transfer-time concern, not something
	// a local read-only check can answer.
	BytePathRemote
	// BytePathUnavailable: the path is a local one that does not currently
	// resolve to a directory. Amber, not red: the commonest cause is a
	// mount that is not up yet, which resolves on its own.
	BytePathUnavailable
)

// CheckBytePath reports what can be determined about this node's
// byte-path right now, with a short human reason for the states that carry
// one. It is a point-in-time read of the filesystem, deliberately: a
// network mount can come and go under a running agent, so no caller may
// cache the answer.
//
// Both `squirrel config check` and the status build call this, which is
// what keeps one set of rules about what a byte-path may look like — an
// absolute directory, an rclone remote spec, or legitimately absent.
func (n *Node) CheckBytePath() (BytePathState, string) {
	if n.Path == "" {
		return BytePathNone, "no bytes are synced to this node"
	}
	if isRcloneRemoteSpec(n.Path) {
		return BytePathRemote, "rclone remote spec — not checked"
	}
	info, err := os.Stat(n.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return BytePathUnavailable, "does not exist — mount not up?"
	case err != nil:
		return BytePathUnavailable, err.Error()
	case !info.IsDir():
		return BytePathUnavailable, "not a directory"
	}
	return BytePathOK, ""
}

// isRcloneRemoteSpec reports whether p is an rclone "remote:path" reference
// rather than a filesystem path. An absolute path (leading /) is always a
// filesystem path; otherwise a leading "name:" segment marks a remote.
func isRcloneRemoteSpec(p string) bool {
	if strings.HasPrefix(p, "/") {
		return false
	}
	i := strings.IndexByte(p, ':')
	return i > 0
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
	// Path is deliberately not required here: whether this node ever
	// receives bytes is a property of the volumes, not of the node block,
	// so the check belongs to resolve() once sync_to is known.
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

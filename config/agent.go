package config

import (
	"errors"
	"fmt"
	"time"
)

// Scan strategy constants for Agent.ScanStrategy.
const (
	ScanStrategyShallow = "shallow"
	ScanStrategyDeep    = "deep"
)

// Agent is the resolved `[agent]` block. It is nil on Config when the
// block is absent; the agent subcommand errors with a config-pointer if
// callers try to start without it.
type Agent struct {
	// Listen is the TCP address the agent binds to, e.g. "0.0.0.0:8443".
	// Required.
	Listen string
	// DB optionally overrides the top-level db for the agent process. The
	// agent binary resolves --db > Agent.DB > top-level db > default; an
	// empty value here means "use the top-level / default".
	DB string
	// TLSCert and TLSKey are absolute filesystem paths to a PEM-encoded
	// certificate and matching private key. Both must be set together;
	// empty disables TLS (plain HTTP).
	TLSCert string
	TLSKey  string
	// Token is the resolved bearer token literal. Required: an agent
	// without a token is an unauthenticated open port and we refuse to
	// start one.
	Token string
	// PeerTokens maps a per-peer bearer token to the node name that
	// presents it, so the agent can recover an authenticated caller
	// identity instead of treating every token-holder as anonymous. When
	// non-empty the agent binds each in-flight sync session to the node
	// whose token opened it and rejects phase calls from a different
	// identity (#110a). Empty (the default) keeps the single shared Token
	// as the only credential, authenticating every caller identically.
	PeerTokens map[string]string
	// ScanInterval is the period between drift-detection passes the
	// agent runs over its hosted volumes (#17). Zero (the default,
	// when the TOML key is absent) disables the scheduler — the
	// agent then only re-hashes during peer syncs.
	ScanInterval time.Duration
	// ScanStrategy picks the per-tick rehash policy:
	// ScanStrategyShallow (default) skips files whose (size, mtime)
	// match the stored row; ScanStrategyDeep re-hashes everything
	// unconditionally — the bit-rot-detection schedule.
	ScanStrategy string
}

// rawAgent mirrors the `[agent]` TOML block. We use typed sub-structs
// (rather than map[string]any) so DisallowUnknownFields catches typos like
// `cret` or `lisetn` at load time instead of silently dropping them.
type rawAgent struct {
	Listen       string   `toml:"listen"`
	DB           string   `toml:"db"`
	TLS          *rawTLS  `toml:"tls"`
	Auth         *rawAuth `toml:"auth"`
	ScanInterval string   `toml:"scan_interval"`
	ScanStrategy string   `toml:"scan_strategy"`
}

type rawTLS struct {
	Cert string `toml:"cert"`
	Key  string `toml:"key"`
}

// rawAuth keeps Token as `any` because the resolved value is either a
// plain string or an inline `{ env = "VAR" }` table — same shape we use
// for destination secrets. resolveSecret normalises both. Peers is the
// optional per-peer token map keyed by node name (`[agent.auth.peers.X]`),
// each carrying its own `bearer` secret in the same shape `[nodes.X]` uses.
type rawAuth struct {
	Token any                     `toml:"token"`
	Peers map[string]*rawPeerAuth `toml:"peers"`
}

type rawPeerAuth struct {
	Bearer any `toml:"bearer"`
}

func resolveAgent(r *rawAgent) (*Agent, error) {
	if r.Listen == "" {
		return nil, errors.New("listen is required")
	}
	a := &Agent{Listen: r.Listen}
	if r.DB != "" {
		expanded, err := expandPath(r.DB)
		if err != nil {
			return nil, fmt.Errorf("db: %w", err)
		}
		a.DB = expanded
	}
	if err := resolveAgentTLS(r.TLS, a); err != nil {
		return nil, err
	}
	if err := resolveAgentAuth(r.Auth, a); err != nil {
		return nil, err
	}
	if err := resolveAgentScan(r, a); err != nil {
		return nil, err
	}
	return a, nil
}

// resolveAgentScan parses the drift-detection scheduler knobs. Both
// are optional; absent means "no scheduled audits". A scan_strategy
// without a scan_interval is accepted as effectively dead config (the
// strategy is ignored when interval is zero) — emitting an error
// would force users who toggle scheduling off to also delete the
// strategy line.
func resolveAgentScan(r *rawAgent, a *Agent) error {
	if r.ScanInterval != "" {
		dur, err := time.ParseDuration(r.ScanInterval)
		if err != nil {
			return fmt.Errorf("scan_interval %q: %w", r.ScanInterval, err)
		}
		if dur < 0 {
			return fmt.Errorf("scan_interval must not be negative, got %s", dur)
		}
		a.ScanInterval = dur
	}
	switch r.ScanStrategy {
	case "":
		a.ScanStrategy = ScanStrategyShallow
	case ScanStrategyShallow, ScanStrategyDeep:
		a.ScanStrategy = r.ScanStrategy
	default:
		return fmt.Errorf("scan_strategy %q is invalid (want %q or %q)",
			r.ScanStrategy, ScanStrategyShallow, ScanStrategyDeep)
	}
	return nil
}

func resolveAgentTLS(r *rawTLS, a *Agent) error {
	if r == nil {
		return nil
	}
	if (r.Cert == "") != (r.Key == "") {
		return errors.New("tls.cert and tls.key must be set together")
	}
	if r.Cert == "" {
		// Empty `tls = { }` is treated the same as no TLS block — plain HTTP.
		return nil
	}
	cert, err := expandPath(r.Cert)
	if err != nil {
		return fmt.Errorf("tls.cert: %w", err)
	}
	key, err := expandPath(r.Key)
	if err != nil {
		return fmt.Errorf("tls.key: %w", err)
	}
	a.TLSCert = cert
	a.TLSKey = key
	return nil
}

func resolveAgentAuth(r *rawAuth, a *Agent) error {
	if r == nil || r.Token == nil {
		return errors.New("auth.token is required (no agent without authentication)")
	}
	// resolveSecret takes a map[string]any and pulls the named key. We pass
	// a synthetic single-entry map so the same code handles plain strings
	// and `{ env = "VAR" }` tables for the agent token just like it does
	// for destination credentials.
	tok, err := resolveSecret(map[string]any{"token": r.Token}, "token")
	if err != nil {
		return fmt.Errorf("auth.%w", err)
	}
	if tok == "" {
		return errors.New("auth.token must not be empty")
	}
	a.Token = tok
	return resolveAgentPeerTokens(r.Peers, a)
}

// resolveAgentPeerTokens resolves the optional `[agent.auth.peers.X]`
// per-peer token map into Agent.PeerTokens (token → node name). A token
// that maps to two identities can't authenticate either, so every peer
// token must be distinct and must differ from the shared auth.token;
// both collisions are rejected at load time rather than silently
// shadowing one peer. Absent or empty leaves PeerTokens nil — the agent
// then authenticates every caller with the shared token alone.
func resolveAgentPeerTokens(peers map[string]*rawPeerAuth, a *Agent) error {
	if len(peers) == 0 {
		return nil
	}
	resolved := make(map[string]string, len(peers))
	owners := map[string]string{a.Token: "auth.token"}
	for name, peer := range peers {
		if !nameRE.MatchString(name) {
			return fmt.Errorf("auth.peers: invalid node name %q (must match %s)", name, nameRE)
		}
		if peer == nil || peer.Bearer == nil {
			return fmt.Errorf("auth.peers.%s.bearer is required", name)
		}
		tok, err := resolveSecret(map[string]any{"bearer": peer.Bearer}, "bearer")
		if err != nil {
			return fmt.Errorf("auth.peers.%s.%w", name, err)
		}
		if tok == "" {
			return fmt.Errorf("auth.peers.%s.bearer must not be empty", name)
		}
		if owner, dup := owners[tok]; dup {
			return fmt.Errorf("auth.peers.%s.bearer reuses the token already bound to %s; each credential must map to one identity", name, owner)
		}
		owners[tok] = "auth.peers." + name
		resolved[tok] = name
	}
	a.PeerTokens = resolved
	return nil
}

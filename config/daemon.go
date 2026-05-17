package config

import (
	"errors"
	"fmt"
)

// Daemon is the resolved `[daemon]` block. It is nil on Config when the
// block is absent; the daemon subcommand errors with a config-pointer if
// callers try to start without it.
type Daemon struct {
	// Listen is the TCP address the daemon binds to, e.g. "0.0.0.0:8443".
	// Required.
	Listen string
	// DB optionally overrides the top-level db for the daemon process. The
	// daemon binary resolves --db > Daemon.DB > top-level db > default; an
	// empty value here means "use the top-level / default".
	DB string
	// TLSCert and TLSKey are absolute filesystem paths to a PEM-encoded
	// certificate and matching private key. Both must be set together;
	// empty disables TLS (plain HTTP).
	TLSCert string
	TLSKey  string
	// Token is the resolved bearer token literal. Required: a daemon
	// without a token is an unauthenticated open port and we refuse to
	// start one.
	Token string
}

// rawDaemon mirrors the `[daemon]` TOML block. We use typed sub-structs
// (rather than map[string]any) so DisallowUnknownFields catches typos like
// `cret` or `lisetn` at load time instead of silently dropping them.
type rawDaemon struct {
	Listen string   `toml:"listen"`
	DB     string   `toml:"db"`
	TLS    *rawTLS  `toml:"tls"`
	Auth   *rawAuth `toml:"auth"`
}

type rawTLS struct {
	Cert string `toml:"cert"`
	Key  string `toml:"key"`
}

// rawAuth keeps Token as `any` because the resolved value is either a
// plain string or an inline `{ env = "VAR" }` table — same shape we use
// for destination secrets. resolveSecret normalises both.
type rawAuth struct {
	Token any `toml:"token"`
}

func resolveDaemon(r *rawDaemon) (*Daemon, error) {
	if r.Listen == "" {
		return nil, errors.New("listen is required")
	}
	d := &Daemon{Listen: r.Listen}
	if r.DB != "" {
		expanded, err := expandPath(r.DB)
		if err != nil {
			return nil, fmt.Errorf("db: %w", err)
		}
		d.DB = expanded
	}
	if err := resolveDaemonTLS(r.TLS, d); err != nil {
		return nil, err
	}
	if err := resolveDaemonAuth(r.Auth, d); err != nil {
		return nil, err
	}
	return d, nil
}

func resolveDaemonTLS(r *rawTLS, d *Daemon) error {
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
	d.TLSCert = cert
	d.TLSKey = key
	return nil
}

func resolveDaemonAuth(r *rawAuth, d *Daemon) error {
	if r == nil || r.Token == nil {
		return errors.New("auth.token is required (no daemon without authentication)")
	}
	// resolveSecret takes a map[string]any and pulls the named key. We pass
	// a synthetic single-entry map so the same code handles plain strings
	// and `{ env = "VAR" }` tables for the daemon token just like it does
	// for destination credentials.
	tok, err := resolveSecret(map[string]any{"token": r.Token}, "token")
	if err != nil {
		return fmt.Errorf("auth.%w", err)
	}
	if tok == "" {
		return errors.New("auth.token must not be empty")
	}
	d.Token = tok
	return nil
}

// Package agent implements the squirrel agent: the long-running
// process that hosts the peer-sync HTTP server and the two background
// schedulers.
//
// The HTTP surface terminates bearer-token auth and (optionally) TLS,
// serves the /v1/health endpoint, and handles the four /v1/sync/*
// peer-sync routes (begin, plan, verify, close). The drift-detection
// scan loop walks every configured volume on ScanInterval and writes
// an `audit`-kind run; the cadence scheduler (#39) fires automatic
// index and sync runs on the per-volume sync_every / index_every
// knobs. Both loops share the per-volume lock with the sync routes so
// no two operations on the same volume overlap.
package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// Scan strategy values for Config.ScanStrategy. Shallow uses the
// (size, mtime) shortcut on the existing index row; Deep always
// rehashes every file (the bit-rot-detection use case).
const (
	ScanStrategyShallow = "shallow"
	ScanStrategyDeep    = "deep"
)

// SyncRunner is the function shape the cadence scheduler delegates a
// (volume, destination) sync to. Returning Err != nil with RunID == 0
// means the sync was refused before a runs row was inserted (e.g.
// destination not in config); a non-zero RunID with Err != nil means
// the run was created but failed mid-flight. Status is one of
// store.RunStatus*; empty falls back to "failed" in the scheduler's
// own log line.
type SyncRunner func(ctx context.Context, vol *config.Volume, destName string) SyncRunReport

// SyncRunReport is the SyncRunner result surfaced to the scheduler.
// Kept minimal — anything richer belongs in the runs table the CLI
// inspects via `squirrel runs`, not on the agent's hot path. Conflicts
// and Contested carry the peer-sync divergence counts so the scheduler
// logs a conflict signal on the initiating machine, not only the hub
// (#158, F27): Conflicts is how many paths this run resolved by
// preserving the loser and landing this node's bytes live, Contested how
// many the receiver refused because a prior freeze still stands.
type SyncRunReport struct {
	RunID     int64
	Status    string
	Err       error
	Conflicts int
	Contested int
}

// VerifyRunner is the function shape the scheduler delegates one
// destination's verify pass to (F32). It runs the same re-check as
// `squirrel verify <destination>` and records a kind='audit' run, so the
// agent package needs no edge to the sync package. A zero RunID with a nil
// Err means the destination had nothing recorded to verify (a no-op, no run
// row written); a non-zero RunID carries the audit run's outcome. Status is
// one of store.RunStatus*.
type VerifyRunner func(ctx context.Context, destName string) VerifyRunReport

// VerifyRunReport is the VerifyRunner result surfaced to the scheduler's
// kicked/finished/error log.
type VerifyRunReport struct {
	RunID  int64
	Status string
	Err    error
}

// DurabilityPuller is the function shape the scheduler delegates one
// (volume, peer) durability pull to (F33). It runs the same metadata-only
// merge as `squirrel peer-sync pull-durability` — never rewinding a
// watermark, because the agent does not escalate — and records a
// kind='audit' run. Counts ride along for the finished log line.
type DurabilityPuller func(ctx context.Context, vol *config.Volume, peerName string) DurabilityPullReport

// DurabilityPullReport is the DurabilityPuller result surfaced to the
// scheduler's kicked/finished/error log.
type DurabilityPullReport struct {
	RunID   int64
	Status  string
	Err     error
	Fetched int
	Applied int
	Dropped int
	Rewinds int
}

// Config configures one agent. Fields are validated by New: Version is
// always required; Listen is optional (empty selects listener-less,
// scheduler-only mode, F35); Token is required only when Listen is set (an
// HTTP surface needs a bearer token); TLSCert/TLSKey must be set together
// or not at all — empty pair means plain HTTP.
type Config struct {
	// Listen is the bind address passed to net.Listen, e.g. "0.0.0.0:8443".
	Listen string
	// Token is the resolved bearer token compared (in constant time)
	// against the Authorization header on every authenticated request.
	Token string
	// PeerTokens optionally maps a per-peer bearer token to the node
	// name that presents it. When non-empty, requireBearer recovers the
	// caller's authenticated node identity and the sync handlers bind
	// each in-flight session to it (#110a); a token absent from the map
	// still authenticates if it equals Token but carries no identity.
	// Empty (the default) preserves the single-shared-token behaviour:
	// every caller authenticates identically and no session binding is
	// enforced.
	PeerTokens map[string]string
	// TLSCert and TLSKey are filesystem paths to a PEM-encoded certificate
	// and matching private key. When both are empty the agent serves
	// plain HTTP; when both are set it terminates TLS natively.
	TLSCert string
	TLSKey  string
	// Version is the agent binary version reported via /v1/health.
	// Required so the field is never an unset zero-value in responses.
	Version string
	// Live is the configuration the agent operates on: its volumes, its
	// destinations, its peer nodes, and the `[agent] verify_every` default.
	// Every consumer takes a whole snapshot from it — the peer-sync
	// endpoints looking up the volume named in /v1/sync/begin, the
	// durability endpoint advertising per-destination offload-gating
	// capability (#145), the scan loop, and the cadence scheduler — so a
	// swap is never half-observable.
	//
	// Setting it is also what enables reload (#204, F9): together with
	// ConfigPath and ConfigDigest, the drift monitor loads the file itself,
	// applies the half of an edit this process can adopt while running, and
	// stores the result here. Leaving it nil gives a fixed, empty
	// configuration — no volumes, no destinations, no nodes — which is what
	// an embedder or a test of the auth/health surface wants.
	Live *config.Live
	// ConfigReloadPrepare, when set alongside Live, is handed each freshly
	// loaded config before the agent swaps it in, to rebuild whatever
	// config-derived state lives outside the agent — the CLI wires it to
	// the rclone lookup and the squirrel-managed rclone.conf. Returning an
	// error abandons that reload entirely: the agent keeps running the
	// configuration it has, and the drift latch stands naming the failure.
	ConfigReloadPrepare func(context.Context, *config.Config) error
	// SyncRunner is the cadence scheduler's (#39) hook for invoking
	// one (volume, destination) sync. The CLI wires this to a closure
	// that calls sync.RunPair against a configured rclone wrapper.
	// Nil disables sync-kicking; index-only cadences still work, and
	// an agent without any cadence-configured volume ignores this
	// field entirely.
	//
	// The indirection keeps the agent package free of an import on
	// the sync package — sync's tests already pull in agent for the
	// peer-sync receiver fixture, and a direct agent→sync edge would
	// close the cycle.
	SyncRunner SyncRunner
	// VerifyRunner is the scheduler's hook for running one destination's
	// verify pass (F32). Nil disables verify-kicking. The CLI wires it to a
	// closure over sync.VerifyRemote; an agent whose config has no
	// verifiable destination with a cadence ignores it.
	VerifyRunner VerifyRunner
	// DurabilityPuller is the scheduler's hook for running one (volume,
	// peer) durability pull (F33). Nil disables pull-kicking. The CLI wires
	// it to a closure over sync.PullDurability.
	DurabilityPuller DurabilityPuller
	// ConfigPath is the config file this agent was started from, and
	// ConfigDigest the content digest config.Load computed for the bytes it
	// parsed (config.Config.Path and .Digest). Both set enables the
	// config-drift monitor (#191, F9): the agent re-hashes the file on a
	// cadence and latches a standing state when it no longer matches what
	// it is running. Leaving either unset disables the monitor, which is
	// what an embedder assembling a Config by hand gets.
	ConfigPath   string
	ConfigDigest []byte
	// ConfigCheckEvery overrides the config-drift re-check period. Zero
	// falls back to DefaultConfigCheckInterval; tests pin it small, and
	// production has no reason to touch it.
	ConfigCheckEvery time.Duration
	// SchedulerTick overrides the scheduler's evaluation period.
	// Zero falls back to DefaultSchedulerTick. Tests pin it to a
	// small value; production rarely needs to touch it.
	SchedulerTick time.Duration
	// Now is the scheduler's time source. Nil falls back to
	// time.Now; tests inject a fake clock so cadence behaviour is
	// deterministic.
	Now func() time.Time
	// ScanInterval is the period between drift-detection passes over
	// every hosted volume (#17). Zero (default) disables the
	// scheduler; the agent then only re-hashes during peer syncs.
	ScanInterval time.Duration
	// ScanStrategy selects the per-tick rehash policy:
	// ScanStrategyShallow (the default; equivalent to today's
	// `--shallow` index) skips files whose (size, mtime) match the
	// stored row, and ScanStrategyDeep re-hashes every file
	// unconditionally (the bit-rot-detection use case). Empty is
	// treated as Shallow.
	ScanStrategy string
	// ScanLogger receives one-line advisories from the
	// drift-detection scheduler (one per volume per tick, plus skip
	// notes on lock contention). Nil discards. The CLI wires this
	// to os.Stderr; tests inject a buffer.
	ScanLogger io.Writer
	// Logger is the structured logger shared with future long-lived
	// components (notably the scheduler in #39). The CLI wires this
	// to a slog.TextHandler over stderr; nil falls back to a discard
	// logger so callers that don't care never have to set it.
	Logger *slog.Logger
}

// Server is one agent instance. It holds the HTTP handler stack and a
// reference to the underlying store for the health endpoint's
// schema_version field; future endpoints (plan, reconcile, ...) will use
// the same handle. The router is kept as a field so the scan scheduler
// (#17) can acquire the same per-volume lock the /v1/sync/* handlers
// use, serialising audit and sync against the same volume.
type Server struct {
	cfg Config
	// live is the configuration snapshot every volume-, destination-, and
	// node-shaped read goes through, so a reload swaps all of them at once.
	// Never nil: New seeds it from Config.Live, or with an empty
	// configuration when the caller supplied none.
	live    *config.Live
	store   *store.Store
	router  *peerSyncRouter
	handler http.Handler
}

// New validates the config, builds the handler stack, and returns a Server
// ready to Serve. It does not open a listener — call ListenAndServe or
// hand a custom listener to Serve.
func New(cfg Config, s *store.Store) (*Server, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, errors.New("agent: store must not be nil")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	live := cfg.Live
	if live == nil {
		live = config.NewLive(nil)
	}
	srv := &Server{cfg: cfg, live: live, store: s}
	srv.handler = srv.buildHandler()
	return srv, nil
}

// reloadable reports whether this agent can adopt a config edit in place:
// it needs a config file to watch, the digest of the bytes it parsed, and a
// live holder to swap the result into. An embedder that assembled its
// configuration by hand has no file to reload from and gets the
// detect-and-surface behaviour alone.
func (s *Server) reloadable() bool {
	return s.cfg.Live != nil && s.cfg.ConfigPath != "" && len(s.cfg.ConfigDigest) == config.DigestLen
}

// Logger returns the structured logger the agent was configured with.
// Long-lived components started by the agent (the scheduler in #39)
// share this handle.
func (s *Server) Logger() *slog.Logger { return s.cfg.Logger }

// Handler exposes the internal HTTP handler so tests can drive it via
// net/http/httptest without going through the network stack.
func (s *Server) Handler() http.Handler { return s.handler }

// HasTLS reports whether the configured agent serves TLS. The CLI uses
// this for the startup banner; tests use it to decide which scheme to
// dial.
func (s *Server) HasTLS() bool { return s.cfg.TLSCert != "" }

// CertFingerprint returns the sha256: pin of the agent's configured TLS
// certificate — the value the startup banner prints so an operator can see
// what peers must pin. It errors when the agent serves plain HTTP (no cert)
// or the certificate file cannot be read.
func (s *Server) CertFingerprint() (string, error) {
	if s.cfg.TLSCert == "" {
		return "", errors.New("agent: no TLS certificate configured")
	}
	return FingerprintCertFile(s.cfg.TLSCert)
}

// Addr returns the configured listen address verbatim. For `:0`-style
// binds the kernel-assigned port is only knowable from the net.Listener
// the caller hands to Serve; this accessor is for the startup banner.
func (s *Server) Addr() string { return s.cfg.Listen }

func validateConfig(cfg Config) error {
	// An empty Listen selects listener-less mode (F35): the agent runs only
	// its background schedulers, so neither a bind address nor a bearer
	// token is required. A token is required only when there is an HTTP
	// surface to protect.
	if cfg.Listen != "" && cfg.Token == "" {
		return errors.New("agent: Config.Token is required")
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return errors.New("agent: Config.TLSCert and Config.TLSKey must be set together")
	}
	if cfg.Version == "" {
		return errors.New("agent: Config.Version is required")
	}
	if cfg.ScanInterval < 0 {
		return errors.New("agent: Config.ScanInterval must not be negative")
	}
	switch cfg.ScanStrategy {
	case "", ScanStrategyShallow, ScanStrategyDeep:
	default:
		return errors.New("agent: Config.ScanStrategy must be \"shallow\" or \"deep\"")
	}
	return nil
}

// buildHandler wires the route table. /v1/health is intentionally outside
// the auth wrapper so monitoring scripts can reach it without holding the
// agent's bearer token; every other route is wrapped individually so the
// pattern is obvious at the route declaration.
func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	s.router = newPeerSyncRouter(s)
	s.router.register(mux)
	return mux
}

// requireBearer is the auth middleware. The Authorization header must
// parse as `<scheme> <token>` with scheme matching "Bearer" case-
// insensitively (per RFC 7235 §2.1). The token authenticates when it
// matches the shared token or any configured per-peer token; a per-peer
// match attaches that node's identity to the request context so the sync
// handlers can bind a session to its caller (#110a).
func (s *Server) requireBearer(next http.Handler) http.Handler {
	auth := newAuthenticator(s.cfg.Token, s.cfg.PeerTokens)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := extractBearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		caller, ok := auth.authenticate(token)
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		next.ServeHTTP(w, withCallerNode(r, caller))
	})
}

// authenticator resolves a presented bearer token to an authenticated
// caller. The shared token and every per-peer token are pre-hashed to a
// fixed-length SHA-256 digest so the comparison time is independent of
// the attacker-controlled token length (subtle.ConstantTimeCompare
// short-circuits on a length mismatch, which would otherwise leak it).
// The per-peer set is keyed by digest rather than by the raw secret, so
// the map probe never compares attacker bytes against a stored secret
// directly.
type authenticator struct {
	sharedHash [32]byte
	peerNodes  map[[32]byte]string
}

func newAuthenticator(sharedToken string, peerTokens map[string]string) authenticator {
	a := authenticator{sharedHash: sha256.Sum256([]byte(sharedToken))}
	if len(peerTokens) > 0 {
		a.peerNodes = make(map[[32]byte]string, len(peerTokens))
		for token, node := range peerTokens {
			a.peerNodes[sha256.Sum256([]byte(token))] = node
		}
	}
	return a
}

// authenticate returns the caller's authenticated node name and whether
// the token is valid. A per-peer token yields its node name; the shared
// token authenticates with an empty name (no recoverable identity, the
// single-token case #110d leaves unbound).
func (a authenticator) authenticate(token string) (string, bool) {
	gotHash := sha256.Sum256([]byte(token))
	if node, ok := a.peerNodes[gotHash]; ok {
		return node, true
	}
	if subtle.ConstantTimeCompare(gotHash[:], a.sharedHash[:]) == 1 {
		return "", true
	}
	return "", false
}

// extractBearerToken parses `<scheme> <token>` from an Authorization
// header value. The scheme match is case-insensitive; trailing
// whitespace after the scheme is consumed so `Bearer  tok` (double
// space) works. Returns ok=false when the header is empty, malformed,
// or uses a non-Bearer scheme — all of which the middleware rejects
// uniformly as "missing bearer token" so callers can't infer which.
func extractBearerToken(header string) (string, bool) {
	scheme, rest, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token := strings.TrimLeft(rest, " ")
	if token == "" {
		return "", false
	}
	return token, true
}

// healthResponse is the documented shape of GET /v1/health. The field
// names are part of the wire protocol; do not rename.
type healthResponse struct {
	Version       string `json:"version"`
	SchemaVersion int    `json:"schema_version"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	sv, err := s.store.CurrentSchemaVersion(r.Context())
	if err != nil {
		// /v1/health is unauthenticated. The underlying store error can
		// contain filesystem paths or SQL fragments, so we return a
		// generic message and leave details to a server-side log story
		// (which lands with the rest of the observability work).
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{
		Version:       s.cfg.Version,
		SchemaVersion: sv,
	})
}

// errorResponse is the uniform JSON error body. Plain text would be
// fewer bytes but a structured shape lets clients pattern-match the
// `error` field without parsing prose.
type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

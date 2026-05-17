// Package daemon implements the squirrel HTTP daemon: the server-side
// component a peer node runs so another node can sync against it.
//
// This package owns the transport layer only — bearer-token auth, TLS
// termination, the public API surface, the health endpoint. Sync logic
// (plan negotiation, reconciliation, peer state) lives in higher-level
// packages and is wired in via future endpoints; the placeholder POST
// /v1/plan handler returns 501 so the auth middleware has something to
// guard in tests until that lands.
package daemon

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/mbertschler/squirrel/store"
)

// Config configures one daemon listener. Fields are validated by New; all
// of them are required except TLSCert/TLSKey (which must be set together
// or not at all — empty pair means plain HTTP).
type Config struct {
	// Listen is the bind address passed to net.Listen, e.g. "0.0.0.0:8443".
	Listen string
	// Token is the resolved bearer token compared (in constant time)
	// against the Authorization header on every authenticated request.
	Token string
	// TLSCert and TLSKey are filesystem paths to a PEM-encoded certificate
	// and matching private key. When both are empty the daemon serves
	// plain HTTP; when both are set it terminates TLS natively.
	TLSCert string
	TLSKey  string
	// Version is the daemon binary version reported via /v1/health.
	// Required so the field is never an unset zero-value in responses.
	Version string
}

// Server is one daemon instance. It holds the HTTP handler stack and a
// reference to the underlying store for the health endpoint's
// schema_version field; future endpoints (plan, reconcile, ...) will use
// the same handle.
type Server struct {
	cfg     Config
	store   *store.Store
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
		return nil, errors.New("daemon: store must not be nil")
	}
	srv := &Server{cfg: cfg, store: s}
	srv.handler = srv.buildHandler()
	return srv, nil
}

// Handler exposes the internal HTTP handler so tests can drive it via
// net/http/httptest without going through the network stack.
func (s *Server) Handler() http.Handler { return s.handler }

// HasTLS reports whether the configured daemon serves TLS. The CLI uses
// this for the startup banner; tests use it to decide which scheme to
// dial.
func (s *Server) HasTLS() bool { return s.cfg.TLSCert != "" }

// Addr returns the configured listen address. The actual address the
// kernel chose (after `:0`-style allocation) is only known after Serve
// owns a listener; see netListenerAddr in serve.go for that case.
func (s *Server) Addr() string { return s.cfg.Listen }

func validateConfig(cfg Config) error {
	if cfg.Listen == "" {
		return errors.New("daemon: Config.Listen is required")
	}
	if cfg.Token == "" {
		return errors.New("daemon: Config.Token is required")
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return errors.New("daemon: Config.TLSCert and Config.TLSKey must be set together")
	}
	if cfg.Version == "" {
		return errors.New("daemon: Config.Version is required")
	}
	return nil
}

// buildHandler wires the route table. /v1/health is intentionally outside
// the auth wrapper so monitoring scripts can reach it without holding the
// daemon's bearer token; every other route is wrapped individually so the
// pattern is obvious at the route declaration.
func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.Handle("POST /v1/plan", s.requireBearer(http.HandlerFunc(s.handlePlanPlaceholder)))
	return mux
}

// requireBearer is the auth middleware. It checks the Authorization
// header is `Bearer <token>` with the configured token, using
// subtle.ConstantTimeCompare to avoid leaking length-prefix information
// via response timing. The configured token must be non-empty (enforced
// by validateConfig).
func (s *Server) requireBearer(next http.Handler) http.Handler {
	expected := []byte(s.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		got := []byte(h[len(prefix):])
		if subtle.ConstantTimeCompare(got, expected) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
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
		writeError(w, http.StatusInternalServerError, "read schema_version: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{
		Version:       s.cfg.Version,
		SchemaVersion: sv,
	})
}

// handlePlanPlaceholder is the auth-guarded 501 stub. PR 3 replaces it
// with the real four-bucket plan negotiation.
func (s *Server) handlePlanPlaceholder(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotImplemented, "plan endpoint not yet implemented")
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

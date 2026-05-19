package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the SQLite-backed index. The exported API across this package
// hangs off it; per-domain methods live in volumes.go, files.go, runs.go,
// and migrations.go.
type Store struct {
	db *sql.DB
}

// OpenOptions tunes Store.Open. NodeName is the identity recorded as the
// self row in the nodes table on first migration to v6 (or beyond). An
// empty NodeName falls back to os.Hostname() — callers that don't yet wire
// a config-derived name still produce a self row.
type OpenOptions struct {
	NodeName string
}

// Open opens (or creates) the SQLite database at the given filesystem path
// and ensures the schema is at the version this binary expects. Equivalent
// to OpenWithOptions(path, OpenOptions{}) — the self node row gets a name
// derived from os.Hostname().
func Open(path string) (*Store, error) {
	return OpenWithOptions(path, OpenOptions{})
}

// OpenWithOptions opens or creates the SQLite database, applies any schema
// migrations, and seeds the self node row using opts.NodeName (falling back
// to os.Hostname() when empty). The path must be a plain filesystem path
// (no '?' query string and no URI scheme prefix); DSN parameters are
// managed internally so callers cannot override pragmas like journal_mode
// or busy_timeout. Returns an error if the database's schema version is
// newer than SchemaVersion, or if it is at an older unsupported version
// (this binary does not migrate v1 databases).
func OpenWithOptions(path string, opts OpenOptions) (*Store, error) {
	if err := validateDBPath(path); err != nil {
		return nil, err
	}
	nodeName, err := resolveNodeName(opts.NodeName)
	if err != nil {
		return nil, err
	}
	db, err := openSQLite(buildDSN(path))
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background(), nodeName); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// validateDBPath rejects DSN-injection vectors. The store appends its own
// pragmas via buildDSN, so any '?', '#', or URI scheme in the caller's
// path would let those defaults be overridden silently.
func validateDBPath(path string) error {
	if strings.ContainsAny(path, "?#") {
		return fmt.Errorf("db path %q must not contain '?' or '#'", path)
	}
	if strings.Contains(path, "://") || strings.HasPrefix(path, "file:") {
		return fmt.Errorf("db path %q must be a plain filesystem path, not a URI", path)
	}
	return nil
}

// buildDSN appends the pragmas the store relies on to the validated path.
// _txlock=immediate makes BeginTx start with `BEGIN IMMEDIATE`, acquiring
// the write lock at transaction start. Without it, a transaction that
// only reads first and then upgrades to a write (which is exactly what
// Upsert's state machine does) can race against another writer and lose
// the "supersede prior live row" step. With MaxOpenConns=1 today this is
// theoretical, but the schema-level invariants would still let a buggy
// future change land bad state — IMMEDIATE keeps the contract explicit.
//
// synchronous=NORMAL is safe under WAL: the WAL itself is fsynced at every
// commit so a power loss never corrupts the database, only loses the most
// recently committed transactions. We accept that trade for the order-of-
// magnitude write-throughput gain on indexing workloads.
func buildDSN(path string) string {
	return path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
}

// openSQLite opens the *sql.DB and pins it to a single connection so the
// pragmas in buildDSN apply to every query.
func openSQLite(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// resolveNodeName returns the explicit name when non-empty, else the host
// name. Explicit names are validated against the identifier rule the
// config layer enforces — the same string lands as a TOML key, a
// destination subfolder, and an rclone.conf section, so syntactic gaps
// between the store and the config would let a database be seeded with
// an identity that later config parsing rejects. The hostname fallback
// is sanitised rather than rejected because real-world hostnames often
// carry dots ("laptop.local") that the strict rule disallows; the
// sanitised form is deterministic and stays human-readable.
func resolveNodeName(name string) (string, error) {
	if name != "" {
		if !nodeNameRE.MatchString(name) {
			return "", fmt.Errorf("OpenOptions.NodeName %q is invalid (must match %s)", name, nodeNameRE)
		}
		return name, nil
	}
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("resolve hostname for self node name: %w", err)
	}
	sanitised := sanitiseNodeName(host)
	if sanitised == "" {
		return "", fmt.Errorf("os.Hostname %q has no characters usable as a node name; pass OpenOptions.NodeName", host)
	}
	return sanitised, nil
}

// nodeNameRE mirrors config.nameRE. Duplicated here to keep store
// free of a config import; if the rule ever changes, both sites change.
var nodeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// sanitiseNodeName produces a nodeNameRE-compliant identifier from a
// hostname by mapping every disallowed rune to '-', then trimming
// leading separators so the result begins with an alphanumeric (the
// regex anchor requires it). Returns "" when no usable characters
// survive.
func sanitiseNodeName(host string) string {
	var b strings.Builder
	for _, r := range host {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'),
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.TrimLeft(b.String(), "-_")
	if out == "" || !nodeNameRE.MatchString(out) {
		return ""
	}
	return out
}

func (s *Store) Close() error { return s.db.Close() }

// IsNotFound reports whether err is sql.ErrNoRows.
func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// NowNs returns the current wall-clock time in nanoseconds.
func NowNs() int64 { return time.Now().UnixNano() }

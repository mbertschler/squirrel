package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// path is the absolute filesystem path of the live DB. Normalised
	// via filepath.Abs at Open time; falls back to the caller-supplied
	// value if Abs returns an error (typically a cwd-resolution issue
	// on an exotic platform), so callers using Path() in production
	// can safely treat it as absolute.
	path string
}

// Path returns the absolute filesystem path of the live database file
// as normalised at Open time. Used by the CLI's `db backup` subcommand
// to derive a default backup directory next to the live DB, and by
// tests that need to inspect the file directly.
func (s *Store) Path() string { return s.path }

// OpenOptions tunes Store.Open. NodeName is the identity recorded as the
// self row in the nodes table on first migration to v6 (or beyond). An
// empty NodeName falls back to os.Hostname() — callers that don't yet wire
// a config-derived name still produce a self row.
type OpenOptions struct {
	NodeName string
	// BackupDir is where pre-migration snapshots land. Empty means
	// "<dirname(path)>/backups". Tests use a custom value to keep
	// snapshots inside t.TempDir(); production callers leave it
	// empty so backups sit next to the live DB.
	BackupDir string
	// DisablePreMigrationBackup turns off the snapshot taken before
	// each schema-advancing migration. Useful for tests that drive
	// runMigrations directly with a fixture and don't want backup
	// files cluttering the fixture directory; production callers
	// should leave it false.
	DisablePreMigrationBackup bool
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
	absPath, absErr := filepath.Abs(path)
	if absErr != nil {
		// Fall back to the verbatim path: filepath.Abs only fails when
		// it can't resolve the cwd, which is an environmental issue,
		// not a reason to refuse Open. The Path() accessor's doc warns
		// the field may be relative on such platforms.
		absPath = path
	}
	s := &Store{db: db, path: absPath}
	if err := s.migrate(context.Background(), nodeName, opts); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// validateDBPath rejects DSN-injection vectors. The store appends its own
// pragmas via buildDSN, so any '?', '#', or URI scheme in the caller's
// path would let those defaults be overridden silently. A NUL byte is
// rejected because it truncates the path at the C boundary the SQLite
// driver crosses — the file actually opened would differ from the string
// validated here. Leading or trailing ASCII whitespace is rejected too:
// it almost always signals a copy-paste or shell-quoting slip, and
// silently opening " db" or "db\n" hides the typo behind a second,
// surprising database file.
func validateDBPath(path string) error {
	if strings.ContainsRune(path, 0) {
		return fmt.Errorf("db path %q must not contain a NUL byte", path)
	}
	if path != strings.TrimFunc(path, isASCIISpace) {
		return fmt.Errorf("db path %q must not have leading or trailing whitespace", path)
	}
	if strings.ContainsAny(path, "?#") {
		return fmt.Errorf("db path %q must not contain '?' or '#'", path)
	}
	if strings.Contains(path, "://") || strings.HasPrefix(path, "file:") {
		return fmt.Errorf("db path %q must be a plain filesystem path, not a URI", path)
	}
	return nil
}

// isASCIISpace reports whether r is one of the ASCII whitespace runes
// (space, tab, newline, carriage return, vertical tab, form feed). We
// deliberately scope to ASCII rather than unicode.IsSpace: a path
// legitimately containing a non-breaking space is none of our business,
// but the ASCII set is what shell quoting and copy-paste slips produce.
func isASCIISpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
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
// synchronous stays at the SQLite default (FULL). Lowering it to NORMAL
// would save ~1-3 % of wall time on a steady-state indexing run (a few
// hundred fsyncs amortised across many batched writes), but the trade is
// "a crash can lose every commit since the last WAL checkpoint" — at odds
// with the project's never-lose-track-of-content principle. Keep the
// stronger guarantee.
func buildDSN(path string) string {
	return path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
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
//
// When the hostname sanitises to nothing usable (empty, or no
// alphanumeric survives — e.g. a host literally named "..."),
// fallbackNodeName synthesises a deterministic id rather than failing
// Open: a machine should always get a stable self-identity without the
// operator having to hand-pick OpenOptions.NodeName.
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
	if sanitised := sanitiseNodeName(host); sanitised != "" {
		return sanitised, nil
	}
	return fallbackNodeName(host), nil
}

// machineIDPath is the canonical location of the host's stable machine
// id on Linux. Pulled out as a var so the deterministic-fallback test
// doesn't depend on the host actually having the file.
var machineIDPath = "/etc/machine-id"

// fallbackNodeName synthesises a deterministic, nodeNameRE-compliant id
// for a host whose name yields nothing usable. It prefers a hash of
// /etc/machine-id (stable across reboots and independent of the unusable
// hostname); when that file is missing or empty it falls back to a hash
// of the raw hostname so the id is at least stable for this host. The
// "node-" prefix guarantees the leading-alphanumeric the regex anchor
// requires and labels the value as machine-derived rather than chosen.
func fallbackNodeName(host string) string {
	if seed := readMachineID(); seed != "" {
		return "node-" + shortHashHex(seed)
	}
	return "node-" + shortHashHex(host)
}

// readMachineID returns the trimmed contents of machineIDPath, or "" when
// the file is unreadable or blank. The trim drops the trailing newline
// systemd writes plus any stray surrounding whitespace so the hashed
// seed is exactly the id bytes.
func readMachineID() string {
	raw, err := os.ReadFile(machineIDPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// shortHashHex returns the first 12 hex characters (48 bits) of the
// SHA-256 of seed. Hex is always within nodeNameRE's allowed set and 48
// bits is ample to keep distinct machines distinct for an identifier
// that only needs to be stable, not cryptographically unique.
func shortHashHex(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:12]
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

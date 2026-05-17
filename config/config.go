// Package config loads and validates the squirrel TOML config file.
//
// The config is the source of truth for *which* volumes and destinations
// exist; the SQLite index records observations and run history. A volume
// declared in config but never indexed has no DB rows yet; a volume in the
// DB but not in config triggers a warning at load (orphan).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// EnvVar is the environment variable that overrides the default config path.
const EnvVar = "SQUIRREL_CONFIG"

// DefaultPath returns the resolved default config path, honoring
// $SQUIRREL_CONFIG before falling back to ~/.squirrel/config.toml.
func DefaultPath() (string, error) {
	if p := os.Getenv(EnvVar); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".squirrel", "config.toml"), nil
}

// Config is the resolved squirrel configuration. Field values are
// post-validation: paths are absolute, ~ is expanded, env-var references in
// secrets are resolved.
type Config struct {
	// Path is where the config was loaded from, used in diagnostics.
	Path string
	// DB is the absolute path to the index database. Empty when not set in
	// the config; the CLI's --db flag and built-in default fill that in.
	DB string
	// NodeName is this host's identity for node-to-node sync. Empty when
	// not set in the config; the store falls back to os.Hostname() when
	// inserting the self row at first migration. Must match nameRE.
	NodeName string
	// Volumes is keyed by volume name. Names match nameRE.
	Volumes map[string]*Volume
	// Destinations is keyed by destination name. Names match nameRE.
	Destinations map[string]*Destination
	// Nodes is keyed by node name. Names match nameRE. A name MUST NOT
	// collide with an entry in Destinations — the syntactic split is the
	// dispatch signal for sync.
	Nodes map[string]*Node
	// Daemon is non-nil when the config declares a `[daemon]` block. The
	// daemon subcommand requires it; other subcommands ignore it.
	Daemon *Daemon
}

// Volume is one indexable root.
type Volume struct {
	Name   string
	Path   string   // absolute, ~ expanded
	SyncTo []string // destination names declared on this volume
	// SyncEvery is the agent-scheduler cadence for full syncs of this
	// volume. Zero means "no scheduled sync" — the agent never auto-
	// triggers a sync for this volume; manual `squirrel sync` still
	// works. A scheduled sync always indexes immediately before
	// pushing.
	SyncEvery time.Duration
	// IndexEvery is the cadence for *standalone* index passes between
	// scheduled syncs, for finer-grained forensic history. Zero means
	// "no extra indexing". When SyncEvery is also set, IndexEvery
	// must be strictly shorter — equal or longer adds nothing on top
	// of the pre-sync indexing the scheduler already runs.
	IndexEvery time.Duration
}

// Destination is one rclone-backed remote. Type drives which Params are
// required and how the destination is rendered into rclone.conf.
type Destination struct {
	Name string
	Type string // local, sftp, s3, b2, gcs
	Root string // remote-side base directory for syncing volumes into
	// Params are type-specific rclone backend parameters with any
	// { env = "VAR" } references already resolved to literal strings.
	// Empty for type=local (no rclone remote needed).
	Params map[string]string
}

// nameRE is the syntactic rule for volume and destination names. We pick a
// conservative subset because the same identifier ends up as: a TOML key,
// a filesystem subfolder at the destination, and an rclone.conf section
// name. Each layer has its own quoting rules; intersecting them avoids
// surprises.
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// Load reads and validates the TOML config at path. ErrNoConfig is returned
// when the file does not exist so callers can render a helpful "create your
// config" message rather than treating it as a generic IO failure.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &MissingError{Path: path}
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var raw rawConfig
	dec := toml.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, formatTomlError(err))
	}
	cfg, err := raw.resolve(path)
	if err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// MissingError is returned by Load when the config file does not exist. It
// carries the absolute path that was checked so the caller can produce a
// "create a config at <path>" hint.
type MissingError struct {
	Path string
}

func (e *MissingError) Error() string {
	return fmt.Sprintf("no config at %s", e.Path)
}

// IsMissing reports whether err is a MissingError.
func IsMissing(err error) bool {
	var m *MissingError
	return errors.As(err, &m)
}

// rawConfig is the on-disk shape; resolve() turns it into Config after
// validation, name normalisation, and secret resolution. Destination
// parameters arrive as map[string]any because each type has its own
// per-field schema; resolve dispatches to per-type validators.
type rawConfig struct {
	DB           string                    `toml:"db"`
	NodeName     string                    `toml:"node_name"`
	Volumes      map[string]rawVolume      `toml:"volumes"`
	Destinations map[string]map[string]any `toml:"destinations"`
	Nodes        map[string]rawNode        `toml:"nodes"`
	Daemon       *rawDaemon                `toml:"daemon"`
}

type rawVolume struct {
	Path       string   `toml:"path"`
	SyncTo     []string `toml:"sync_to"`
	SyncEvery  string   `toml:"sync_every"`
	IndexEvery string   `toml:"index_every"`
}

func (r *rawConfig) resolve(path string) (*Config, error) {
	cfg := &Config{
		Path:         path,
		Volumes:      make(map[string]*Volume, len(r.Volumes)),
		Destinations: make(map[string]*Destination, len(r.Destinations)),
		Nodes:        make(map[string]*Node, len(r.Nodes)),
	}
	if r.DB != "" {
		expanded, err := expandPath(r.DB)
		if err != nil {
			return nil, fmt.Errorf("db: %w", err)
		}
		cfg.DB = expanded
	}
	if r.NodeName != "" {
		if !nameRE.MatchString(r.NodeName) {
			return nil, fmt.Errorf("node_name %q is invalid (must match %s)", r.NodeName, nameRE)
		}
		cfg.NodeName = r.NodeName
	}
	for name, raw := range r.Destinations {
		dest, err := resolveDestination(name, raw)
		if err != nil {
			return nil, fmt.Errorf("destinations.%s: %w", name, err)
		}
		cfg.Destinations[name] = dest
	}
	for name, raw := range r.Nodes {
		if _, clash := cfg.Destinations[name]; clash {
			return nil, fmt.Errorf("nodes.%s: name also declared as a destination — names must be unique across both kinds", name)
		}
		node, err := resolveNode(name, &raw)
		if err != nil {
			return nil, fmt.Errorf("nodes.%s: %w", name, err)
		}
		cfg.Nodes[name] = node
	}
	for name, raw := range r.Volumes {
		vol, err := resolveVolume(name, raw, cfg.Destinations, cfg.Nodes)
		if err != nil {
			return nil, fmt.Errorf("volumes.%s: %w", name, err)
		}
		cfg.Volumes[name] = vol
	}
	if r.Daemon != nil {
		d, err := resolveDaemon(r.Daemon)
		if err != nil {
			return nil, fmt.Errorf("daemon: %w", err)
		}
		cfg.Daemon = d
	}
	return cfg, nil
}

func resolveVolume(name string, raw rawVolume, dests map[string]*Destination, nodes map[string]*Node) (*Volume, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("invalid volume name (must match %s)", nameRE)
	}
	if raw.Path == "" {
		return nil, errors.New("path is required")
	}
	abs, err := expandPath(raw.Path)
	if err != nil {
		return nil, fmt.Errorf("path: %w", err)
	}
	for _, dst := range raw.SyncTo {
		if _, ok := dests[dst]; ok {
			continue
		}
		if _, ok := nodes[dst]; ok {
			continue
		}
		return nil, fmt.Errorf("sync_to references unknown destination or node %q", dst)
	}
	syncEvery, err := parseVolumeCadence("sync_every", raw.SyncEvery)
	if err != nil {
		return nil, err
	}
	indexEvery, err := parseVolumeCadence("index_every", raw.IndexEvery)
	if err != nil {
		return nil, err
	}
	// The scheduler always indexes immediately before each scheduled
	// sync, so an index_every that is equal to or longer than
	// sync_every contributes no extra observations between syncs —
	// it's almost certainly a misconfiguration. Reject it loudly.
	if syncEvery > 0 && indexEvery > 0 && indexEvery >= syncEvery {
		return nil, errors.New("index_every must be strictly shorter than sync_every (pre-sync indexing already runs at sync_every cadence)")
	}
	return &Volume{
		Name:       name,
		Path:       abs,
		SyncTo:     raw.SyncTo,
		SyncEvery:  syncEvery,
		IndexEvery: indexEvery,
	}, nil
}

// volumeCadenceFloor is the minimum scheduler interval for either
// per-volume cadence knob. The floor exists to catch obvious
// misconfigurations (e.g. forgetting the unit suffix and writing `5`
// where `5m` was intended) before the scheduler spins up a tight loop.
const volumeCadenceFloor = time.Second

// parseVolumeCadence parses an optional cadence string. Empty stays
// zero (caller treats that as "agent does not auto-trigger this
// cadence for the volume"). Non-empty must (a) parse as a
// time.Duration, (b) be strictly positive, and (c) be at least
// volumeCadenceFloor.
func parseVolumeCadence(field, raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	dur, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s %q: %w", field, raw, err)
	}
	if dur <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, got %s", field, dur)
	}
	if dur < volumeCadenceFloor {
		return 0, fmt.Errorf("%s must be at least %s, got %s", field, volumeCadenceFloor, dur)
	}
	return dur, nil
}

// formatTomlError unwraps pelletier's StrictMissingError so the bare
// "strict mode: fields in the document are missing in the target struct"
// message is replaced with the actual offending key(s) and line(s).
// The Errors slice carries per-key diagnostics that go-toml's top-level
// Error() throws away.
func formatTomlError(err error) error {
	if sme, ok := errors.AsType[*toml.StrictMissingError](err); ok {
		return errors.New(sme.String())
	}
	return err
}

// expandPath turns a leading ~ into the user's home directory and returns
// the absolute path. Relative paths are resolved against the current working
// directory of the squirrel binary at load time, which is consistent with
// other tools that take paths from a config file.
func expandPath(p string) (string, error) {
	if strings.HasPrefix(p, "~") && (len(p) == 1 || p[1] == '/') {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~: %w", err)
		}
		p = filepath.Join(home, p[1:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}
	return abs, nil
}

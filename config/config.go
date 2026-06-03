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
	// Agent is non-nil when the config declares an `[agent]` block. The
	// agent subcommand requires it; other subcommands ignore it.
	Agent *Agent
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
	// Hook is the per-volume external-tool hook, or nil when the volume
	// declares no `[volumes.X.hook]` block. The agent exec's its command
	// (without a shell) on a trigger and records only the generic outcome
	// — squirrel never learns what the command does. See VolumeHook.
	Hook *VolumeHook
}

// VolumeHook is a per-volume, best-effort command squirrel runs to nudge
// an external tool when content settles or on a cadence (#84). squirrel
// stays tool-agnostic: it exec's Command without a shell, passes context
// via SQUIRREL_* environment variables, and records only the exit
// code/timestamps. There is intentionally no rules engine — a single
// command, distinguished across triggers by the SQUIRREL_TRIGGER env var
// the agent sets, not by separate config.
type VolumeHook struct {
	// Command is the argv exec'd without a shell — Command[0] is the
	// program, the rest its arguments. Users wanting shell features write
	// `sh -c '…'` themselves; squirrel never string-concatenates the
	// volume path into a command line.
	Command []string
	// Timeout bounds one invocation so a hung hook can't wedge the agent's
	// scheduler. Zero is replaced with DefaultHookTimeout at load time.
	Timeout time.Duration
	// Interval is the cadence for the interval ("check") trigger: the
	// agent fires the same command on this period regardless of whether
	// content changed (the motivating use is periodic backup
	// verification, where bitrot happens to static data and so must be
	// re-checked on a clock, not on an event). Zero means "no interval
	// firing" — the hook then fires only on-change. The command tells the
	// two triggers apart via SQUIRREL_TRIGGER (change vs interval).
	Interval time.Duration
}

// DefaultHookTimeout is the per-invocation timeout applied when a hook
// block omits `timeout`. Generous because the motivating consumer (a
// backup tool snapshotting a large volume) can legitimately run for a
// while; the bound exists to reap a truly wedged process, not to cap
// normal work. Overlap between invocations is handled separately by the
// agent's don't-stack guard, so a hook that outlives its own cadence is
// skipped rather than stacked.
const DefaultHookTimeout = time.Hour

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
	Agent        *rawAgent                 `toml:"agent"`
}

type rawVolume struct {
	Path       string         `toml:"path"`
	SyncTo     []string       `toml:"sync_to"`
	SyncEvery  string         `toml:"sync_every"`
	IndexEvery string         `toml:"index_every"`
	Hook       *rawVolumeHook `toml:"hook"`
}

type rawVolumeHook struct {
	Command  []string `toml:"command"`
	Timeout  string   `toml:"timeout"`
	Interval string   `toml:"interval"`
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
	if r.Agent != nil {
		a, err := resolveAgent(r.Agent)
		if err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
		cfg.Agent = a
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
	hook, err := resolveVolumeHook(raw.Hook)
	if err != nil {
		return nil, err
	}
	return &Volume{
		Name:       name,
		Path:       abs,
		SyncTo:     raw.SyncTo,
		SyncEvery:  syncEvery,
		IndexEvery: indexEvery,
		Hook:       hook,
	}, nil
}

// resolveVolumeHook validates an optional `[volumes.X.hook]` block. A nil
// raw (no block) yields a nil hook. When present, command is required and
// every argv element must be non-empty (an empty element is almost always
// a templating mistake that would exec the wrong program). timeout is
// optional; empty falls back to DefaultHookTimeout.
func resolveVolumeHook(raw *rawVolumeHook) (*VolumeHook, error) {
	if raw == nil {
		return nil, nil
	}
	if len(raw.Command) == 0 {
		return nil, errors.New("hook.command is required and must have at least one element")
	}
	for i, arg := range raw.Command {
		if arg == "" {
			return nil, fmt.Errorf("hook.command[%d] is empty", i)
		}
	}
	timeout := DefaultHookTimeout
	if raw.Timeout != "" {
		dur, err := time.ParseDuration(raw.Timeout)
		if err != nil {
			return nil, fmt.Errorf("hook.timeout %q: %w", raw.Timeout, err)
		}
		if dur <= 0 {
			return nil, fmt.Errorf("hook.timeout must be a positive duration, got %s", dur)
		}
		timeout = dur
	}
	interval, err := parseVolumeCadence("hook.interval", raw.Interval)
	if err != nil {
		return nil, err
	}
	return &VolumeHook{
		Command:  append([]string(nil), raw.Command...),
		Timeout:  timeout,
		Interval: interval,
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

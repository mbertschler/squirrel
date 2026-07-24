// Package volmark reads, writes, and validates the .squirrel-volume
// marker file that sits at the root of every squirrel-managed volume
// tree (both source vol.Path and destination <root>/<volume>/).
//
// The marker is the safety gate against pointing sync or restore at
// the wrong directory: a typo in dest.Root or vol.Path silently
// targets a directory of unrelated files unless that directory carries
// the marker. Tools writing into these trees must filter the marker
// out of their transfer/comparison flows; the file is metadata, not
// payload.
package volmark

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// MarkerName is the reserved filename at the root of a squirrel
// volume directory. Tools that walk these directories must skip it
// (same treatment as .squirrel-history and .squirrel-conflicts).
const MarkerName = ".squirrel-volume"

// Marker is the parsed contents of a .squirrel-volume file. Volume
// is the only field consulted for validation; Node and CreatedAt are
// forensic metadata so an operator inspecting the marker later can
// answer "who initialised this directory and when?"
//
// A binary-version field was considered but dropped: squirrel doesn't
// plumb a build-time version string today, and an always-empty field
// on every marker would be misleading documentation. If/when a real
// version is available, add it back here and stamp it at every Write
// call site.
type Marker struct {
	Volume    string `json:"volume"`
	Node      string `json:"node,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// ErrMissing is returned by Read and Validate when the marker file
// is not present at the given root.
var ErrMissing = errors.New("squirrel volume marker is missing")

// ErrMismatch is returned by Validate when the marker exists but
// names a different volume than the caller expected. Expected is
// what the caller asked for; Found is what the marker carries.
type ErrMismatch struct {
	Root     string
	Expected string
	Found    string
}

func (e *ErrMismatch) Error() string {
	return fmt.Sprintf("squirrel volume marker at %s names %q, want %q", e.Root, e.Found, e.Expected)
}

// Path returns the absolute path of the marker file under root.
func Path(root string) string {
	return filepath.Join(root, MarkerName)
}

// Read parses the marker at root. Returns ErrMissing wrapped via
// errors.Is when the file does not exist; other I/O or parse
// failures are surfaced verbatim.
func Read(root string) (*Marker, error) {
	data, err := os.ReadFile(Path(root))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrMissing
		}
		return nil, fmt.Errorf("read marker: %w", err)
	}
	m, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%w at %s", err, Path(root))
	}
	return m, nil
}

// Parse decodes marker bytes and enforces the required volume field. It
// is the format's single decoder: Read composes it with a filesystem
// read, and callers that already hold the marker bytes (for instance a
// marker fetched over rclone from a remote destination) parse them
// directly without touching the local disk.
func Parse(data []byte) (*Marker, error) {
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse marker: %w", err)
	}
	if m.Volume == "" {
		return nil, errors.New("parse marker: missing volume field")
	}
	return &m, nil
}

// Marshal renders m as the canonical marker file bytes (indented JSON
// with a trailing newline). It is the format's single encoder: Write
// composes it with an atomic filesystem write, and callers writing a
// marker through another transport (rclone copyto to a remote root) use
// the same bytes so a remote marker is byte-identical to a local one.
func Marshal(m Marker) ([]byte, error) {
	if m.Volume == "" {
		return nil, fmt.Errorf("volmark.Marshal: volume must be non-empty")
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("volmark.Marshal: %w", err)
	}
	return append(data, '\n'), nil
}

// Write atomically writes m to <root>/.squirrel-volume via a sibling
// tempfile + rename. The root directory must already exist; missing
// intermediate directories surface as an explicit error rather than a
// silent create-on-write since the marker lives at the volume root,
// which the caller has already validated.
func Write(root string, m Marker) error {
	data, err := Marshal(m)
	if err != nil {
		return err
	}
	if _, err := os.Stat(root); err != nil {
		return fmt.Errorf("volmark.Write: stat root %s: %w", root, err)
	}
	tmp, err := os.CreateTemp(root, ".squirrel-volume.*")
	if err != nil {
		return fmt.Errorf("volmark.Write: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("volmark.Write: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("volmark.Write: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("volmark.Write: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, Path(root)); err != nil {
		cleanup()
		return fmt.Errorf("volmark.Write: rename: %w", err)
	}
	return nil
}

// Validate reads the marker at root and verifies its Volume field
// matches expected. Returns ErrMissing (sentinel, errors.Is-friendly)
// or *ErrMismatch (errors.As-friendly) for the two distinguishable
// failure modes; other errors propagate verbatim.
func Validate(root, expected string) error {
	m, err := Read(root)
	if err != nil {
		return err
	}
	if m.Volume != expected {
		return &ErrMismatch{Root: root, Expected: expected, Found: m.Volume}
	}
	return nil
}

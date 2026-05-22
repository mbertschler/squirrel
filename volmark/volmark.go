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
// is the only field consulted for validation; Node, Version, and
// CreatedAt are forensic metadata so an operator inspecting the
// marker later can answer "who initialised this directory and when?"
type Marker struct {
	Volume    string `json:"volume"`
	Node      string `json:"node,omitempty"`
	Version   string `json:"version,omitempty"`
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
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse marker at %s: %w", Path(root), err)
	}
	if m.Volume == "" {
		return nil, fmt.Errorf("parse marker at %s: missing volume field", Path(root))
	}
	return &m, nil
}

// Write atomically writes m to <root>/.squirrel-volume via a sibling
// tempfile + rename. The root directory must already exist; missing
// intermediate directories surface as an explicit error rather than a
// silent create-on-write since the marker lives at the volume root,
// which the caller has already validated.
func Write(root string, m Marker) error {
	if m.Volume == "" {
		return fmt.Errorf("volmark.Write: volume must be non-empty")
	}
	if _, err := os.Stat(root); err != nil {
		return fmt.Errorf("volmark.Write: stat root %s: %w", root, err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("volmark.Write: marshal: %w", err)
	}
	data = append(data, '\n')
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

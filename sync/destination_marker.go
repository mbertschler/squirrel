package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"time"

	"github.com/mbertschler/squirrel/volmark"
)

// maxMarkerBytes bounds how much of a destination's marker squirrel reads.
const maxMarkerBytes = 64 << 10

// ensureMarker is a native destination's gate against a wrong or unmounted
// root: the destination's <volume>/.squirrel-volume must name this volume.
// A missing marker is written only under init, and a marker naming another
// volume, or one that does not parse, is always refused and never
// overwritten. A local root that does not exist yet is created under init,
// so a first push can land on an empty disk.
func (h *destinationRoot) ensureMarker(ctx context.Context, init bool) error {
	if init && h.dest.Type == "local" {
		if err := os.MkdirAll(h.dest.Root, 0o755); err != nil {
			return fmt.Errorf("destination %q: create root %s: %w", h.dest.Name, h.dest.Root, err)
		}
	}
	tr, err := h.guarded(ctx, nameGuard{volumeDir: h.volumeDir, bootstrap: init})
	if err != nil {
		return err
	}
	name := path.Join(h.volumeDir, volmark.MarkerName)
	data, err := readSmallFile(ctx, tr, name, maxMarkerBytes)
	switch {
	case err == nil:
		return h.checkMarker(name, data)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("destination %q: read %s: %w", h.dest.Name, name, err)
	case !init:
		return fmt.Errorf("destination %q has no %s at %s — re-run with --init to bootstrap (refusing in case the root is a typo or the disk is not mounted): %w", h.dest.Name, volmark.MarkerName, h.where(name), ErrRefused)
	}
	return h.writeMarker(ctx, tr, name)
}

func (h *destinationRoot) checkMarker(name string, data []byte) error {
	m, err := volmark.Parse(data)
	if err != nil {
		return fmt.Errorf("destination %q: %w at %s: %w", h.dest.Name, err, h.where(name), ErrRefused)
	}
	if m.Volume != h.volumeDir {
		return fmt.Errorf("destination %q: %s at %s names volume %q, want %q (refusing to sync over a different volume's tree): %w",
			h.dest.Name, volmark.MarkerName, h.where(name), m.Volume, h.volumeDir, ErrRefused)
	}
	return nil
}

// writeMarker stages the marker, then renames it onto name, so a write
// that fails halfway never leaves a marker that does not parse. A copy an
// earlier --init left staged is cleared first.
func (h *destinationRoot) writeMarker(ctx context.Context, tr transport, name string) error {
	m, err := selfMarker(ctx, h.store, h.volumeDir)
	if err != nil {
		return fmt.Errorf("destination %q: %w", h.dest.Name, err)
	}
	data, err := volmark.Marshal(m)
	if err != nil {
		return fmt.Errorf("destination %q: %w", h.dest.Name, err)
	}
	staged := markerStagingName(h.volumeDir)
	if err := tr.Remove(ctx, staged); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("destination %q: clear the staged marker %s: %w", h.dest.Name, h.where(staged), err)
	}
	if err := tr.Put(ctx, staged, bytes.NewReader(data), time.Now()); err != nil {
		return fmt.Errorf("destination %q: write %s: %w", h.dest.Name, h.where(staged), err)
	}
	if err := tr.Rename(ctx, staged, name); err != nil {
		return fmt.Errorf("destination %q: write %s: %w", h.dest.Name, h.where(name), err)
	}
	return nil
}

// where names a destination-relative name for a message: the path on a
// local disk, host:path on an sftp server.
func (h *destinationRoot) where(name string) string {
	full := path.Join(h.dest.Root, name)
	if h.dest.Type == "sftp" {
		return h.dest.Params["host"] + ":" + full
	}
	return full
}

// readSmallFile reads the whole regular file at name, refusing one larger
// than limit.
func readSmallFile(ctx context.Context, tr transport, name string, limit int64) ([]byte, error) {
	rc, err := tr.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(ctxReader{ctx: ctx, r: rc}, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return data, nil
}

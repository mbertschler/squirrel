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
// so a first push can land on an empty disk, and refused like a missing
// marker otherwise.
func (h *destinationRoot) ensureMarker(ctx context.Context, init bool) error {
	if err := h.ensureLocalRoot(ctx, init); err != nil {
		return err
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
		return fmt.Errorf("destination %q has no %s at %s — %s: %w", h.dest.Name, volmark.MarkerName, h.where(name), h.bootstrapHint(ctx), ErrRefused)
	}
	return h.writeMarker(ctx, tr, name)
}

func (h *destinationRoot) ensureLocalRoot(ctx context.Context, init bool) error {
	if h.dest.Type != "local" {
		return nil
	}
	_, err := os.Stat(h.dest.Root)
	switch {
	case !errors.Is(err, fs.ErrNotExist):
		return nil
	case init:
		if err := os.MkdirAll(h.dest.Root, 0o755); err != nil {
			return fmt.Errorf("destination %q: create root %s: %w", h.dest.Name, h.dest.Root, err)
		}
		return nil
	}
	return fmt.Errorf("destination %q has no root at %s — %s: %w", h.dest.Name, h.dest.Root, h.bootstrapHint(ctx), ErrRefused)
}

// bootstrapHint is what a refusal over a missing root or marker asks for.
// A volume that synced to the destination before points at an unmounted
// disk or a moved root, not at --init, which would bootstrap a new, empty
// destination in its place.
func (h *destinationRoot) bootstrapHint(ctx context.Context) string {
	if h.volumeSyncedBefore(ctx) {
		return fmt.Sprintf("volume %q has synced to it before, so the disk is most likely not mounted or the root moved; mount it and sync again (--init is only for a new, empty destination)", h.volumeDir)
	}
	return "re-run with --init to bootstrap (refusing in case the root is a typo or the disk is not mounted)"
}

func (h *destinationRoot) volumeSyncedBefore(ctx context.Context) bool {
	v, err := h.store.GetVolumeByName(ctx, h.volumeDir)
	if err != nil {
		return false
	}
	_, err = h.store.LatestSuccessfulSyncRun(ctx, v.ID, h.dest.Name)
	return err == nil
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
// that fails halfway never leaves a marker that does not parse; its bytes
// are flushed before the rename and the rename before it returns. A copy
// an earlier --init left staged is cleared first.
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
	if err := landStaged(ctx, tr, staged, name); err != nil {
		return fmt.Errorf("destination %q: write %s: %w", h.dest.Name, h.where(name), err)
	}
	return nil
}

// landStaged renames a staged file onto name, flushing its bytes before
// the rename and the rename after it, so neither a crash nor a power cut
// leaves name holding anything but the whole file.
func landStaged(ctx context.Context, tr transport, staged, name string) error {
	if err := tr.Flush(ctx); err != nil {
		return err
	}
	if err := tr.Rename(ctx, staged, name); err != nil {
		return err
	}
	return tr.Flush(ctx)
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

package sync

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"time"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/runevents"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/volmark"
)

// mirrorHandler pushes a volume to a native mirror: the volume's tree at
// <dest.root>/<volume>/, written through a transport instead of rclone.
// It plans from the index like the content layouts, writes each changed
// path through .squirrel-staging/, moves whatever the path held into
// .squirrel-history/run-<id>/ first, and records every version it wrote
// in remote_paths. Each run leaves a receipt, the same manifest segment the
// content layouts write, at <volume>/.squirrel-index/run-<id>.
type mirrorHandler struct {
	store *store.Store
	vol   *config.Volume
	dest  *config.Destination
	// openTransport opens the destination root; tests wrap it to inject
	// faults.
	openTransport func(*config.Destination) (transport, error)
	// stallTimeout bounds every transport call by progress.
	stallTimeout time.Duration

	raw      transport                // opened by root on first use, closed when Push returns
	progress func(runevents.Progress) // this push's Options.Progress
}

func (h *mirrorHandler) TargetName() string { return h.dest.Name }

func (h *mirrorHandler) sealed() {}

func (h *mirrorHandler) Push(ctx context.Context, opts Options) (Report, error) {
	rep := Report{Volume: h.vol.Name, Destination: h.dest.Name}
	rep.Verification.Method = VerifyMethodPresenceSize
	if opts.Shallow {
		return rep, fmt.Errorf("destination %q is a native mirror: a push already decides from squirrel's records, confirms each planned path's size and mtime, and moves whatever is there aside before writing, so --shallow has nothing to switch off", h.dest.Name)
	}
	if n := stalledCounter(h.dest.Name).Load(); n > 0 {
		return rep, fmt.Errorf("destination %q: %d transport call(s) an earlier push gave up on have not returned — the disk or server may be hung; wait, or restart squirrel once it is reachable again", h.dest.Name, n)
	}
	h.progress = opts.Progress
	defer h.closeRoot()
	return pushThrough(ctx, pushTarget{store: h.store, vol: h.vol, dest: h.dest}, h, opts)
}

// root is the destination root as a layout may hold it: guarded, so only
// runID's own moves pass (none when runID is 0), and bounded by progress.
func (h *mirrorHandler) root(runID int64) (transport, error) {
	if h.raw == nil {
		raw, err := h.openTransport(h.dest)
		if err != nil {
			return nil, err
		}
		h.raw = raw
	}
	guard := nameGuard{volumeDir: h.vol.Name, runID: runID, finished: h.runFinished}
	return stallTransport{
		transport: guardedTransport{transport: h.raw, guard: guard},
		timeout:   h.stallTimeout,
		stalled:   stalledCounter(h.dest.Name),
	}, nil
}

func (h *mirrorHandler) closeRoot() {
	if h.raw != nil {
		_ = h.raw.Close()
		h.raw = nil
	}
}

// runFinished reports whether runID has ended, so its staging may be
// removed. A run squirrel cannot look up is left alone.
func (h *mirrorHandler) runFinished(runID int64) bool {
	r, err := h.store.GetRun(context.Background(), runID)
	return err == nil && r.Status != store.RunStatusRunning
}

// markers gates the push on the per-volume .squirrel-volume marker, the
// guard against pushing to an unmounted disk or a mistyped root. A refusal
// is recorded as its own refused run, so a disk that stays unplugged shows
// up in `squirrel runs` and the TUI. A dry run skips the gate: it writes
// nothing.
func (h *mirrorHandler) markers(ctx context.Context, rep *Report, volumeID int64, opts Options) error {
	if opts.DryRun {
		return nil
	}
	merr := ensureLocalDestinationMarker(ctx, h.store, h.dest, h.vol.Name, opts.Init)
	if merr == nil {
		return nil
	}
	_, err := recordSyncRefusal(ctx, h.store, volumeID, h.dest.Name, rep, merr)
	return err
}

// landed reports whether runID left its receipt at the destination.
func (h *mirrorHandler) landed(ctx context.Context, runID int64) (bool, error) {
	tr, err := h.root(0)
	if err != nil {
		return false, err
	}
	e, err := tr.Stat(ctx, h.receiptName(runID))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errParentNotDir) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return e.kind == kindFile, nil
}

// rootEmpty reports whether the destination root holds no file beyond
// the volume markers, walking it until the first one.
func (h *mirrorHandler) rootEmpty(ctx context.Context) (bool, error) {
	tr, err := h.root(0)
	if err != nil {
		return false, err
	}
	return treeEmpty(ctx, tr, ".")
}

func treeEmpty(ctx context.Context, tr transport, dir string) (bool, error) {
	entries, err := tr.List(ctx, dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		switch {
		case e.kind == kindDir:
			empty, err := treeEmpty(ctx, tr, path.Join(dir, e.name))
			if err != nil || !empty {
				return false, err
			}
		case e.kind == kindFile && e.name == volmark.MarkerName:
		default:
			return false, nil
		}
	}
	return true, nil
}

func (h *mirrorHandler) foreignHistory(runID int64) error {
	return fmt.Errorf("destination %q: the last successful sync (run %d) left no receipt at %s — the tree was written by rclone or another layout, and a native mirror does not adopt an existing tree; point the destination at a fresh or emptied root, or (after emptying it) run `squirrel destination reset %s`: %w", h.dest.Name, runID, h.receiptName(runID), h.dest.Name, ErrRefused)
}

// seal writes the run's receipt, the manifest segment of its delta, and
// confirms it landed at its size.
func (h *mirrorHandler) seal(ctx context.Context, _ *Report, runID int64, p pushPlan, _ *mirrorOps) error {
	body, err := encodeManifestSegment(p.delta)
	if err != nil {
		return err
	}
	tr, err := h.root(runID)
	if err != nil {
		return err
	}
	name := h.receiptName(runID)
	if err := tr.Put(ctx, name, bytes.NewReader(body), time.Now()); err != nil {
		return fmt.Errorf("write receipt %s: %w", name, err)
	}
	e, err := tr.Stat(ctx, name)
	if err != nil {
		return fmt.Errorf("confirm receipt %s: %w", name, err)
	}
	if e.size != int64(len(body)) {
		return fmt.Errorf("receipt %s landed with size %d, want %d", name, e.size, len(body))
	}
	return nil
}

// advanceMethod is presence+size: the push hashed every path's bytes as
// they streamed out and confirmed each landed at its size.
func (h *mirrorHandler) advanceMethod(context.Context, *Report, pushPlan) (string, error) {
	return store.VerifyMethodPresenceSize, nil
}

func (h *mirrorHandler) receiptName(runID int64) string {
	return path.Join(h.vol.Name, IndexDirName, "run-"+strconv.FormatInt(runID, 10))
}

// liveName is where a volume-relative path lives on the destination.
func (h *mirrorHandler) liveName(rel string) string { return path.Join(h.vol.Name, rel) }

// stagingKey names a path's staging file: the lowercase hex BLAKE3 of the
// volume-relative path, unique per path and unchanged by a destination
// that folds case.
func stagingKey(rel string) string {
	sum := blake3.Sum256([]byte(rel))
	return hex.EncodeToString(sum[:])
}

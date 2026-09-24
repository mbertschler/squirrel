package sync

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
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
	*destinationRoot
	vol      *config.Volume
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
	defer h.close()
	return pushThrough(ctx, pushTarget{store: h.store, vol: h.vol, dest: h.dest}, h, opts)
}

// root is the destination root as a layout may hold it: guarded, so only
// runID's own moves pass (none when runID is 0), and bounded by progress.
func (h *mirrorHandler) root(ctx context.Context, runID int64) (transport, error) {
	return h.guarded(ctx, nameGuard{volumeDir: h.vol.Name, runID: runID, finished: h.runFinished})
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
	merr := h.ensureMarker(ctx, opts.Init)
	if merr == nil {
		return nil
	}
	_, err := recordSyncRefusal(ctx, h.store, volumeID, h.dest.Name, rep, merr)
	return err
}

// landed reports whether runID left its receipt at the destination.
func (h *mirrorHandler) landed(ctx context.Context, runID int64) (bool, error) {
	tr, err := h.root(ctx, 0)
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
// the volume markers.
func (h *mirrorHandler) rootEmpty(ctx context.Context) (bool, error) {
	tr, err := h.root(ctx, 0)
	if err != nil {
		return false, err
	}
	return treeEmpty(ctx, tr, ".")
}

// treeEmpty reports whether dir holds no file beyond volume markers,
// walking it until the first one. A directory skip names is not walked.
func treeEmpty(ctx context.Context, tr transport, dir string, skip ...string) (bool, error) {
	entries, err := tr.List(ctx, dir)
	if err != nil {
		return false, err
	}
	names := entryNames(entries)
	for _, e := range entries {
		switch {
		case appleDouble(e.name, names):
		case e.kind == kindDir && slices.Contains(skip, path.Join(dir, e.name)):
		case e.kind == kindDir:
			empty, err := treeEmpty(ctx, tr, path.Join(dir, e.name), skip...)
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

// firstPush refuses to start a volume's mirror over a volume directory
// that already holds files squirrel has no record of writing — a tree
// rclone wrote, another layout's, or a native mirror whose index is gone:
// a native mirror adopts no tree. A first push that crashed left records,
// so its retry passes.
func (h *mirrorHandler) firstPush(ctx context.Context, volumeID int64) error {
	recorded, err := h.store.VolumeHasRemotePaths(ctx, h.dest.Name, volumeID)
	if err != nil || recorded {
		return err
	}
	tr, err := h.root(ctx, 0)
	if err != nil {
		return err
	}
	empty, err := treeEmpty(ctx, tr, h.vol.Name, path.Join(h.vol.Name, StagingDirName))
	if errors.Is(err, fs.ErrNotExist) || empty {
		return nil
	}
	if err != nil {
		return fmt.Errorf("destination %q: check whether %s/ is empty: %w", h.dest.Name, h.vol.Name, err)
	}
	return fmt.Errorf("destination %q: %s/ already holds files, but squirrel has no record of syncing volume %q there; a native mirror does not adopt a tree it did not record writing: if squirrel wrote it, recover this index from it with `squirrel recover --from %s`; otherwise point the destination at a fresh root, or empty %s/ apart from its %s: %w",
		h.dest.Name, h.vol.Name, h.vol.Name, h.dest.Name, h.vol.Name, volmark.MarkerName, ErrRefused)
}

func (h *mirrorHandler) foreignHistory(runID int64) error {
	return fmt.Errorf("destination %q: the last successful sync (run %d) left no receipt at %s, so the root holds a tree this mirror did not write (by rclone, or another layout) or was emptied while squirrel still records uploads to it; a native mirror does not adopt an existing tree: point the destination at a fresh or emptied root, and after emptying it run `squirrel destination reset %s`: %w", h.dest.Name, runID, h.receiptName(runID), h.dest.Name, ErrRefused)
}

// seal writes the run's receipt, the manifest segment of its delta, and
// confirms it landed at its size.
func (h *mirrorHandler) seal(ctx context.Context, _ *Report, runID int64, p pushPlan, _ *mirrorOps) error {
	body, err := encodeManifestSegment(p.delta)
	if err != nil {
		return err
	}
	tr, err := h.root(ctx, runID)
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

// advanceMethod is fingerprint-verified once every present content of the
// volume has a copy on the destination whose read-back confirmed its
// BLAKE3 (a local disk), and presence+size otherwise: the push hashed
// every path's bytes as they streamed out and confirmed each landed at its
// size, and an sftp mirror stays there.
func (h *mirrorHandler) advanceMethod(ctx context.Context, _ *Report, p pushPlan) (string, error) {
	if len(p.advance) == 0 {
		return store.VerifyMethodPresenceSize, nil
	}
	pending, err := h.store.CountVolumeContentsPendingFingerprint(ctx, p.volumeID, h.dest.Name)
	if err != nil {
		return "", err
	}
	if pending == 0 {
		return store.VerifyMethodFingerprint, nil
	}
	return store.VerifyMethodPresenceSize, nil
}

// shelf is the mirror's <volume>/.squirrel-index/, reached through runID's
// guarded transport.
func (h *mirrorHandler) shelf(runID int64) snapshotShelf {
	return transportShelf{
		open:   func(ctx context.Context) (transport, error) { return h.root(ctx, runID) },
		volume: h.vol.Name,
		runID:  runID,
	}
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

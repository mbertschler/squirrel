package sync

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/runevents"
	"github.com/mbertschler/squirrel/store"
)

// mirrorWriter carries one run's execution: the guarded transport, the
// live records it keeps current, and the tally for the report.
type mirrorWriter struct {
	h     *mirrorHandler
	rep   *Report
	runID int64
	tr    transport
	live  map[string]store.RemotePath

	progress func(runevents.Progress)
	total    int
	done     int
	drifted  int
}

// execute confirms or writes every planned path. A failure on one path
// is counted and the rest still run — every version that lands now is
// recorded and saves work on the retry — but any failure fails the run
// before its receipt is written, so the watermark holds.
func (h *mirrorHandler) execute(ctx context.Context, rep *Report, runID int64, ops *mirrorOps) error {
	tr, err := h.root(ctx, runID)
	if err != nil {
		return err
	}
	w := &mirrorWriter{h: h, rep: rep, runID: runID, tr: tr, live: ops.live, progress: h.progress, total: len(ops.paths)}
	rep.AlreadyCorrect = ops.inSync
	for _, p := range ops.paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.apply(ctx, p); err != nil {
			w.fail(p.delta.Path, err)
			if errors.Is(err, errTransportStalled) {
				break
			}
		}
		w.done++
		w.emitProgress()
	}
	return w.result()
}

func (w *mirrorWriter) apply(ctx context.Context, p mirrorPath) error {
	if p.current {
		ok, err := w.confirm(ctx, p.delta)
		if err != nil || ok {
			return err
		}
	}
	return w.write(ctx, p.delta)
}

// confirm checks an already-correct path with one Lstat. A path whose size
// or mtime disagrees with its record was changed behind squirrel's back:
// the record becomes lost, and the path is written again.
func (w *mirrorWriter) confirm(ctx context.Context, d store.PathDelta) (bool, error) {
	live := w.live[d.Path]
	e, err := w.tr.Stat(ctx, w.h.liveName(d.Path))
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, errParentNotDir) {
		return false, err
	}
	if err == nil && matchesRecord(e, live) {
		w.rep.RcloneResult.Checked++
		return true, nil
	}
	w.rep.AlreadyCorrect--
	if err := w.loseRecord(ctx, live, "no longer matches its record"); err != nil {
		return false, err
	}
	return false, nil
}

// write lands one path: stage the content while hashing it, clear the way
// (displacing whatever the path and its parents hold), then commit.
func (w *mirrorWriter) write(ctx context.Context, d store.PathDelta) error {
	staged, mtime, err := w.stage(ctx, d)
	if err != nil {
		return err
	}
	if err := w.clearParents(ctx, d.Path); err != nil {
		return err
	}
	if err := w.displace(ctx, d.Path); err != nil {
		return err
	}
	return w.commit(ctx, d, staged, mtime)
}

// stage streams the source into this run's staging and hashes the same
// bytes. A source that drifted from its indexed hash fails the path with
// errContentDrift; its staged file stays until the next push's reconcile
// removes it with this run's staging.
func (w *mirrorWriter) stage(ctx context.Context, d store.PathDelta) (string, time.Time, error) {
	src := filepath.Join(w.h.vol.Path, filepath.FromSlash(d.Path))
	f, err := os.Open(src)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("open source %s: %w", src, err)
	}
	defer f.Close()
	staged := stagingName(w.h.vol.Name, w.runID, stagingKey(d.Path))
	hasher := blake3.New()
	counted := &countWriter{w: hasher}
	if err := w.tr.Put(ctx, staged, io.TeeReader(f, counted), time.Unix(0, d.MtimeNs)); err != nil {
		return "", time.Time{}, fmt.Errorf("stage %s: %w", d.Path, err)
	}
	if counted.n != d.SizeBytes || !bytes.Equal(hasher.Sum(nil), d.Blake3) {
		return "", time.Time{}, fmt.Errorf("%w: %s sent %d bytes hashing to %s, indexed as %d bytes %s — run `squirrel index %s` and sync again",
			errContentDrift, d.Path, counted.n, hex.EncodeToString(hasher.Sum(nil)), d.SizeBytes, hex.EncodeToString(d.Blake3), w.h.vol.Name)
	}
	e, err := w.tr.Stat(ctx, staged)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("confirm staged %s: %w", d.Path, err)
	}
	if e.size != d.SizeBytes {
		return "", time.Time{}, fmt.Errorf("staged %s landed with size %d, want %d", d.Path, e.size, d.SizeBytes)
	}
	return staged, e.mtime, nil
}

// clearParents displaces the first parent of rel that is not a directory
// — a file, a symlink — so rel's directory can be created.
func (w *mirrorWriter) clearParents(ctx context.Context, rel string) error {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		e, err := w.tr.Stat(ctx, w.h.liveName(parent))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if e.kind != kindDir {
			return w.displace(ctx, parent)
		}
	}
	return nil
}

// displace moves whatever rel holds into this run's history. A version
// whose size and mtime match its live record moves as that record; a
// directory moves with every live record under it; anything else moves as
// unrecorded bytes, and a record they contradict becomes lost, so a
// record in history always vouches for the bytes there.
func (w *mirrorWriter) displace(ctx context.Context, rel string) error {
	e, err := w.tr.Stat(ctx, w.h.liveName(rel))
	if errors.Is(err, fs.ErrNotExist) {
		return w.loseRecordAt(ctx, rel, "is gone from the destination")
	}
	if err != nil {
		return err
	}
	if live, ok := w.live[rel]; ok {
		if matchesRecord(e, live) {
			return w.moveRecorded(ctx, rel, []int64{live.ID})
		}
		if err := w.loseRecord(ctx, live, "no longer matches its record"); err != nil {
			return err
		}
	}
	if e.kind == kindDir {
		return w.displaceDir(ctx, rel)
	}
	return w.moveUnrecorded(ctx, rel, e)
}

// displaceDir moves a directory a file replaced, with every live record
// under it; a record whose bytes changed becomes lost first.
func (w *mirrorWriter) displaceDir(ctx context.Context, dir string) error {
	var ids []int64
	for rel, live := range w.live {
		if !strings.HasPrefix(rel, dir+"/") {
			continue
		}
		e, err := w.tr.Stat(ctx, w.h.liveName(rel))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err == nil && matchesRecord(e, live) {
			ids = append(ids, live.ID)
			continue
		}
		if err := w.loseRecord(ctx, live, "no longer matches its record"); err != nil {
			return err
		}
	}
	if err := w.noteUnrecordedMove(ctx, fmt.Sprintf("directory=%q recorded-versions=%d moving-to=%q", dir, len(ids), w.historyName(dir))); err != nil {
		return err
	}
	if err := w.moveRecorded(ctx, dir, ids); err != nil {
		return err
	}
	w.rep.Warnings = append(w.rep.Warnings, fmt.Sprintf("destination %q: directory %s was replaced by a file; it moved into %s with the %d version(s) squirrel recorded in it and anything else it held",
		w.h.dest.Name, dir, w.historyName(dir), len(ids)))
	return nil
}

// moveRecorded records the move of ids, renames rel into history, and
// confirms. If the rename fails the rows stay displacing, and the next
// push's reconcile settles them from where the bytes are.
func (w *mirrorWriter) moveRecorded(ctx context.Context, rel string, ids []int64) error {
	if err := w.h.store.BeginRemotePathsDisplace(ctx, w.runID, ids...); err != nil {
		return err
	}
	if err := w.tr.Rename(ctx, w.h.liveName(rel), w.historyName(rel)); err != nil {
		return fmt.Errorf("displace %s: %w", rel, err)
	}
	if err := w.h.store.ConfirmRemotePathsDisplaced(ctx, ids...); err != nil {
		return err
	}
	for r := range w.live {
		if r == rel || strings.HasPrefix(r, rel+"/") {
			delete(w.live, r)
		}
	}
	return nil
}

// moveUnrecorded moves bytes squirrel did not write into history. The
// move is noted in the run's audit trail before it happens, and reported
// as a warning once it did.
func (w *mirrorWriter) moveUnrecorded(ctx context.Context, rel string, e entry) error {
	to := w.historyName(rel)
	if err := w.noteUnrecordedMove(ctx, fmt.Sprintf("path=%q size=%d moving-to=%q", rel, e.size, to)); err != nil {
		return err
	}
	if err := w.tr.Rename(ctx, w.h.liveName(rel), to); err != nil {
		return fmt.Errorf("displace unrecorded %s: %w", rel, err)
	}
	w.rep.Warnings = append(w.rep.Warnings, fmt.Sprintf("destination %q: %s held bytes squirrel did not write (%d bytes); they are preserved at %s", w.h.dest.Name, rel, e.size, to))
	return nil
}

func (w *mirrorWriter) noteUnrecordedMove(ctx context.Context, note string) error {
	return w.h.store.AppendRunAudit(ctx, store.RunAuditEntry{
		RunID:      w.runID,
		Transition: store.TransitionDisplaceUnrecorded,
		Note:       note,
	})
}

// commit records the intent, renames the staged version onto rel, and
// confirms. If the rename fails the row stays committing, and the next
// push's reconcile withdraws it.
func (w *mirrorWriter) commit(ctx context.Context, d store.PathDelta, staged string, mtime time.Time) error {
	id, err := w.h.store.BeginRemotePathCommit(ctx, store.RemotePathWrite{
		Destination: w.h.dest.Name,
		FolderID:    d.FolderID,
		Name:        path.Base(d.Path),
		ContentID:   d.ContentID,
		RunID:       w.runID,
		MtimeNs:     mtime.UnixNano(),
	})
	if err != nil {
		return err
	}
	if err := w.tr.Rename(ctx, staged, w.h.liveName(d.Path)); err != nil {
		return fmt.Errorf("commit %s: %w", d.Path, err)
	}
	if err := w.h.store.ConfirmRemotePathsLive(ctx, id); err != nil {
		return err
	}
	w.live[d.Path] = store.RemotePath{ID: id, ContentID: d.ContentID, WrittenRunID: w.runID, State: store.RemotePathLive,
		MtimeNs: mtime.UnixNano(), Path: d.Path, SizeBytes: d.SizeBytes}
	w.rep.RcloneResult.Transferred++
	w.rep.RcloneResult.Bytes += d.SizeBytes
	return nil
}

// loseRecordAt marks the live record at rel lost, if there is one.
func (w *mirrorWriter) loseRecordAt(ctx context.Context, rel, why string) error {
	if live, ok := w.live[rel]; ok {
		return w.loseRecord(ctx, live, why)
	}
	return nil
}

func (w *mirrorWriter) loseRecord(ctx context.Context, live store.RemotePath, why string) error {
	if err := w.h.store.MarkRemotePathsLost(ctx, live.ID); err != nil {
		return err
	}
	delete(w.live, live.Path)
	w.rep.Warnings = append(w.rep.Warnings, fmt.Sprintf("destination %q: %s %s — it was changed behind squirrel's back, so it is written again", w.h.dest.Name, live.Path, why))
	return nil
}

func (w *mirrorWriter) historyName(rel string) string {
	return historyName(w.h.vol.Name, w.runID, rel)
}

func (w *mirrorWriter) fail(rel string, err error) {
	if errors.Is(err, errContentDrift) {
		w.drifted++
		w.rep.Warnings = append(w.rep.Warnings, err.Error())
		return
	}
	w.rep.RcloneResult.Errors++
	if int64(len(w.rep.RcloneResult.FailedFiles)) < maxFailedFiles {
		w.rep.RcloneResult.FailedFiles = append(w.rep.RcloneResult.FailedFiles, FailedFile{Object: rel, Message: err.Error()})
	}
}

func (w *mirrorWriter) result() error {
	if n := w.rep.RcloneResult.Errors; n > 0 {
		return fmt.Errorf("%d path(s) failed to land on %q; the receipt for run %d was not written and the durability vector did not advance", n, w.h.dest.Name, w.runID)
	}
	if w.drifted > 0 {
		return fmt.Errorf("%d path(s) on %q were refused for drifting from their indexed hash; re-index the volume and sync again — the receipt for run %d was not written and the durability vector did not advance", w.drifted, w.h.dest.Name, w.runID)
	}
	return nil
}

func (w *mirrorWriter) emitProgress() {
	if w.progress == nil {
		return
	}
	w.progress(runevents.Progress{
		Stage:     runevents.StageUploading,
		Done:      int64(w.done),
		Total:     int64(w.total),
		BytesDone: w.rep.RcloneResult.Bytes,
	})
}

// matchesRecord reports whether an entry found at a path is the version
// its live record describes, by kind, size and mtime.
func matchesRecord(e entry, live store.RemotePath) bool {
	return e.kind == kindFile && e.size == live.SizeBytes && e.mtime.UnixNano() == live.MtimeNs
}

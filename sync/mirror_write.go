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
	gosync "sync"
	"time"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/runevents"
	"github.com/mbertschler/squirrel/store"
)

// mirrorWriter carries one run's execution: the guarded transport, the
// live records it keeps current, and the tally for the report. Several
// paths are in flight at once, so the records and the tally are shared
// behind mu, which is never held across a transport call.
type mirrorWriter struct {
	h        *mirrorHandler
	rep      *Report
	runID    int64
	volumeID int64
	tr       transport
	// fold is how the destination compares names; names plans the
	// writes around the collisions its folding causes.
	fold  nameFolding
	names foldPlan
	// concurrency is how many paths are in flight at once.
	concurrency int

	progress func(runevents.Progress)
	total    int

	mu   gosync.Mutex
	live map[string]store.RemotePath
	// displacing is the rows this batch moved into history, which the
	// batch's flush lets it record as displaced.
	displacing []int64
	done       int
	drifted    int
	collided   int
	stalled    bool
}

// execute confirms or writes every planned path, a batch at a time. A
// failure on one path is counted and the rest still run — every version
// that lands now is recorded and saves work on the retry — but any failure
// fails the run before its receipt is written, so the watermark holds.
func (h *mirrorHandler) execute(ctx context.Context, rep *Report, runID int64, ops *mirrorOps) error {
	tr, err := h.root(ctx, runID)
	if err != nil {
		return err
	}
	w := &mirrorWriter{h: h, rep: rep, runID: runID, volumeID: ops.volumeID, tr: tr, live: ops.live,
		concurrency: pushConcurrency(h.dest), progress: h.progress, total: len(ops.paths)}
	rep.AlreadyCorrect = ops.inSync
	if ops.repairs > 0 {
		rep.Changed = knownChanged(rep.Changed.Int64 + ops.repairs)
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("destination %q lost %d path(s) the index still holds there (found gone or changed); they are written again", h.dest.Name, ops.repairs))
	}
	if err := w.learnNames(ctx, ops); err != nil {
		return err
	}
	for _, batch := range batches(ops.paths, writtenBytes) {
		if err := ctx.Err(); err != nil {
			return err
		}
		w.writeBatch(ctx, batch)
		if w.isStalled() {
			break
		}
	}
	return w.result()
}

// writtenBytes is what a planned path adds to its batch: its bytes when
// it may be written, nothing when it is only confirmed.
func writtenBytes(p mirrorPath) int64 {
	if p.current {
		return 0
	}
	return p.delta.SizeBytes
}

// learnNames probes how the destination compares names before the first
// path lands, and on a destination that folds them plans around the
// collisions that causes.
func (w *mirrorWriter) learnNames(ctx context.Context, ops *mirrorOps) error {
	if len(ops.paths) == 0 {
		return nil
	}
	var err error
	if w.fold, err = w.probeFolding(ctx); err != nil || !w.fold.folds() {
		return err
	}
	w.names, err = w.planFolding(ctx, ops)
	return err
}

// landing is one planned path on its way through a batch.
type landing struct {
	p      mirrorPath
	staged stagedVersion
	// commitID is the path's committing row, once inserted.
	commitID int64
	// settled: the path needs no further step, because it was confirmed
	// in place, refused, or failed.
	settled bool
}

// writeBatch takes one batch through the steps of a write, each for the
// whole batch before the next: confirm or stage every path, flush, read
// back, clear the way, displace, flush and record the displaced versions,
// commit, flush and record the committed ones. A path whose step fails
// drops out; a stalled transport ends the batch where it is, and the next
// push's reconcile settles what it left recorded in flight.
func (w *mirrorWriter) writeBatch(ctx context.Context, batch []mirrorPath) {
	ls := make([]*landing, len(batch))
	for i, p := range batch {
		ls[i] = &landing{p: p}
	}
	steps := []func(context.Context, []*landing){
		w.each(w.prepare),
		w.flushOrDrop,
		w.readBackAll,
		w.clearTheWay,
		w.each(w.displaceLanding),
		w.recordDisplaced,
		w.each(w.commit),
		w.recordCommitted,
	}
	for _, step := range steps {
		step(ctx, ls)
		if w.isStalled() || ctx.Err() != nil {
			return
		}
	}
}

// each is the step that runs fn for every path still on its way, with
// w.concurrency of them in flight. A path fn fails drops out.
func (w *mirrorWriter) each(fn func(context.Context, *landing) error) func(context.Context, []*landing) {
	return func(ctx context.Context, ls []*landing) {
		inFlight(ctx, w.concurrency, active(ls), w.isStalled, func(l *landing) {
			if err := fn(ctx, l); err != nil {
				w.drop(l, err)
			}
		})
	}
}

// active is the batch's paths still on their way.
func active(ls []*landing) []*landing {
	var out []*landing
	for _, l := range ls {
		if !l.settled {
			out = append(out, l)
		}
	}
	return out
}

// prepare refuses a path whose name collides with another's, confirms an
// already-correct one in place, and stages every other.
func (w *mirrorWriter) prepare(ctx context.Context, l *landing) error {
	rel := l.p.delta.Path
	if other, ok := w.names.refused[rel]; ok {
		w.refuseCollision(rel, other)
		w.settle(l)
		return nil
	}
	if l.p.current {
		ok, err := w.confirm(ctx, l.p.delta)
		if err != nil {
			return err
		}
		if ok {
			w.settle(l)
			return nil
		}
	}
	staged, err := w.stage(ctx, l.p.delta)
	l.staged = staged
	return err
}

// flushOrDrop makes the batch's staged files durable; a flush that fails
// drops every path still on its way.
func (w *mirrorWriter) flushOrDrop(ctx context.Context, ls []*landing) {
	if len(active(ls)) == 0 {
		return
	}
	if err := w.tr.Flush(ctx); err != nil {
		w.dropAll(ls, fmt.Errorf("flush the staged files: %w", err))
	}
}

// readBackAll reads every staged file back on a disk squirrel reads back.
func (w *mirrorWriter) readBackAll(ctx context.Context, ls []*landing) {
	if !readsBack(w.h.dest) {
		return
	}
	w.each(func(ctx context.Context, l *landing) error {
		return w.readBackStaged(ctx, l.p.delta, &l.staged)
	})(ctx, ls)
}

// clearTheWay moves aside, one path after another, what stands in the
// way of more than one path's name: versions recorded under another
// spelling of it, and a parent that is not a directory. Each directory is
// looked at once per batch.
func (w *mirrorWriter) clearTheWay(ctx context.Context, ls []*landing) {
	cleared := map[string]bool{}
	for _, l := range active(ls) {
		if w.isStalled() {
			return
		}
		if err := w.clearFor(ctx, l.p.delta.Path, cleared); err != nil {
			w.drop(l, err)
		}
	}
}

func (w *mirrorWriter) clearFor(ctx context.Context, rel string, cleared map[string]bool) error {
	for _, other := range w.names.displace[rel] {
		if err := w.displaceSpelling(ctx, other); err != nil {
			return err
		}
	}
	return w.clearParents(ctx, rel, cleared)
}

func (w *mirrorWriter) displaceLanding(ctx context.Context, l *landing) error {
	return w.displace(ctx, l.p.delta.Path)
}

// recordDisplaced flushes the batch's moves into history, then records
// them as displaced. Until then their rows stay displacing, which the
// next push's reconcile settles from where the bytes are.
func (w *mirrorWriter) recordDisplaced(ctx context.Context, ls []*landing) {
	w.mu.Lock()
	ids := w.displacing
	w.displacing = nil
	w.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	err := w.tr.Flush(ctx)
	if err == nil {
		err = w.h.store.ConfirmRemotePathsDisplaced(ctx, ids...)
	}
	if err != nil {
		w.dropAll(ls, fmt.Errorf("record the versions moved into history: %w", err))
	}
}

// recordCommitted flushes the batch's commits, then records them live
// and counts them. Until then their rows stay committing, which the next
// push's reconcile settles from where the bytes are.
func (w *mirrorWriter) recordCommitted(ctx context.Context, ls []*landing) {
	var ids []int64
	for _, l := range active(ls) {
		ids = append(ids, l.commitID)
	}
	if len(ids) == 0 {
		return
	}
	err := w.tr.Flush(ctx)
	if err == nil {
		err = w.h.store.ConfirmRemotePathsLive(ctx, ids...)
	}
	if err != nil {
		w.dropAll(ls, fmt.Errorf("record the committed paths: %w", err))
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, l := range active(ls) {
		w.rep.RcloneResult.Transferred++
		w.rep.RcloneResult.Bytes += l.p.delta.SizeBytes
		w.settleLocked(l)
	}
}

func (w *mirrorWriter) settle(l *landing) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.settleLocked(l)
}

// settleLocked takes l off its way, counts it done and reports progress;
// w.mu is held, so progress arrives in order.
func (w *mirrorWriter) settleLocked(l *landing) {
	l.settled = true
	w.done++
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

// confirm checks an already-correct path with one Lstat. A path whose size
// or mtime disagrees with its record was changed behind squirrel's back:
// the record becomes lost, and the path is written again.
func (w *mirrorWriter) confirm(ctx context.Context, d store.PathDelta) (bool, error) {
	live, _ := w.liveAt(d.Path)
	e, err := w.tr.Stat(ctx, w.h.liveName(d.Path))
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, errParentNotDir) {
		return false, err
	}
	if err == nil && matchesRecord(e, live) {
		w.mu.Lock()
		w.rep.RcloneResult.Checked++
		w.mu.Unlock()
		return true, nil
	}
	w.mu.Lock()
	w.rep.AlreadyCorrect--
	w.mu.Unlock()
	if err := w.loseRecord(ctx, live, "no longer matches its record"); err != nil {
		return false, err
	}
	return false, nil
}

// stagedVersion is one path's version in this run's staging, as the
// destination confirmed it: its mtime, and on a disk squirrel reads back,
// the hex BLAKE3 the read-back confirmed and when.
type stagedVersion struct {
	name         string
	mtime        time.Time
	checksum     string
	verifiedAtNs int64
}

// stage streams the source into this run's staging and hashes the same
// bytes, then confirms what landed: its size. A source that drifted from
// its indexed hash fails the path with errContentDrift; its staged file
// stays until the next push's reconcile removes it with this run's
// staging.
func (w *mirrorWriter) stage(ctx context.Context, d store.PathDelta) (stagedVersion, error) {
	src := filepath.Join(w.h.vol.Path, filepath.FromSlash(d.Path))
	f, err := os.Open(src)
	if err != nil {
		return stagedVersion{}, fmt.Errorf("open source %s: %w", src, err)
	}
	defer f.Close()
	staged := stagedVersion{name: stagingName(w.h.vol.Name, w.runID, stagingKey(d.Path))}
	hasher := blake3.New()
	counted := &countWriter{w: hasher}
	if err := w.tr.Put(ctx, staged.name, io.TeeReader(f, counted), time.Unix(0, d.MtimeNs)); err != nil {
		return stagedVersion{}, fmt.Errorf("stage %s: %w", d.Path, err)
	}
	if counted.n != d.SizeBytes || !bytes.Equal(hasher.Sum(nil), d.Blake3) {
		return stagedVersion{}, fmt.Errorf("%w: %s sent %d bytes hashing to %s, indexed as %d bytes %s — run `squirrel index %s` and sync again",
			errContentDrift, d.Path, counted.n, hex.EncodeToString(hasher.Sum(nil)), d.SizeBytes, hex.EncodeToString(d.Blake3), w.h.vol.Name)
	}
	e, err := w.tr.Stat(ctx, staged.name)
	if err != nil {
		return stagedVersion{}, fmt.Errorf("confirm staged %s: %w", d.Path, err)
	}
	if e.size != d.SizeBytes {
		return stagedVersion{}, fmt.Errorf("staged %s landed with size %d, want %d", d.Path, e.size, d.SizeBytes)
	}
	staged.mtime = e.mtime
	return staged, nil
}

// readBackStaged confirms the staged bytes, flushed, are the content
// squirrel sent, and records the fingerprint that earns.
func (w *mirrorWriter) readBackStaged(ctx context.Context, d store.PathDelta, staged *stagedVersion) error {
	sum, err := readBack(ctx, w.tr, staged.name)
	if err != nil {
		return fmt.Errorf("read back staged %s: %w", d.Path, err)
	}
	if !bytes.Equal(sum, d.Blake3) {
		return fmt.Errorf("%w: staged %s hashes to %s, sent %s", errReadBackMismatch, d.Path, hex.EncodeToString(sum), hex.EncodeToString(d.Blake3))
	}
	staged.checksum = hex.EncodeToString(sum)
	staged.verifiedAtNs = store.NowNs()
	w.mu.Lock()
	w.rep.Fingerprints++
	w.mu.Unlock()
	return nil
}

// clearParents displaces the first parent of rel that is not a directory
// — a file, a symlink — so rel's directory can be created. cleared holds
// what this batch already found at each parent: a directory (true) or
// nothing (false), below which nothing exists either.
func (w *mirrorWriter) clearParents(ctx context.Context, rel string, cleared map[string]bool) error {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		if isDir, seen := cleared[parent]; seen {
			if !isDir {
				return nil
			}
			continue
		}
		e, err := w.tr.Stat(ctx, w.h.liveName(parent))
		if errors.Is(err, fs.ErrNotExist) {
			cleared[parent] = false
			return nil
		}
		if err != nil {
			return err
		}
		if e.kind == kindDir {
			cleared[parent] = true
			continue
		}
		if err := w.displace(ctx, parent); err != nil {
			return err
		}
		cleared[parent] = false
		return nil
	}
	return nil
}

// displace moves whatever rel holds into this run's history. A version
// whose size and mtime match its live record moves as that record; a
// directory moves with every live record under it; anything else moves as
// unrecorded bytes, and a record they contradict becomes lost, so a
// record in history always vouches for the bytes there. Where rel holds
// no directory, every record under it is lost too.
func (w *mirrorWriter) displace(ctx context.Context, rel string) error {
	e, err := w.tr.Stat(ctx, w.h.liveName(rel))
	if errors.Is(err, fs.ErrNotExist) {
		if err := w.loseRecordsUnder(ctx, rel); err != nil {
			return err
		}
		return w.loseRecordAt(ctx, rel, "is gone from the destination")
	}
	if err != nil {
		return err
	}
	if e.kind != kindDir {
		if err := w.loseRecordsUnder(ctx, rel); err != nil {
			return err
		}
	}
	if live, ok := w.liveAt(rel); ok {
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
	for _, live := range w.liveUnder(dir) {
		e, err := w.tr.Stat(ctx, w.h.liveName(live.Path))
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
	w.warn(fmt.Sprintf("destination %q: directory %s was replaced by a file; it moved into %s with the %d version(s) squirrel recorded in it and anything else it held",
		w.h.dest.Name, dir, w.historyName(dir), len(ids)))
	return nil
}

// moveRecorded records the move of ids, renames rel into history, and
// leaves the rows for the batch's flush to confirm. If the rename fails,
// or the push ends before that flush, the rows stay displacing, and the
// next push's reconcile settles them from where the bytes are.
func (w *mirrorWriter) moveRecorded(ctx context.Context, rel string, ids []int64) error {
	if err := w.h.store.BeginRemotePathsDisplace(ctx, w.runID, ids...); err != nil {
		return err
	}
	if err := w.tr.Rename(ctx, w.h.liveName(rel), w.historyName(rel)); err != nil {
		return fmt.Errorf("displace %s: %w", rel, err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.displacing = append(w.displacing, ids...)
	for r := range w.live {
		if r == rel || w.fold.under(r, rel) {
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
	w.warn(fmt.Sprintf("destination %q: %s held bytes squirrel did not write (%d bytes); they are preserved at %s", w.h.dest.Name, rel, e.size, to))
	return nil
}

func (w *mirrorWriter) noteUnrecordedMove(ctx context.Context, note string) error {
	return w.h.store.AppendRunAudit(ctx, store.RunAuditEntry{
		RunID:      w.runID,
		Transition: store.TransitionDisplaceUnrecorded,
		Note:       note,
	})
}

// commit records the intent and renames the staged version onto its
// path, leaving the row for the batch's flush to confirm. If the rename
// fails, or the push ends before that flush, the row stays committing,
// and the next push's reconcile settles it.
func (w *mirrorWriter) commit(ctx context.Context, l *landing) error {
	d, staged := l.p.delta, l.staged
	id, err := w.h.store.BeginRemotePathCommit(ctx, store.RemotePathWrite{
		Destination:  w.h.dest.Name,
		FolderID:     d.FolderID,
		Name:         path.Base(d.Path),
		ContentID:    d.ContentID,
		RunID:        w.runID,
		MtimeNs:      staged.mtime.UnixNano(),
		Checksum:     staged.checksum,
		VerifiedAtNs: staged.verifiedAtNs,
	})
	if err != nil {
		return err
	}
	if err := w.tr.Rename(ctx, staged.name, w.h.liveName(d.Path)); err != nil {
		return fmt.Errorf("commit %s: %w", d.Path, err)
	}
	l.commitID = id
	w.mu.Lock()
	w.live[d.Path] = store.RemotePath{ID: id, ContentID: d.ContentID, WrittenRunID: w.runID, State: store.RemotePathLive,
		MtimeNs: staged.mtime.UnixNano(), Path: d.Path, SizeBytes: d.SizeBytes}
	w.mu.Unlock()
	return nil
}

// loseRecordsUnder marks lost every live record below rel, whose
// directory the destination no longer holds.
func (w *mirrorWriter) loseRecordsUnder(ctx context.Context, rel string) error {
	for _, live := range w.liveUnder(rel) {
		if err := w.loseRecord(ctx, live, "is gone: "+rel+" is not a directory on the destination"); err != nil {
			return err
		}
	}
	return nil
}

// loseRecordAt marks the live record at rel lost, if there is one.
func (w *mirrorWriter) loseRecordAt(ctx context.Context, rel, why string) error {
	if live, ok := w.liveAt(rel); ok {
		return w.loseRecord(ctx, live, why)
	}
	return nil
}

func (w *mirrorWriter) loseRecord(ctx context.Context, live store.RemotePath, why string) error {
	if err := w.h.store.MarkRemotePathsLost(ctx, live.ID); err != nil {
		return err
	}
	w.mu.Lock()
	delete(w.live, live.Path)
	w.mu.Unlock()
	w.warn(fmt.Sprintf("destination %q: %s %s — it was changed behind squirrel's back, so its record no longer counts as a copy, and a push writes it again while the index holds it", w.h.dest.Name, live.Path, why))
	return nil
}

// liveAt is the live record at rel, if there is one.
func (w *mirrorWriter) liveAt(rel string) (store.RemotePath, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	live, ok := w.live[rel]
	return live, ok
}

// liveUnder is every live record below dir, as the destination resolves
// names.
func (w *mirrorWriter) liveUnder(dir string) []store.RemotePath {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []store.RemotePath
	for rel, live := range w.live {
		if w.fold.under(rel, dir) {
			out = append(out, live)
		}
	}
	return out
}

func (w *mirrorWriter) warn(msg string) {
	w.mu.Lock()
	w.rep.Warnings = append(w.rep.Warnings, msg)
	w.mu.Unlock()
}

func (w *mirrorWriter) historyName(rel string) string {
	return historyName(w.h.vol.Name, w.runID, rel)
}

// drop takes l out of its batch, counting why.
func (w *mirrorWriter) drop(l *landing, err error) {
	rel := l.p.delta.Path
	w.mu.Lock()
	defer w.mu.Unlock()
	defer w.settleLocked(l)
	switch {
	case errors.Is(err, errContentDrift):
		w.drifted++
		w.rep.Warnings = append(w.rep.Warnings, err.Error())
		return
	case errors.Is(err, errTransportStalled):
		w.stalled = true
	}
	w.rep.RcloneResult.Errors++
	if int64(len(w.rep.RcloneResult.FailedFiles)) < maxFailedFiles {
		w.rep.RcloneResult.FailedFiles = append(w.rep.RcloneResult.FailedFiles, FailedFile{Object: rel, Message: err.Error()})
	}
}

// dropAll takes every path still on its way out of the batch.
func (w *mirrorWriter) dropAll(ls []*landing, err error) {
	for _, l := range active(ls) {
		w.drop(l, err)
	}
}

func (w *mirrorWriter) isStalled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stalled
}

func (w *mirrorWriter) result() error {
	if n := w.rep.RcloneResult.Errors; n > 0 {
		return fmt.Errorf("%d path(s) failed to land on %q; the receipt for run %d was not written and the durability vector did not advance", n, w.h.dest.Name, w.runID)
	}
	if w.drifted > 0 {
		return fmt.Errorf("%d path(s) on %q were refused for drifting from their indexed hash; re-index the volume and sync again — the receipt for run %d was not written and the durability vector did not advance", w.drifted, w.h.dest.Name, w.runID)
	}
	if w.collided > 0 {
		return fmt.Errorf("%d path(s) on %q were refused because the destination cannot tell their names apart from another path's; rename one of each pair at the source, index, and sync again — the receipt for run %d was not written and the durability vector did not advance", w.collided, w.h.dest.Name, w.runID)
	}
	return nil
}

// matchesRecord reports whether an entry found at a path is the version
// its live record describes, by kind, size and mtime.
func matchesRecord(e entry, live store.RemotePath) bool {
	return e.kind == kindFile && e.size == live.SizeBytes && e.mtime.UnixNano() == live.MtimeNs
}

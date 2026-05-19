package index

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/store"
)

type Options struct {
	// Name pins the volume name in the DB so it matches what config
	// declares. When empty, the volume name is derived from the path
	// basename (the historical, pre-config behaviour). New callers should
	// always set it; legacy callers and tests may leave it blank.
	Name string
	// Shallow: skip rehashing if (size, mtime) match the stored row.
	Shallow bool
	// DryRun: do not write to the database; only compute and report.
	DryRun bool
	// Workers: number of hashing goroutines. 0 means runtime.NumCPU().
	Workers int
	// QueueDepth: maximum pending entries between walker and workers. 0 means 4 * Workers.
	QueueDepth int
	// Kind selects the runs.kind label written for this invocation.
	// Empty defaults to store.RunKindIndex. The agent's drift-detection
	// scheduler (#17) sets store.RunKindAudit so out-of-band re-walks
	// are distinguishable from regular indexing in `squirrel runs`.
	Kind string
}

type Report struct {
	Added     int
	Modified  int
	Unchanged int
	Missing   int
	// Errors is the count of per-file errors encountered during the walk.
	Errors int
	// ErrorList contains the actual errors. Callers (CLI, tests) decide how
	// to surface them; this package never writes to os.Stderr directly.
	ErrorList []error
	// RunID is the runs.id this invocation wrote to. Zero in DryRun mode
	// (which never inserts a row). Exposed so audit callers can correlate
	// the report with a runs row without a follow-up lookup.
	RunID int64
}

type changeKind int

const (
	kindUnchanged changeKind = iota
	kindAdded
	kindModified
)

type workItem struct {
	absPath   string
	relPath   string
	sizeBytes int64
	mtimeNs   int64
}

type resultItem struct {
	row  store.FileRow
	kind changeKind
	err  error
}

// Index walks root, hashes regular files, and updates s. Paths are stored
// relative to root; the volume for root is resolved (or created) up front and
// then referenced by ID on every row. Each non-dry-run invocation creates a
// row in the runs table that is finalised to 'success' / 'partial' / 'failed'
// before Index returns.
func Index(ctx context.Context, s *store.Store, root string, opts Options) (report Report, err error) {
	idx, err := newIndexer(ctx, s, root, opts)
	if err != nil {
		return Report{}, err
	}
	if err := idx.beginRun(); err != nil {
		return Report{}, err
	}
	defer func() { idx.finishRun(&report, err) }()

	idx.startWorkers()
	idx.startWalker()

	if report, err = idx.collect(); err != nil {
		return report, err
	}
	if err = idx.waitForWalker(); err != nil {
		return report, err
	}
	if err = idx.finalizeMissing(&report); err != nil {
		return report, err
	}
	return report, nil
}

// indexer holds the state of one Index() invocation. It owns the worker pool,
// the walker goroutine, and the channels connecting them.
type indexer struct {
	ctx          context.Context
	store        *store.Store
	absRoot      string
	volumeID     int64
	volumeExists bool
	opts         Options
	startedAtNs  int64
	// runID is the row in the runs table for this invocation. Zero in DryRun
	// mode; non-dry-run callers must always have a valid run.
	runID int64

	workers int
	work    chan workItem
	results chan resultItem

	workerWG  sync.WaitGroup
	walkErrCh chan error

	// seen is populated only in DryRun mode; finalizeMissing uses it to count
	// rows that exist in the DB but were not encountered during the walk.
	seen map[string]struct{}

	// preloaded is the per-volume snapshot of live rows, loaded once at the
	// start of the run. Workers consult it instead of calling GetByPath per
	// file, which would funnel every lookup through the single shared sqlite
	// connection. nil for fresh volumes (no rows to load) and for DryRun
	// runs against never-indexed paths.
	preloaded map[string]store.FileRow
}

func newIndexer(ctx context.Context, s *store.Store, root string, opts Options) (*indexer, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("stat root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("root %q is not a directory", absRoot)
	}

	workers := opts.Workers
	if workers <= 0 {
		// Each worker spends roughly half its wall time blocked on file
		// I/O and half hashing on CPU, so NumCPU workers leave NumCPU/2
		// cores idle on average. Doubling the count keeps the cores busy
		// (the hash phases interleave across workers) and increases the
		// NVMe in-flight read count, which APFS rewards.
		workers = 2 * runtime.NumCPU()
	}
	queueDepth := opts.QueueDepth
	if queueDepth <= 0 {
		queueDepth = workers * 4
	}

	vol, exists, err := resolveVolume(ctx, s, opts.Name, absRoot, opts.DryRun)
	if err != nil {
		return nil, err
	}

	idx := &indexer{
		ctx:          ctx,
		store:        s,
		absRoot:      absRoot,
		volumeID:     vol.ID,
		volumeExists: exists,
		opts:         opts,
		startedAtNs:  store.NowNs(),
		workers:      workers,
		work:         make(chan workItem, queueDepth),
		results:      make(chan resultItem, queueDepth),
		walkErrCh:    make(chan error, 1),
	}
	if opts.DryRun {
		idx.seen = make(map[string]struct{})
	}
	if exists {
		preloaded, err := s.LoadVolumeIndex(ctx, vol.ID)
		if err != nil {
			return nil, fmt.Errorf("preload volume index: %w", err)
		}
		idx.preloaded = preloaded
	}
	return idx, nil
}

// beginRun records the start of this index run in the store, unless the
// indexer is operating in DryRun mode (which never touches the database).
// The resulting run id is what every per-row write is tagged with.
func (i *indexer) beginRun() error {
	if i.opts.DryRun {
		return nil
	}
	kind := i.opts.Kind
	if kind == "" {
		kind = store.RunKindIndex
	}
	id, err := i.store.BeginRun(i.ctx, kind, i.volumeID, "")
	if err != nil {
		return fmt.Errorf("begin run: %w", err)
	}
	i.runID = id
	return nil
}

// finishRun closes out the runs row for this invocation. It is a no-op in
// DryRun mode. The terminal status is derived from the fatal error (if any)
// and the per-file error count: a fatal failure yields 'failed', per-file
// errors with a clean walk yield 'partial', and an entirely clean run yields
// 'success'.
func (i *indexer) finishRun(report *Report, fatalErr error) {
	report.RunID = i.runID
	if i.opts.DryRun || i.runID == 0 {
		return
	}
	status, errMsg := runStatus(report, fatalErr)
	fileCount := int64(report.Added + report.Modified + report.Unchanged)
	if err := i.store.FinishRun(i.ctx, i.runID, status, errMsg, fileCount); err != nil {
		// Surface as a per-run error rather than swallowing silently. The
		// outer caller has already accepted a report and a fatal error (if
		// any); we can only append.
		report.Errors++
		report.ErrorList = append(report.ErrorList, fmt.Errorf("finish run %d: %w", i.runID, err))
	}
}

func runStatus(report *Report, fatalErr error) (string, string) {
	if fatalErr != nil {
		return store.RunStatusFailed, fatalErr.Error()
	}
	if report.Errors > 0 {
		return store.RunStatusPartial, ""
	}
	return store.RunStatusSuccess, ""
}

func (i *indexer) startWorkers() {
	for n := 0; n < i.workers; n++ {
		i.workerWG.Add(1)
		go i.worker()
	}
	go func() {
		i.workerWG.Wait()
		close(i.results)
	}()
}

func (i *indexer) worker() {
	defer i.workerWG.Done()
	// One scratch buffer per worker, reused for every file this worker
	// hashes. Allocating it per-file (67 k+ × 1 MiB) created enough GC
	// pressure to outweigh the syscall savings — see benchmarks.md.test
	// step 4 vs step 4b on the indexing-performance-improvements branch.
	buf := make([]byte, hashReadBufferSize)
	for w := range i.work {
		i.results <- i.process(w, buf)
	}
}

func (i *indexer) startWalker() {
	go func() {
		defer close(i.work)
		i.walkErrCh <- filepath.WalkDir(i.absRoot, i.visit)
	}()
}

// visit is the filepath.WalkDir callback. It filters entries we don't index
// and hands the rest off to the worker pool via the work channel.
func (i *indexer) visit(path string, d os.DirEntry, walkErr error) error {
	if walkErr != nil {
		i.sendErr(fmt.Errorf("walk %s: %w", path, walkErr))
		if d != nil && d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}
	if err := i.ctx.Err(); err != nil {
		return err
	}
	if !shouldIndex(d) {
		return nil
	}
	fi, err := d.Info()
	if err != nil {
		i.sendErr(fmt.Errorf("stat %s: %w", path, err))
		return nil
	}
	relPath, err := filepath.Rel(i.absRoot, path)
	if err != nil {
		i.sendErr(fmt.Errorf("rel %s: %w", path, err))
		return nil
	}
	return i.sendWork(workItem{
		absPath:   path,
		relPath:   filepath.ToSlash(relPath),
		sizeBytes: fi.Size(),
		mtimeNs:   fi.ModTime().UnixNano(),
	})
}

// shouldIndex reports whether the entry is a regular file (not a directory,
// not a symlink, not a device).
func shouldIndex(d os.DirEntry) bool {
	if d.IsDir() {
		return false
	}
	t := d.Type()
	return t&os.ModeSymlink == 0 && t.IsRegular()
}

// sendErr pushes a per-entry error into the results stream, respecting
// context cancellation so the walker can unwind cleanly.
func (i *indexer) sendErr(err error) {
	select {
	case i.results <- resultItem{err: err}:
	case <-i.ctx.Done():
	}
}

// sendWork hands a work item to the worker pool, returning ctx.Err() if the
// context is cancelled. Returning the error stops the WalkDir traversal.
func (i *indexer) sendWork(w workItem) error {
	select {
	case i.work <- w:
		return nil
	case <-i.ctx.Done():
		return i.ctx.Err()
	}
}

// batchSize is the number of result rows the collector buffers before
// flushing a single store.ApplyIndexBatch transaction. Chosen empirically:
// large enough to amortise BeginTx/Commit/fsync across many ops, small
// enough that a fatal error in the middle of a run still surfaces quickly.
const batchSize = 256

// collect drains the results channel, updates the report, and writes to the
// store in batched transactions (or records seen paths for dry-run).
// Returns the partial report alongside any fatal write error.
func (i *indexer) collect() (Report, error) {
	report := Report{}
	batch := make([]store.IndexBatchEntry, 0, batchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := i.store.ApplyIndexBatch(i.ctx, batch); err != nil {
			return fmt.Errorf("apply index batch: %w", err)
		}
		batch = batch[:0]
		return nil
	}
	for r := range i.results {
		if r.err != nil {
			report.Errors++
			report.ErrorList = append(report.ErrorList, r.err)
			continue
		}
		tally(&report, r.kind)
		if i.opts.DryRun {
			i.seen[r.row.Path] = struct{}{}
			continue
		}
		batch = append(batch, i.batchEntry(r))
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return report, err
			}
		}
	}
	if err := flush(); err != nil {
		return report, err
	}
	return report, nil
}

// batchEntry translates a resultItem into the store-level batch op shape.
// Unchanged rows become TouchSeen with LastSeenRunID pinned to the current
// run; added/modified rows become Upsert. The store-side helpers read the
// fields each op needs and ignore the rest.
func (i *indexer) batchEntry(r resultItem) store.IndexBatchEntry {
	if r.kind == kindUnchanged {
		row := r.row
		row.LastSeenRunID = i.runID
		return store.IndexBatchEntry{Kind: store.BatchOpTouchSeen, Row: row}
	}
	return store.IndexBatchEntry{Kind: store.BatchOpUpsert, Row: r.row}
}

func tally(report *Report, kind changeKind) {
	switch kind {
	case kindAdded:
		report.Added++
	case kindModified:
		report.Modified++
	case kindUnchanged:
		report.Unchanged++
	}
}

func (i *indexer) waitForWalker() error {
	err := <-i.walkErrCh
	if err == nil {
		return nil
	}
	// Treat context cancellation as a fatal failure rather than silently
	// declaring success: if the walk was cut short, we have not seen every
	// file, and running MarkMissing afterwards would flip live rows to
	// missing. The deferred finishRun in Index() classifies this as
	// RunStatusFailed via the propagated error.
	return fmt.Errorf("walk: %w", err)
}

// finalizeMissing flips DB rows in this volume that we did not encounter to
// status='missing' (or counts what would be flipped, in DryRun mode). It is
// only safe to call after a fully clean walk: a partial walk (per-file
// errors) leaves some files un-touched even though they exist on disk, so
// MarkMissing is skipped and report.Missing stays at zero in that case. The
// caller surfaces partial-ness via the run's status='partial'.
//
// A brand-new dry-run volume (volumeExists=false) has no rows to flip.
func (i *indexer) finalizeMissing(report *Report) error {
	if !i.volumeExists {
		return nil
	}
	if report.Errors > 0 {
		return nil
	}
	if !i.opts.DryRun {
		n, err := i.store.MarkMissing(i.ctx, i.volumeID, i.runID)
		if err != nil {
			return fmt.Errorf("mark missing: %w", err)
		}
		report.Missing = int(n)
		return nil
	}
	present, err := i.store.ListPresentPathsUnder(i.ctx, i.volumeID)
	if err != nil {
		return fmt.Errorf("list present: %w", err)
	}
	for p := range present {
		if _, ok := i.seen[p]; !ok {
			report.Missing++
		}
	}
	return nil
}

// process is the per-file decision: shallow shortcut, hash, classify as
// added/modified/unchanged. buf is the worker-local scratch buffer threaded
// through to hashFile so the 1 MiB io.CopyBuffer allocation happens once
// per worker, not once per file.
func (i *indexer) process(w workItem, buf []byte) resultItem {
	var existing store.FileRow
	var hasExisting bool
	if i.preloaded != nil {
		// Workers read the preloaded map concurrently; map reads are safe
		// because the map is never written after newIndexer populated it.
		if row, ok := i.preloaded[w.relPath]; ok {
			existing, hasExisting = row, true
		}
	}

	if i.opts.Shallow && hasExisting && metadataMatches(existing, w) {
		return resultItem{row: existing, kind: kindUnchanged}
	}

	digest, err := hashFile(w.absPath, buf)
	if err != nil {
		return resultItem{err: fmt.Errorf("hash %s: %w", w.absPath, err)}
	}

	row := i.rowFor(w, digest)
	if !hasExisting {
		return resultItem{row: row, kind: kindAdded}
	}
	if bytes.Equal(existing.Blake3, digest) && existing.Status == store.StatusPresent {
		return resultItem{row: existing, kind: kindUnchanged}
	}
	return resultItem{row: row, kind: kindModified}
}

// metadataMatches reports whether the on-disk size and mtime agree with the
// stored row's metadata and the row is currently 'present'. This is the
// signal --shallow uses to skip re-hashing.
func metadataMatches(existing store.FileRow, w workItem) bool {
	return existing.Status == store.StatusPresent &&
		existing.SizeBytes == w.sizeBytes &&
		existing.MtimeNs == w.mtimeNs
}

func (i *indexer) rowFor(w workItem, digest []byte) store.FileRow {
	return store.FileRow{
		VolumeID:       i.volumeID,
		Path:           w.relPath,
		Blake3:         digest,
		SizeBytes:      w.sizeBytes,
		MtimeNs:        w.mtimeNs,
		Status:         store.StatusPresent,
		FirstSeenRunID: i.runID,
		LastSeenRunID:  i.runID,
		IndexedAtNs:    store.NowNs(),
	}
}

// resolveVolume returns the volume for absRoot and whether it already exists
// in the database. When name is non-empty, the lookup is by name first;
// otherwise the legacy path-keyed behaviour applies. In dry-run mode against
// a never-indexed path the returned Volume has no row backing it
// (exists=false); callers must avoid issuing volume-scoped DB queries
// against it.
func resolveVolume(ctx context.Context, s *store.Store, name, absRoot string, dryRun bool) (store.Volume, bool, error) {
	if name != "" {
		return resolveNamedVolume(ctx, s, name, absRoot, dryRun)
	}
	if !dryRun {
		v, err := s.GetOrCreateVolume(ctx, absRoot)
		if err != nil {
			return store.Volume{}, false, fmt.Errorf("resolve volume: %w", err)
		}
		return v, true, nil
	}
	v, err := s.GetVolumeByPath(ctx, absRoot)
	if err == nil {
		return v, true, nil
	}
	if !store.IsNotFound(err) {
		return store.Volume{}, false, fmt.Errorf("lookup volume: %w", err)
	}
	return store.Volume{Path: absRoot}, false, nil
}

// resolveNamedVolume is the config-aware path: the caller passes a volume
// name (from config) plus the absolute path declared for that name. If a
// volume row already exists under this name, its path must match — a
// mismatch indicates the config moved the volume to a new location, which
// the user must resolve explicitly (re-name in config or migrate the DB).
func resolveNamedVolume(ctx context.Context, s *store.Store, name, absRoot string, dryRun bool) (store.Volume, bool, error) {
	v, err := s.GetVolumeByName(ctx, name)
	if err == nil {
		if v.Path != absRoot {
			return store.Volume{}, false, fmt.Errorf("volume %q is at %q in the DB but config says %q — resolve the conflict before re-indexing", name, v.Path, absRoot)
		}
		return v, true, nil
	}
	if !store.IsNotFound(err) {
		return store.Volume{}, false, fmt.Errorf("lookup volume by name: %w", err)
	}
	if dryRun {
		return store.Volume{Name: name, Path: absRoot}, false, nil
	}
	created, err := s.CreateVolume(ctx, name, absRoot)
	if err != nil {
		return store.Volume{}, false, fmt.Errorf("create volume %q: %w", name, err)
	}
	return created, true, nil
}

// hashReadBufferSize is the read buffer io.CopyBuffer hands to the file →
// BLAKE3 copy. io.Copy's default 32 KiB triggers ~80 read syscalls on the
// 2.5 MB average file in this volume; at 1 MiB it's 3. Bigger reads also
// let APFS readahead engage, since the kernel grows its readahead window
// based on observed read sizes. The buffer is allocated once per worker
// (see (*indexer).worker) and threaded through to amortise the cost across
// the run; allocating per-file made GC pressure outweigh the syscall win.
const hashReadBufferSize = 1 << 20

func hashFile(path string, buf []byte) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := blake3.New()
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

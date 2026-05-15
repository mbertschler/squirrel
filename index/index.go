package index

import (
	"bytes"
	"context"
	"errors"
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
	// Shallow: skip rehashing if (size, mtime) match the stored row.
	Shallow bool
	// DryRun: do not write to the database; only compute and report.
	DryRun bool
	// Workers: number of hashing goroutines. 0 means runtime.NumCPU().
	Workers int
	// QueueDepth: maximum pending entries between walker and workers. 0 means 4 * Workers.
	QueueDepth int
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
// relative to root; root itself is stored on each row as the absolute path
// passed in (a later config milestone will replace this with a logical name).
func Index(ctx context.Context, s *store.Store, root string, opts Options) (Report, error) {
	idx, err := newIndexer(ctx, s, root, opts)
	if err != nil {
		return Report{}, err
	}

	idx.startWorkers()
	idx.startWalker()

	report, err := idx.collect()
	if err != nil {
		return report, err
	}
	if err := idx.waitForWalker(); err != nil {
		return report, err
	}
	if err := idx.finalizeMissing(&report); err != nil {
		return report, err
	}
	return report, nil
}

// indexer holds the state of one Index() invocation. It owns the worker pool,
// the walker goroutine, and the channels connecting them.
type indexer struct {
	ctx       context.Context
	store     *store.Store
	absRoot   string
	opts      Options
	startedAt int64

	workers int
	work    chan workItem
	results chan resultItem

	workerWG  sync.WaitGroup
	walkErrCh chan error

	// seen is populated only in DryRun mode; finalizeMissing uses it to count
	// rows that exist in the DB but were not encountered during the walk.
	seen map[string]struct{}
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
		workers = runtime.NumCPU()
	}
	queueDepth := opts.QueueDepth
	if queueDepth <= 0 {
		queueDepth = workers * 4
	}

	idx := &indexer{
		ctx:       ctx,
		store:     s,
		absRoot:   absRoot,
		opts:      opts,
		startedAt: store.Now(),
		workers:   workers,
		work:      make(chan workItem, queueDepth),
		results:   make(chan resultItem, queueDepth),
		walkErrCh: make(chan error, 1),
	}
	if opts.DryRun {
		idx.seen = make(map[string]struct{})
	}
	return idx, nil
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
	for w := range i.work {
		i.results <- i.process(w)
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

// collect drains the results channel, updates the report, and writes to the
// store (or records seen paths for dry-run). Returns the partial report
// alongside any fatal write error.
func (i *indexer) collect() (Report, error) {
	report := Report{}
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
		if err := i.persist(r); err != nil {
			return report, err
		}
	}
	return report, nil
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

func (i *indexer) persist(r resultItem) error {
	if r.kind == kindUnchanged {
		if err := i.store.TouchSeen(i.ctx, r.row.Root, r.row.Path, i.startedAt); err != nil {
			return fmt.Errorf("touch %s/%s: %w", r.row.Root, r.row.Path, err)
		}
		return nil
	}
	if err := i.store.Upsert(i.ctx, r.row); err != nil {
		return fmt.Errorf("upsert %s/%s: %w", r.row.Root, r.row.Path, err)
	}
	return nil
}

func (i *indexer) waitForWalker() error {
	err := <-i.walkErrCh
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("walk: %w", err)
	}
	return nil
}

// finalizeMissing flips DB rows under absRoot that we did not encounter to
// status='missing' (or counts what would be flipped, in DryRun mode).
func (i *indexer) finalizeMissing(report *Report) error {
	if !i.opts.DryRun {
		n, err := i.store.MarkMissing(i.ctx, i.absRoot, i.startedAt)
		if err != nil {
			return fmt.Errorf("mark missing: %w", err)
		}
		report.Missing = int(n)
		return nil
	}
	present, err := i.store.ListPresentPathsUnder(i.ctx, i.absRoot)
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
// added/modified/unchanged.
func (i *indexer) process(w workItem) resultItem {
	existing, err := i.store.GetByPath(i.ctx, i.absRoot, w.relPath)
	hasExisting := err == nil
	if err != nil && !store.IsNotFound(err) {
		return resultItem{err: fmt.Errorf("lookup %s/%s: %w", i.absRoot, w.relPath, err)}
	}

	if i.opts.Shallow && hasExisting && metadataMatches(existing, w) {
		return resultItem{row: existing, kind: kindUnchanged}
	}

	digest, err := hashFile(w.absPath)
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
		Root:       i.absRoot,
		Path:       w.relPath,
		Blake3:     digest,
		SizeBytes:  w.sizeBytes,
		MtimeNs:    w.mtimeNs,
		Status:     store.StatusPresent,
		LastSeenAt: i.startedAt,
		IndexedAt:  store.Now(),
	}
}

func hashFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := blake3.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

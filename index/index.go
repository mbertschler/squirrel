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
	// FollowSymlinks: traverse into symlinked directories. Always skipped for now.
	FollowSymlinks bool
}

type Report struct {
	Added     int
	Modified  int
	Unchanged int
	Missing   int
	Errors    int
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
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Report{}, fmt.Errorf("resolve root: %w", err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return Report{}, fmt.Errorf("stat root: %w", err)
	}
	if !info.IsDir() {
		return Report{}, fmt.Errorf("root %q is not a directory", absRoot)
	}

	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	queueDepth := opts.QueueDepth
	if queueDepth <= 0 {
		queueDepth = workers * 4
	}

	startedAt := store.Now()

	work := make(chan workItem, queueDepth)
	results := make(chan resultItem, queueDepth)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for w := range work {
				results <- processFile(ctx, s, absRoot, w, opts, startedAt)
			}
		}()
	}

	walkErrCh := make(chan error, 1)
	go func() {
		defer close(work)
		err := filepath.WalkDir(absRoot, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				select {
				case results <- resultItem{err: fmt.Errorf("walk %s: %w", path, walkErr)}:
				case <-ctx.Done():
					return ctx.Err()
				}
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				select {
				case results <- resultItem{err: fmt.Errorf("stat %s: %w", path, err)}:
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			}
			relPath, err := filepath.Rel(absRoot, path)
			if err != nil {
				select {
				case results <- resultItem{err: fmt.Errorf("rel %s: %w", path, err)}:
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			}
			select {
			case work <- workItem{
				absPath:   path,
				relPath:   filepath.ToSlash(relPath),
				sizeBytes: fi.Size(),
				mtimeNs:   fi.ModTime().UnixNano(),
			}:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		})
		walkErrCh <- err
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	report := Report{}
	var seen map[string]struct{}
	if opts.DryRun {
		seen = make(map[string]struct{})
	}
	for r := range results {
		if r.err != nil {
			report.Errors++
			fmt.Fprintln(os.Stderr, "error:", r.err)
			continue
		}
		switch r.kind {
		case kindAdded:
			report.Added++
		case kindModified:
			report.Modified++
		case kindUnchanged:
			report.Unchanged++
		}
		if opts.DryRun {
			seen[r.row.Path] = struct{}{}
			continue
		}
		if r.kind == kindUnchanged {
			if err := s.TouchSeen(ctx, r.row.Root, r.row.Path, startedAt); err != nil {
				return report, fmt.Errorf("touch %s/%s: %w", r.row.Root, r.row.Path, err)
			}
		} else {
			if err := s.Upsert(ctx, r.row); err != nil {
				return report, fmt.Errorf("upsert %s/%s: %w", r.row.Root, r.row.Path, err)
			}
		}
	}

	if err := <-walkErrCh; err != nil && !errors.Is(err, context.Canceled) {
		return report, fmt.Errorf("walk: %w", err)
	}

	if !opts.DryRun {
		n, err := s.MarkMissing(ctx, absRoot, startedAt)
		if err != nil {
			return report, fmt.Errorf("mark missing: %w", err)
		}
		report.Missing = int(n)
	} else {
		present, err := s.ListPresentPathsUnder(ctx, absRoot)
		if err != nil {
			return report, fmt.Errorf("list present: %w", err)
		}
		for p := range present {
			if _, ok := seen[p]; !ok {
				report.Missing++
			}
		}
	}

	return report, nil
}

func processFile(ctx context.Context, s *store.Store, absRoot string, w workItem, opts Options, startedAt int64) resultItem {
	existing, err := s.GetByPath(ctx, absRoot, w.relPath)
	hasExisting := err == nil
	if err != nil && !store.IsNotFound(err) {
		return resultItem{err: fmt.Errorf("lookup %s/%s: %w", absRoot, w.relPath, err)}
	}

	if hasExisting && existing.Status == store.StatusPresent &&
		existing.SizeBytes == w.sizeBytes && existing.MtimeNs == w.mtimeNs {
		if opts.Shallow {
			return resultItem{row: existing, kind: kindUnchanged}
		}
	}

	digest, err := hashFile(w.absPath)
	if err != nil {
		return resultItem{err: fmt.Errorf("hash %s: %w", w.absPath, err)}
	}

	row := store.FileRow{
		Root:       absRoot,
		Path:       w.relPath,
		Blake3:     digest,
		SizeBytes:  w.sizeBytes,
		MtimeNs:    w.mtimeNs,
		Status:     store.StatusPresent,
		LastSeenAt: startedAt,
		IndexedAt:  store.Now(),
	}

	if !hasExisting {
		return resultItem{row: row, kind: kindAdded}
	}
	if bytes.Equal(existing.Blake3, digest) && existing.Status == store.StatusPresent {
		return resultItem{row: existing, kind: kindUnchanged}
	}
	return resultItem{row: row, kind: kindModified}
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

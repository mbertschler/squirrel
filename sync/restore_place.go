package sync

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/zeebo/blake3"
)

// restorePlacer lands restored files in a target tree. Each file streams
// into a temporary file beside its path while it is hashed, is checked,
// stamped and flushed, and is then renamed over the path: a crash never
// leaves a half-written file at a path, and bytes that fail their check
// never reach one. With preserve, a file the path already held first moves
// into .squirrel-restore-history/run-<id>/.
type restorePlacer struct {
	target   string
	runID    int64
	preserve bool
}

// place writes the bytes r yields to rel, stamped with mtime. want, when
// set, is the BLAKE3 they must hash to; nil places them unchecked. It
// returns how many bytes it placed.
func (pl restorePlacer) place(rel string, r io.Reader, want []byte, mtime time.Time) (int64, error) {
	dst := pl.path(rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("create target dir: %w", err)
	}
	if err := refuseNonRegularTarget(rel, dst); err != nil {
		return 0, err
	}
	tmp, n, err := stageRestored(dst, r, want, mtime)
	if err != nil {
		return 0, fmt.Errorf("restore %s: %w", rel, err)
	}
	if err := pl.commit(rel, dst, tmp); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return n, nil
}

// holds reports whether rel already holds size bytes hashing to want, so
// a restore can leave it as it is.
func (pl restorePlacer) holds(rel string, size int64, want []byte) bool {
	dst := pl.path(rel)
	fi, err := os.Lstat(dst)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != size {
		return false
	}
	digest, err := hashLocalFile(dst)
	return err == nil && bytes.Equal(digest, want)
}

func (pl restorePlacer) path(rel string) string {
	return filepath.Join(pl.target, filepath.FromSlash(rel))
}

// stageRestored streams r into a fresh temporary file beside dst while
// hashing it, and returns the file's name once its bytes check out, carry
// mtime, and are flushed to disk.
func stageRestored(dst string, r io.Reader, want []byte, mtime time.Time) (string, int64, error) {
	f, err := os.CreateTemp(filepath.Dir(dst), ".squirrel-restore-*")
	if err != nil {
		return "", 0, err
	}
	h := blake3.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err == nil && want != nil && !bytes.Equal(h.Sum(nil), want) {
		err = fmt.Errorf("the bytes hash to %s, want %s", hex.EncodeToString(h.Sum(nil)), hex.EncodeToString(want))
	}
	if err == nil {
		err = os.Chtimes(f.Name(), mtime, mtime)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", 0, err
	}
	return f.Name(), n, nil
}

// commit renames the staged file over rel, first moving a file rel holds
// aside when the restore preserves what it replaces.
func (pl restorePlacer) commit(rel, dst, tmp string) error {
	if pl.preserve {
		if err := pl.backupExisting(rel, dst); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("place %s: %w", rel, err)
	}
	return syncLocalDir(filepath.Dir(dst))
}

// backupExisting moves an existing regular file at dst under the per-run
// restore-history subtree so an in-place restore never destroys prior
// bytes — the local-side counterpart of a mirror's .squirrel-history. The
// caller has already refused any non-regular destination, so dst is absent
// or a regular file here.
func (pl restorePlacer) backupExisting(rel, dst string) error {
	if _, err := os.Lstat(dst); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat existing %s: %w", rel, err)
	}
	backup := filepath.Join(pl.target, RestoreHistoryDirName, "run-"+strconv.FormatInt(pl.runID, 10), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return fmt.Errorf("create restore-history dir: %w", err)
	}
	if err := os.Rename(dst, backup); err != nil {
		return fmt.Errorf("preserve overwritten %s: %w", rel, err)
	}
	return nil
}

// refuseNonRegularTarget refuses to write when the destination already
// exists as anything other than a regular file. A symlink, device, fifo,
// socket, or directory is a hard refusal, not a silent skip: squirrel
// never replaces what it does not understand. An absent path or a plain
// regular file is allowed.
func refuseNonRegularTarget(rel, dst string) error {
	info, err := os.Lstat(dst)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat target %s: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to write %s: destination already exists as a non-regular file (%s) — squirrel will not follow or replace it", rel, info.Mode().Type())
	}
	return nil
}

// syncLocalDir flushes a directory of this machine, so a name moved into
// it survives a power cut.
func syncLocalDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s to sync it: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil && !dirSyncUnsupported(err) {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}

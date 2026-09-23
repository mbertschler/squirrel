package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// localTransport is the transport onto a directory of this machine's
// filesystem. os.Root confines every name to the root; on top of that each
// operation checks its name's parent chain with Lstat and refuses a
// symlink anywhere along it, because os.Root follows symlinks that stay
// inside the root.
type localTransport struct {
	root *os.Root
}

func openLocalTransport(dir string) (*localTransport, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open destination root %s: %w", dir, err)
	}
	return &localTransport{root: root}, nil
}

func (t *localTransport) Close() error { return t.root.Close() }

func (t *localTransport) Stat(_ context.Context, name string) (entry, error) {
	if err := t.checkName(name); err != nil {
		return entry{}, err
	}
	fi, err := t.root.Lstat(filepath.FromSlash(name))
	if err != nil {
		return entry{}, err
	}
	return entryOf(fi), nil
}

func (t *localTransport) List(_ context.Context, dir string) ([]entry, error) {
	if dir != "." {
		if err := t.checkName(dir); err != nil {
			return nil, err
		}
		if err := requireKind(dir, kindDir, t.lstat); err != nil {
			return nil, err
		}
	}
	f, err := t.root.Open(filepath.FromSlash(dir))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	des, err := f.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	out := make([]entry, 0, len(des))
	for _, de := range des {
		fi, err := de.Info()
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", dir, err)
		}
		out = append(out, entryOf(fi))
	}
	return out, nil
}

func (t *localTransport) Get(_ context.Context, name string) (io.ReadCloser, error) {
	if err := t.checkName(name); err != nil {
		return nil, err
	}
	if err := requireKind(name, kindFile, t.lstat); err != nil {
		return nil, err
	}
	return t.root.Open(filepath.FromSlash(name))
}

func (t *localTransport) Put(ctx context.Context, name string, r io.Reader, mtime time.Time) error {
	if err := t.checkName(name); err != nil {
		return err
	}
	if err := t.mkdirParents(name); err != nil {
		return err
	}
	f, err := t.root.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := t.fill(ctx, f, name, r, mtime); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	return t.syncDir(path.Dir(name))
}

// fill streams r into the freshly created f, stamps mtime, and syncs.
func (t *localTransport) fill(ctx context.Context, f *os.File, name string, r io.Reader, mtime time.Time) error {
	if _, err := io.Copy(f, ctxReader{ctx: ctx, r: r}); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := t.root.Chtimes(filepath.FromSlash(name), mtime, mtime); err != nil {
		return fmt.Errorf("set mtime of %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	return nil
}

func (t *localTransport) Rename(_ context.Context, from, to string) error {
	if err := t.checkName(from); err != nil {
		return err
	}
	if err := t.checkName(to); err != nil {
		return err
	}
	if _, err := t.root.Lstat(filepath.FromSlash(from)); err != nil {
		return err
	}
	if err := t.mkdirParents(to); err != nil {
		return err
	}
	if err := renameNoReplace(t.root, from, to); err != nil {
		return err
	}
	if err := t.syncDir(path.Dir(from)); err != nil {
		return err
	}
	return t.syncDir(path.Dir(to))
}

func (t *localTransport) Remove(_ context.Context, name string) error {
	if err := t.checkName(name); err != nil {
		return err
	}
	return t.root.Remove(filepath.FromSlash(name))
}

// checkName refuses a name that is not a clean path below the root, or
// whose parent chain crosses a symlink or a non-directory.
func (t *localTransport) checkName(name string) error {
	if !validName(name) || (runtime.GOOS == "windows" && strings.ContainsRune(name, '\\')) {
		return fmt.Errorf("%w: %q", errInvalidName, name)
	}
	return checkParents(name, t.lstat)
}

func (t *localTransport) lstat(name string) (fs.FileInfo, error) {
	return t.root.Lstat(filepath.FromSlash(name))
}

func (t *localTransport) mkdirParents(name string) error {
	dir := path.Dir(name)
	if dir == "." {
		return nil
	}
	if err := t.root.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}

// syncDir flushes a directory, so a name created or moved in it survives
// a power cut.
func (t *localTransport) syncDir(dir string) error {
	f, err := t.root.Open(filepath.FromSlash(dir))
	if err != nil {
		return fmt.Errorf("open %s to sync it: %w", dir, err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil && !dirSyncUnsupported(err) {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}

// renameCheckThenMove is the rename for a filesystem without a no-replace
// rename, exFAT for one: an Lstat, then a plain rename. A writer outside
// squirrel could slip a file in between the two; squirrel cannot collide
// with itself there, because the run guard and the volume marker already
// exclude a second squirrel writer.
func renameCheckThenMove(root *os.Root, from, to string) error {
	_, err := root.Lstat(filepath.FromSlash(to))
	if err == nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: fs.ErrExist}
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return root.Rename(filepath.FromSlash(from), filepath.FromSlash(to))
}

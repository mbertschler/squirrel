package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"
)

// transport is byte-level access to one destination root. Names are
// slash-separated and relative to the root, and a name whose path crosses
// a symlink is refused. Put creates exclusively and Rename fails on an
// existing target, so every call leaves bytes already there in place.
type transport interface {
	// Stat describes name itself, a symlink as a symlink; fs.ErrNotExist
	// when absent.
	Stat(ctx context.Context, name string) (entry, error)
	// List describes the entries directly in dir ("." is the root).
	List(ctx context.Context, dir string) ([]entry, error)
	// Get opens the regular file at name for reading.
	Get(ctx context.Context, name string) (io.ReadCloser, error)
	// Put creates name exclusively (fs.ErrExist if present), creating its
	// parents, streams r into it, sets its mtime, and syncs it to stable
	// storage before returning.
	Put(ctx context.Context, name string, r io.Reader, mtime time.Time) error
	// Rename moves from to to, creating to's parents; fs.ErrExist if to
	// exists.
	Rename(ctx context.Context, from, to string) error
	// Remove deletes the file or empty directory at name.
	Remove(ctx context.Context, name string) error
	Close() error
}

// entry describes one name on a destination as the transport found it.
type entry struct {
	name  string // base name
	kind  entryKind
	size  int64
	mtime time.Time
}

// entryKind is what a name on a destination is.
type entryKind int

const (
	kindFile entryKind = iota
	kindDir
	kindSymlink
	kindOther
)

func kindOf(mode fs.FileMode) entryKind {
	switch {
	case mode.IsRegular():
		return kindFile
	case mode.IsDir():
		return kindDir
	case mode&fs.ModeSymlink != 0:
		return kindSymlink
	default:
		return kindOther
	}
}

// errSymlinkInPath refuses a name whose parent chain crosses a symlink,
// which the transport never follows.
var errSymlinkInPath = errors.New("a directory along the path is a symlink")

// errParentNotDir reports that a name's parent chain crosses something
// that is not a directory, so the name cannot exist.
var errParentNotDir = errors.New("a parent along the path is not a directory")

// errInvalidName refuses a name that is not a clean slash-separated path
// inside the root.
var errInvalidName = errors.New("invalid destination name")

// errUnexpectedKind refuses an operation on a name that is not the kind of
// entry the operation reads.
var errUnexpectedKind = errors.New("unexpected kind of entry")

// validName reports whether name is a clean slash-separated path strictly
// below a root.
func validName(name string) bool {
	return fs.ValidPath(name) && name != "."
}

// checkParents refuses a name whose parent chain crosses a symlink or
// something that is not a directory. lstat describes one root-relative
// name without following it. A parent that does not exist ends the check:
// nothing below it exists either.
func checkParents(name string, lstat func(string) (fs.FileInfo, error)) error {
	dir := path.Dir(name)
	if dir == "." {
		return nil
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		fi, err := lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		switch kindOf(fi.Mode()) {
		case kindDir:
		case kindSymlink:
			return fmt.Errorf("%s: %w", p, errSymlinkInPath)
		default:
			return fmt.Errorf("%s: %w", p, errParentNotDir)
		}
	}
	return nil
}

// requireKind refuses name unless it exists and is of kind want.
func requireKind(name string, want entryKind, lstat func(string) (fs.FileInfo, error)) error {
	fi, err := lstat(name)
	if err != nil {
		return err
	}
	if kindOf(fi.Mode()) != want {
		return fmt.Errorf("%s: %w", name, errUnexpectedKind)
	}
	return nil
}

func entryOf(fi fs.FileInfo) entry {
	return entry{name: fi.Name(), kind: kindOf(fi.Mode()), size: fi.Size(), mtime: fi.ModTime()}
}

// copyBufferSize is the buffer a copy out of a transport reads with. A
// read this large lets the sftp client split it into concurrent requests,
// so a high-latency link does not cap a download at one round trip per
// 32 KiB.
const copyBufferSize = 1 << 20

// ctxReader stops a streaming copy once its context is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

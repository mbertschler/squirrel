package sync

import (
	"context"
	"errors"
	"io"
	"io/fs"
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

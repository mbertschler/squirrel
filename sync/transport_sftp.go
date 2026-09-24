package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// sftpTransport is the transport onto a directory of an sftp server. The
// server resolves symlinks itself, so every operation checks its name's
// parent chain with Lstat and refuses a symlink anywhere along it. The
// protocol's own create-exclusive and rename are not trusted to refuse an
// existing name on every server, so Put and Rename Lstat their target
// first.
type sftpTransport struct {
	conn   *ssh.Client
	client *sftp.Client
	root   string // the destination root on the server
	// fsync is whether the server offers fsync@openssh.com, which Put
	// uses to flush a file to stable storage before it returns.
	fsync bool
}

// Close drops the ssh connection first: the sftp client's own Close
// waits for the server to end the session, which a hung server never does.
func (t *sftpTransport) Close() error {
	err := t.conn.Close()
	_ = t.client.Close()
	return err
}

func (t *sftpTransport) Stat(_ context.Context, name string) (entry, error) {
	if err := t.checkName(name); err != nil {
		return entry{}, err
	}
	fi, err := t.lstat(name)
	if err != nil {
		return entry{}, err
	}
	return entryOf(fi), nil
}

func (t *sftpTransport) List(_ context.Context, dir string) ([]entry, error) {
	full := t.root
	if dir != "." {
		if err := t.checkName(dir); err != nil {
			return nil, err
		}
		if err := requireKind(dir, kindDir, t.lstat); err != nil {
			return nil, err
		}
		full = t.full(dir)
	}
	fis, err := t.client.ReadDir(full)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	out := make([]entry, 0, len(fis))
	for _, fi := range fis {
		switch name := fi.Name(); {
		case name == "." || name == "..":
		case !validName(name) || strings.Contains(name, "/"):
			return nil, fmt.Errorf("list %s: the server listed %q: %w", dir, name, errInvalidName)
		default:
			out = append(out, entryOf(fi))
		}
	}
	return out, nil
}

func (t *sftpTransport) Get(_ context.Context, name string) (io.ReadCloser, error) {
	if err := t.checkName(name); err != nil {
		return nil, err
	}
	if err := requireKind(name, kindFile, t.lstat); err != nil {
		return nil, err
	}
	return t.client.Open(t.full(name))
}

func (t *sftpTransport) Put(ctx context.Context, name string, r io.Reader, mtime time.Time) error {
	if err := t.checkName(name); err != nil {
		return err
	}
	if err := t.refuseExisting("put", name); err != nil {
		return err
	}
	if err := t.mkdirParents(name); err != nil {
		return err
	}
	f, err := t.client.OpenFile(t.full(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		if exists := t.refuseExisting("put", name); exists != nil {
			return exists
		}
		return err
	}
	if err := t.fill(ctx, f, name, r, mtime); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	return nil
}

// fill streams r into the freshly created f with concurrent writes, so a
// high-latency link does not cap throughput, stamps mtime, and flushes
// the file where the server offers it.
func (t *sftpTransport) fill(ctx context.Context, f *sftp.File, name string, r io.Reader, mtime time.Time) error {
	if _, err := f.ReadFromWithConcurrency(ctxReader{ctx: ctx, r: r}, 0); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := t.client.Chtimes(t.full(name), mtime, mtime); err != nil {
		return fmt.Errorf("set mtime of %s: %w", name, err)
	}
	if !t.fsync {
		return nil
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	return nil
}

func (t *sftpTransport) Rename(_ context.Context, from, to string) error {
	if err := t.checkName(from); err != nil {
		return err
	}
	if err := t.checkName(to); err != nil {
		return err
	}
	if _, err := t.lstat(from); err != nil {
		return err
	}
	if err := t.mkdirParents(to); err != nil {
		return err
	}
	if err := t.refuseExisting("rename", to); err != nil {
		return err
	}
	if err := t.client.Rename(t.full(from), t.full(to)); err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}

func (t *sftpTransport) Remove(_ context.Context, name string) error {
	if err := t.checkName(name); err != nil {
		return err
	}
	fi, err := t.lstat(name)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return t.client.RemoveDirectory(t.full(name))
	}
	return t.client.Remove(t.full(name))
}

// refuseExisting fails with fs.ErrExist when name is present.
func (t *sftpTransport) refuseExisting(op, name string) error {
	_, err := t.lstat(name)
	if err == nil {
		return &fs.PathError{Op: op, Path: name, Err: fs.ErrExist}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (t *sftpTransport) checkName(name string) error {
	if !validName(name) {
		return fmt.Errorf("%w: %q", errInvalidName, name)
	}
	return checkParents(name, t.lstat)
}

func (t *sftpTransport) lstat(name string) (fs.FileInfo, error) {
	return t.client.Lstat(t.full(name))
}

func (t *sftpTransport) full(name string) string { return path.Join(t.root, name) }

func (t *sftpTransport) mkdirParents(name string) error {
	dir := path.Dir(name)
	if dir == "." {
		return nil
	}
	if err := t.client.MkdirAll(t.full(dir)); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}

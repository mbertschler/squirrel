package offload

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/store"
)

// hashReadBufferSize matches the indexer's read buffer for the same
// reason: 1 MiB reads keep the syscall count low and let filesystem
// readahead engage on the multi-MB files offload typically targets.
const hashReadBufferSize = 1 << 20

// verifyAndRemove re-verifies that the on-disk bytes at row.Path are
// exactly the bytes the indexed row describes, then unlinks the file —
// and only the file. Every disagreement between disk and index is a
// refusal returned as a drift reason ("the disk is newer than the
// index"): bytes the index never recorded must survive the run.
//
// Traversal safety mirrors the indexer's walk, which treats symlinks as
// opaque: every parent component is Lstat'ed through the os.Root (which
// also confines the whole resolution to the volume) and must be a real
// directory, and the final component must Lstat as a regular file. The
// opened handle is then bound to the Lstat'ed inode with os.SameFile —
// the O_NOFOLLOW-equivalent check — the size, mtime, and BLAKE3 are
// verified from that handle, and a final Lstat+SameFile narrows the
// verify→unlink race window to the Remove call itself.
func verifyAndRemove(dir *os.Root, row store.FileRow, buf []byte) (drift string, err error) {
	if drift, err := checkParents(dir, row.Path); drift != "" || err != nil {
		return drift, err
	}
	fi, err := dir.Lstat(row.Path)
	if errors.Is(err, os.ErrNotExist) {
		return "missing on disk while the index row is 'present'; re-index", nil
	}
	if err != nil {
		return "", fmt.Errorf("lstat: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "path is a symlink on disk, indexed as a regular file", nil
	}
	if !fi.Mode().IsRegular() {
		return fmt.Sprintf("path is %s on disk, indexed as a regular file", fi.Mode().Type()), nil
	}

	f, err := dir.Open(row.Path)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	if drift, err := verifyHandle(f, fi, row, buf); drift != "" || err != nil {
		return drift, err
	}

	post, err := dir.Lstat(row.Path)
	if err != nil || !os.SameFile(fi, post) {
		return "file replaced during verification", nil
	}
	if err := dir.Remove(row.Path); err != nil {
		return "", fmt.Errorf("remove: %w", err)
	}
	return "", nil
}

// verifyHandle checks the opened handle against both the just-taken
// Lstat (same inode) and the indexed row (size, mtime, BLAKE3). The
// hash is computed from the handle itself, so the verified bytes are
// the ones behind the inode the surrounding checks pin down.
func verifyHandle(f *os.File, lstat os.FileInfo, row store.FileRow, buf []byte) (string, error) {
	hfi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat open handle: %w", err)
	}
	if !os.SameFile(lstat, hfi) {
		return "file replaced between check and open", nil
	}
	if hfi.Size() != row.SizeBytes {
		return fmt.Sprintf("size changed: disk %d, indexed %d", hfi.Size(), row.SizeBytes), nil
	}
	if hfi.ModTime().UnixNano() != row.MtimeNs {
		return fmt.Sprintf("mtime changed: disk %d, indexed %d", hfi.ModTime().UnixNano(), row.MtimeNs), nil
	}
	h := blake3.New()
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", fmt.Errorf("hash: %w", err)
	}
	if digest := h.Sum(nil); !bytes.Equal(digest, row.Blake3) {
		return fmt.Sprintf("content hash changed: disk %x, indexed %x", digest, row.Blake3), nil
	}
	return "", nil
}

// checkParents Lstats every parent component of relPath inside the
// root, shallowest first, requiring each to be a real directory. The
// indexer's walk records paths whose every component was a directory,
// so a symlink (or anything else) appearing in the chain since is
// drift.
func checkParents(dir *os.Root, relPath string) (string, error) {
	for i := 0; i < len(relPath); i++ {
		if relPath[i] != '/' {
			continue
		}
		prefix := relPath[:i]
		fi, err := dir.Lstat(prefix)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Sprintf("parent %s missing on disk", prefix), nil
		}
		if err != nil {
			return "", fmt.Errorf("lstat parent %s: %w", prefix, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Sprintf("parent %s is a symlink", prefix), nil
		}
		if !fi.IsDir() {
			return fmt.Sprintf("parent %s is not a directory", prefix), nil
		}
	}
	return "", nil
}

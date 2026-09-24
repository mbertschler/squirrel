package sync

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace moves from to to inside root with renameat2's
// RENAME_NOREPLACE, which fails with EEXIST instead of replacing to. A
// filesystem that does not support the flag gets renameCheckThenMove.
func renameNoReplace(root *os.Root, from, to string) error {
	err := withParentDirs(root, from, to, func(fromDir, toDir *os.File, fromBase, toBase string) error {
		return unix.Renameat2(int(fromDir.Fd()), fromBase, int(toDir.Fd()), toBase, unix.RENAME_NOREPLACE)
	})
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP) {
		return renameCheckThenMove(root, from, to)
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}

// bypassCache drops f's clean pages from the cache, so the next read of
// what was synced comes from the device.
func bypassCache(f *os.File) {
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
}

// settleFile leaves f to flushLocal: fsync flushes the drive itself on
// Linux, so each file is fsynced once, at the flush.
func settleFile(*os.File) (bool, error) { return true, nil }

// flushLocal fsyncs every pending file and changed directory together, so
// the filesystem's journal commits the batch at once.
func flushLocal(root *os.Root, files []*os.File, dirs []string) error {
	for _, f := range files {
		if err := f.Sync(); err != nil {
			return fmt.Errorf("sync %s: %w", f.Name(), err)
		}
	}
	for _, d := range dirs {
		if err := syncDir(root, d, (*os.File).Sync); err != nil {
			return err
		}
	}
	return nil
}

// dirSyncUnsupported reports a filesystem that cannot flush a directory.
func dirSyncUnsupported(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP)
}

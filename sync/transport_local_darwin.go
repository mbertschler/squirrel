package sync

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace moves from to to inside root with renameatx_np's
// RENAME_EXCL, which fails with EEXIST instead of replacing to. A
// filesystem that does not support the flag gets renameCheckThenMove.
func renameNoReplace(root *os.Root, from, to string) error {
	err := withParentDirs(root, from, to, func(fromDir, toDir *os.File, fromBase, toBase string) error {
		return unix.RenameatxNp(int(fromDir.Fd()), fromBase, int(toDir.Fd()), toBase, unix.RENAME_EXCL)
	})
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
		return renameCheckThenMove(root, from, to)
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}

// dirSyncUnsupported reports a filesystem that cannot flush a directory.
func dirSyncUnsupported(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP)
}

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

// bypassCache sets F_NOCACHE on f, so its reads and writes go to the
// device and leave no pages in the cache behind: a read of a file written
// that way returns what the disk holds.
func bypassCache(f *os.File) {
	_, _ = unix.FcntlInt(f.Fd(), unix.F_NOCACHE, 1)
}

// settleFile moves f's bytes to the device with a plain fsync. The drive's
// cache is flushed once for the whole batch by flushLocal, so f stays open
// until then.
func settleFile(f *os.File) (bool, error) {
	return true, unix.Fsync(int(f.Fd()))
}

// flushLocal fsyncs every changed directory, then flushes the drive's
// cache once with F_FULLFSYNC (os.File.Sync), which also persists
// everything fsynced on the device before it: the files settleFile
// fsynced included. A file carries that flush where there is one, since
// some filesystems cannot flush a directory.
func flushLocal(root *os.Root, files []*os.File, dirs []string) error {
	for _, d := range dirs {
		if err := syncDir(root, d, fsyncPlain); err != nil {
			return err
		}
	}
	if len(files) > 0 {
		return files[0].Sync()
	}
	if len(dirs) == 0 {
		return nil
	}
	return syncDir(root, ".", (*os.File).Sync)
}

func fsyncPlain(f *os.File) error { return unix.Fsync(int(f.Fd())) }

// dirSyncUnsupported reports a filesystem that cannot flush a directory.
func dirSyncUnsupported(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP)
}

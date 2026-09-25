//go:build !linux && !darwin

package sync

import "os"

// renameNoReplace moves from to to inside root. This platform offers
// squirrel no no-replace rename, so it checks, then renames.
func renameNoReplace(root *os.Root, from, to string) error {
	return renameCheckThenMove(root, from, to)
}

// bypassCache: this platform gives squirrel no handle on the cache, so a
// read may be served from it.
func bypassCache(*os.File) {}

// settleFile flushes f as its Put ends: this platform offers squirrel no
// flush that covers several files.
func settleFile(f *os.File) (bool, error) { return false, f.Sync() }

// flushLocal flushes every changed directory where the platform can.
func flushLocal(root *os.Root, _ []*os.File, dirs []string) error {
	for _, d := range dirs {
		if err := syncDir(root, d, (*os.File).Sync); err != nil {
			return err
		}
	}
	return nil
}

// dirSyncUnsupported: a directory cannot be flushed through a handle on
// this platform, so the sync is best effort.
func dirSyncUnsupported(error) bool { return true }

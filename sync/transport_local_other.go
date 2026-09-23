//go:build !linux && !darwin

package sync

import "os"

// renameNoReplace moves from to to inside root. This platform offers
// squirrel no no-replace rename, so it checks, then renames.
func renameNoReplace(root *os.Root, from, to string) error {
	return renameCheckThenMove(root, from, to)
}

// dirSyncUnsupported: a directory cannot be flushed through a handle on
// this platform, so the sync is best effort.
func dirSyncUnsupported(error) bool { return true }

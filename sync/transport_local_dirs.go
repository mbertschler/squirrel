//go:build linux || darwin

package sync

import (
	"os"
	"path"
	"path/filepath"
)

// withParentDirs opens the parent directories of from and to inside root
// and hands fn their handles and the two base names, for the directory
// relative system calls a no-replace rename needs.
func withParentDirs(root *os.Root, from, to string, fn func(fromDir, toDir *os.File, fromBase, toBase string) error) error {
	fromDir, err := root.Open(filepath.FromSlash(path.Dir(from)))
	if err != nil {
		return err
	}
	defer fromDir.Close()
	toDir, err := root.Open(filepath.FromSlash(path.Dir(to)))
	if err != nil {
		return err
	}
	defer toDir.Close()
	return fn(fromDir, toDir, path.Base(from), path.Base(to))
}

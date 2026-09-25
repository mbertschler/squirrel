package sync

import "strings"

// appleDouble reports whether name is the AppleDouble companion of an
// entry beside it: "._X" next to "X", where macOS keeps X's extended
// attributes on a filesystem that has none (exFAT, FAT, some SMB shares).
// The system moves and removes the companion together with X.
func appleDouble(name string, siblings map[string]bool) bool {
	x, ok := strings.CutPrefix(name, "._")
	return ok && x != "" && siblings[x]
}

// entryNames is the set of the entries' names.
func entryNames(entries []entry) map[string]bool {
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.name] = true
	}
	return names
}

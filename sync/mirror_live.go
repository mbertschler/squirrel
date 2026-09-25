package sync

import (
	"github.com/mbertschler/squirrel/store"
)

// liveRecords is the live records a push keeps current, by path, with a
// count of the records below every directory as the destination resolves
// names. Asking what lies below a path that holds nothing — every new
// path of a push asks — costs one lookup, so a push stays linear in the
// paths it writes. The writer's lock guards it.
type liveRecords struct {
	fold   nameFolding
	byPath map[string]store.RemotePath
	below  map[string]int
}

func newLiveRecords(fold nameFolding, rows map[string]store.RemotePath) *liveRecords {
	l := &liveRecords{fold: fold, byPath: make(map[string]store.RemotePath, len(rows)), below: map[string]int{}}
	for _, r := range rows {
		l.set(r)
	}
	return l
}

func (l *liveRecords) at(rel string) (store.RemotePath, bool) {
	r, ok := l.byPath[rel]
	return r, ok
}

// set records r as the live record at its path.
func (l *liveRecords) set(r store.RemotePath) {
	l.remove(r.Path)
	l.byPath[r.Path] = r
	l.countAncestors(r.Path, 1)
}

func (l *liveRecords) remove(rel string) {
	if _, ok := l.byPath[rel]; !ok {
		return
	}
	delete(l.byPath, rel)
	l.countAncestors(rel, -1)
}

// under is every live record below dir.
func (l *liveRecords) under(dir string) []store.RemotePath {
	if l.below[l.fold.key(dir)] == 0 {
		return nil
	}
	var out []store.RemotePath
	for rel, r := range l.byPath {
		if l.fold.under(rel, dir) {
			out = append(out, r)
		}
	}
	return out
}

// countAncestors adds by to the count of every directory above rel.
func (l *liveRecords) countAncestors(rel string, by int) {
	key := l.fold.key(rel)
	for i := range len(key) {
		if key[i] != '/' {
			continue
		}
		if l.below[key[:i]] += by; l.below[key[:i]] == 0 {
			delete(l.below, key[:i])
		}
	}
}

package sync

import (
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

// TestLiveRecordsMatchAScan: through random sets and removes, on a
// destination that folds names and one that doesn't, under always returns
// what a scan of every record returns, and the counts that let it answer
// in one lookup always match a recount.
func TestLiveRecordsMatchAScan(t *testing.T) {
	for _, fold := range []nameFolding{{}, caseAndNormFolding} {
		rng := rand.New(rand.NewPCG(7, 7))
		randomPath := func() string {
			elems := make([]string, 1+rng.IntN(3))
			for i := range elems {
				elems[i] = []string{"a", "A", "b", "é", "é"}[rng.IntN(5)]
			}
			return strings.Join(elems, "/")
		}
		l := newLiveRecords(fold, map[string]store.RemotePath{"a/b": {Path: "a/b"}, "b": {Path: "b"}})
		for step := range 2000 {
			if p := randomPath(); rng.IntN(3) == 0 {
				l.remove(p)
			} else {
				l.set(store.RemotePath{Path: p, ID: int64(step)})
			}
			dir := randomPath()
			if got, want := sortedPaths(l.under(dir)), scanUnder(l, dir); !slices.Equal(got, want) {
				t.Fatalf("folds=%v step %d: under(%q) = %v, want %v", fold.folds(), step, dir, got, want)
			}
		}
		if want := recountBelow(l); !maps.Equal(l.below, want) {
			t.Fatalf("folds=%v: counts = %v, want %v", fold.folds(), l.below, want)
		}
	}
}

func sortedPaths(rs []store.RemotePath) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Path)
	}
	slices.Sort(out)
	return out
}

func scanUnder(l *liveRecords, dir string) []string {
	out := []string{}
	for rel := range l.byPath {
		if l.fold.under(rel, dir) {
			out = append(out, rel)
		}
	}
	slices.Sort(out)
	return out
}

func recountBelow(l *liveRecords) map[string]int {
	out := map[string]int{}
	for rel := range l.byPath {
		key := l.fold.key(rel)
		for i := range len(key) {
			if key[i] == '/' {
				out[key[:i]]++
			}
		}
	}
	return out
}

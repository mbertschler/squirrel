package sync

import (
	"errors"
	"strings"
	"testing"
)

// TestNameGuardPermits is the table behind the name guard, the whole audit
// surface for moving or destroying bytes on a destination.
func TestNameGuardPermits(t *testing.T) {
	key := strings.Repeat("ab", 32)
	g := nameGuard{volumeDir: "pics", runID: 7, finished: func(id int64) bool { return id != 9 }}
	cases := []struct {
		name     string
		op       guardOp
		from, to string
		want     bool
	}{
		{"commit from this run's staging", opRename, "pics/.squirrel-staging/run-7/" + key, "pics/2024/cat.jpg", true},
		{"commit from another run's staging", opRename, "pics/.squirrel-staging/run-6/" + key, "pics/2024/cat.jpg", false},
		{"commit of a foreign staging name", opRename, "pics/.squirrel-staging/run-7/notes.txt", "pics/2024/cat.jpg", false},
		{"commit onto the marker", opRename, "pics/.squirrel-staging/run-7/" + key, "pics/.squirrel-volume", false},
		{"commit into history", opRename, "pics/.squirrel-staging/run-7/" + key, "pics/.squirrel-history/run-7/cat.jpg", false},
		{"commit into another volume", opRename, "pics/.squirrel-staging/run-7/" + key, "docs/cat.jpg", false},
		{"displace into this run's history", opRename, "pics/2024/cat.jpg", "pics/.squirrel-history/run-7/2024/cat.jpg", true},
		{"displace a directory", opRename, "pics/2024", "pics/.squirrel-history/run-7/2024", true},
		{"displace into another run's history", opRename, "pics/2024/cat.jpg", "pics/.squirrel-history/run-6/2024/cat.jpg", false},
		{"displace under another path", opRename, "pics/2024/cat.jpg", "pics/.squirrel-history/run-7/dog.jpg", false},
		{"move history back out", opRename, "pics/.squirrel-history/run-3/cat.jpg", "pics/.squirrel-history/run-7/.squirrel-history/run-3/cat.jpg", false},
		{"displace the marker", opRename, "pics/.squirrel-volume", "pics/.squirrel-history/run-7/.squirrel-volume", false},
		{"rename between live names", opRename, "pics/a.jpg", "pics/b.jpg", false},
		{"escape the volume", opRename, "pics/../x", "pics/.squirrel-history/run-7/../x", false},
		{"remove a finished run's staging", opRemove, "pics/.squirrel-staging/run-6/" + key, "", true},
		{"remove a finished run's staging directory", opRemove, "pics/.squirrel-staging/run-6", "", true},
		{"remove this run's staging", opRemove, "pics/.squirrel-staging/run-7/" + key, "", false},
		{"remove a running run's staging", opRemove, "pics/.squirrel-staging/run-9/" + key, "", false},
		{"remove a foreign file in staging", opRemove, "pics/.squirrel-staging/run-6/notes.txt", "", false},
		{"remove a malformed run directory", opRemove, "pics/.squirrel-staging/run-06/" + key, "", false},
		{"remove a snapshot", opRemove, "pics/.squirrel-index/index-20260101T000000.000Z-run-6.db", "", true},
		{"remove a receipt", opRemove, "pics/.squirrel-index/run-6", "", false},
		{"remove a live file", opRemove, "pics/2024/cat.jpg", "", false},
		{"remove history", opRemove, "pics/.squirrel-history/run-6/cat.jpg", "", false},
		{"remove another volume's staging", opRemove, "docs/.squirrel-staging/run-6/" + key, "", false},
		{"ride a snapshot along from this run's staging", opRename, "pics/.squirrel-staging/run-7/" + key, "pics/.squirrel-index/index-20260101T000000.000Z-run-7.db", true},
		{"stage onto a receipt", opRename, "pics/.squirrel-staging/run-7/" + key, "pics/.squirrel-index/run-7", false},
		{"ride a snapshot along from another run's staging", opRename, "pics/.squirrel-staging/run-6/" + key, "pics/.squirrel-index/index-20260101T000000.000Z-run-7.db", false},
		{"move a live file onto a snapshot name", opRename, "pics/a.db", "pics/.squirrel-index/index-20260101T000000.000Z-run-7.db", false},
		{"marker bootstrap without --init", opRename, "pics/.squirrel-staging/volume-marker", "pics/.squirrel-volume", false},
		{"remove the staged marker without --init", opRemove, "pics/.squirrel-staging/volume-marker", "", false},
		{"remove the name probe", opRemove, foldProbeName("pics"), "", true},
		{"remove the name probe decomposed", opRemove, "pics/.squirrel-staging/" + foldProbeNorm, "", false},
		{"remove another volume's name probe", opRemove, foldProbeName("docs"), "", false},
		{"commit the name probe", opRename, foldProbeName("pics"), "pics/a.txt", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := (nameGuard{volumeDir: "pics", finished: g.finished}).permit(c.op, c.from, c.to); err == nil {
				t.Fatal("a guard no run holds permitted the operation")
			}
			err := g.permit(c.op, c.from, c.to)
			if got := err == nil; got != c.want {
				t.Fatalf("permit = %v, want allowed=%t", err, c.want)
			}
			if err != nil && !errors.Is(err, errGuardRefused) {
				t.Fatalf("refusal %v does not wrap errGuardRefused", err)
			}
		})
	}
}

// TestNameGuardContentLayout: a content layout's guard commits this run's
// staging onto artifact names alone, and displaces nothing.
func TestNameGuardContentLayout(t *testing.T) {
	key := strings.Repeat("ab", 32)
	staged := "pics/.squirrel-staging/run-7/" + key
	g := nameGuard{volumeDir: "pics", runID: 7, content: true, finished: func(id int64) bool { return id != 7 }}
	cases := []struct {
		name     string
		op       guardOp
		from, to string
		want     bool
	}{
		{"commit an object", opRename, staged, "objects/" + key, true},
		{"commit a pack", opRename, staged, "packs/" + key, true},
		{"commit this run's placement map", opRename, staged, "packs/map-7", true},
		{"commit another run's placement map", opRename, staged, "packs/map-6", false},
		{"commit this run's segment", opRename, staged, "pics/index/run-7", true},
		{"commit another run's segment", opRename, staged, "pics/index/run-6", false},
		{"commit another volume's segment", opRename, staged, "docs/index/run-7", false},
		{"commit an object from another run's staging", opRename, "pics/.squirrel-staging/run-6/" + key, "objects/" + key, false},
		{"commit an object under a name that is no hash", opRename, staged, "objects/cat.jpg", false},
		{"commit an object below objects", opRename, staged, "objects/ab/" + key, false},
		{"commit onto the mirrored tree", opRename, staged, "pics/2024/cat.jpg", false},
		{"ride a snapshot along", opRename, staged, "pics/.squirrel-index/index-20260101T000000.000Z-run-7.db", true},
		{"displace a segment", opRename, "pics/index/run-6", "pics/.squirrel-history/run-7/index/run-6", false},
		{"move an object", opRename, "objects/" + key, "packs/" + key, false},
		{"remove an object", opRemove, "objects/" + key, "", false},
		{"remove a finished run's staging", opRemove, "pics/.squirrel-staging/run-6/" + key, "", true},
		{"remove a mirror's name probe", opRemove, foldProbeName("pics"), "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := g.permit(c.op, c.from, c.to)
			if got := err == nil; got != c.want {
				t.Fatalf("permit = %v, want allowed=%t", err, c.want)
			}
		})
	}
	mirror := nameGuard{volumeDir: "pics", runID: 7}
	if err := mirror.permit(opRename, staged, "objects/"+key); err == nil {
		t.Fatal("a mirror's guard committed onto an object name")
	}
}

// TestNameGuardBootstrap: under --init the guard also lets the marker be
// staged, renamed onto .squirrel-volume, and a stale staged copy removed —
// run or not — and nothing more.
func TestNameGuardBootstrap(t *testing.T) {
	cases := []struct {
		name     string
		op       guardOp
		from, to string
		want     bool
	}{
		{"rename the staged marker onto the marker", opRename, "pics/.squirrel-staging/volume-marker", "pics/.squirrel-volume", true},
		{"remove a stale staged marker", opRemove, "pics/.squirrel-staging/volume-marker", "", true},
		{"rename the staged marker elsewhere", opRename, "pics/.squirrel-staging/volume-marker", "pics/a.txt", false},
		{"rename something else onto the marker", opRename, "pics/a.txt", "pics/.squirrel-volume", false},
		{"remove the marker", opRemove, "pics/.squirrel-volume", "", false},
		{"another volume's staged marker", opRename, "docs/.squirrel-staging/volume-marker", "docs/.squirrel-volume", false},
		{"remove a live file", opRemove, "pics/a.txt", "", false},
	}
	g := nameGuard{volumeDir: "pics", bootstrap: true}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := g.permit(c.op, c.from, c.to)
			if got := err == nil; got != c.want {
				t.Fatalf("permit = %v, want allowed=%t", err, c.want)
			}
		})
	}
}

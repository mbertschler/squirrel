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

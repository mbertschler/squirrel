package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

func (f *mirrorFixture) verify(t *testing.T) RemoteVerifyReport {
	t.Helper()
	rep, err := VerifyRemote(context.Background(), f.store, nil, f.pair.Destination)
	if err != nil {
		t.Fatalf("VerifyRemote: %v", err)
	}
	return rep
}

// TestVerifyMirrorChecksEveryStoredCopy: a pass checks the live and the
// displaced copies without rclone, re-reads a slice of them on a local
// disk, and stays clean while they hold what squirrel stored.
func TestVerifyMirrorChecksEveryStoredCopy(t *testing.T) {
	f := setupMirrorFixture(t)
	f.write(t, "a.txt", "v1")
	f.write(t, "b.txt", "beta")
	f.index(t)
	f.mustPush(t)
	f.write(t, "a.txt", "v2")
	f.index(t)
	f.mustPush(t)

	rep := f.verify(t)
	if !rep.Clean() || rep.Paths != 3 || rep.PathsReread == 0 || rep.RunID == 0 {
		t.Fatalf("rep = %+v, want a clean pass over 3 copies that re-read some", rep)
	}
	run, err := f.store.GetRun(context.Background(), rep.RunID)
	if err != nil || run.Status != store.RunStatusSuccess || run.FileCount != 3 {
		t.Fatalf("run = %+v, %v; want a successful audit over 3 copies", run, err)
	}
}

// TestVerifyMirrorRereadRotates: successive passes re-read the least
// recently verified copies first, so every copy is re-read in turn.
func TestVerifyMirrorRereadRotates(t *testing.T) {
	f := setupMirrorFixture(t)
	for _, name := range []string{"a", "b", "c", "d"} {
		f.write(t, name, "same size")
	}
	f.index(t)
	f.mustPush(t)
	before := map[int64]int64{}
	for _, r := range f.rows(t) {
		before[r.ID] = r.VerifiedAtNs.Int64
	}
	for range 4 {
		if rep := f.verify(t); rep.PathsReread != 1 {
			t.Fatalf("pass re-read %d copies, want the one a tenth of the bytes needs", rep.PathsReread)
		}
	}
	for _, r := range f.rows(t) {
		if r.VerifiedAtNs.Int64 == before[r.ID] {
			t.Fatalf("%s was never re-read in four passes", r.Path)
		}
	}
}

// TestVerifyMirrorFindingsMarkCopiesLost: a copy deleted, changed in size
// and mtime, or changed behind an unchanged size and mtime is a finding:
// its row becomes lost, the destination's alarm latches, and the next push
// writes each path again although the index did not change.
func TestVerifyMirrorFindingsMarkCopiesLost(t *testing.T) {
	f := setupMirrorFixture(t)
	f.write(t, "gone.txt", "gone")
	f.write(t, "grown.txt", "grown")
	f.write(t, "flipped.txt", "flipped")
	f.index(t)
	f.mustPush(t)

	if err := os.Remove(f.dest("gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.dest("grown.txt"), []byte("grown, and longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	flipped := f.dest("flipped.txt")
	fi, _ := os.Stat(flipped)
	if err := os.WriteFile(flipped, []byte("FLIPPED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(flipped, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	found := f.verify(t)
	if found.Clean() || len(found.PathsMissing) != 1 || len(found.PathsChanged) != 2 || !found.AlarmRaised {
		t.Fatalf("rep = %+v, want one copy missing, two changed, and the alarm raised", found)
	}
	if got := f.lostPaths(t); len(got) != 3 {
		t.Fatalf("lost = %v, want all three copies", got)
	}

	rep := f.mustPush(t)
	if rep.RcloneResult.Transferred != 3 {
		t.Fatalf("repair push transferred %d, want the three lost paths", rep.RcloneResult.Transferred)
	}
	if f.readDest(t, "gone.txt") != "gone" || f.readDest(t, "grown.txt") != "grown" || f.readDest(t, "flipped.txt") != "flipped" {
		t.Fatal("the repair push did not restore the lost copies")
	}
	f.checkInvariants(t, nil)
	if rep := f.verify(t); !rep.Clean() {
		t.Fatalf("pass after the repair = %+v, want clean", rep)
	}
}

func (f *mirrorFixture) lostPaths(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, r := range f.rows(t) {
		if r.State == store.RemotePathLost {
			out = append(out, r.Path)
		}
	}
	return out
}

// TestVerifyMirrorOverSFTPChecksWithoutReading: over sftp a pass checks
// size and mtime alone, and finds a copy that went missing.
func TestVerifyMirrorOverSFTPChecksWithoutReading(t *testing.T) {
	f := setupMirrorFixtureOn(t, sftpBackend)
	f.write(t, "a.txt", "alpha")
	f.write(t, "b.txt", "beta")
	f.index(t)
	f.mustPush(t)
	if rep := f.verify(t); !rep.Clean() || rep.Paths != 2 || rep.PathsReread != 0 {
		t.Fatalf("rep = %+v, want a clean size-and-mtime pass", rep)
	}
	if err := os.Remove(filepath.Join(f.dst, "pics", "a.txt")); err != nil {
		t.Fatal(err)
	}
	rep := f.verify(t)
	if len(rep.PathsMissing) != 1 || rep.PathsMissing[0] != "pics/a.txt" {
		t.Fatalf("missing = %v, want pics/a.txt", rep.PathsMissing)
	}
}

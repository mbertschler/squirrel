package sync

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/volmark"
)

// TestMirrorRefusesATreeItHasNoRecordOf: a volume's first push to a native
// mirror refuses a volume directory that already holds files squirrel has
// no record of writing — a tree rclone wrote, or a native mirror whose
// index is gone — and a dry run refuses the same. Nothing moves. Another
// volume on the same root still starts fresh, and so does one whose
// directory holds only its marker and staging.
func TestMirrorRefusesATreeItHasNoRecordOf(t *testing.T) {
	for _, b := range mirrorBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupMirrorFixtureOn(t, b)
			f.write(t, "a.txt", "alpha")
			f.index(t)
			if err := os.WriteFile(f.dest("a.txt"), []byte("written by another tool"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, dryRun := range []bool{true, false} {
				_, err := f.push(t, Options{DryRun: dryRun})
				if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "squirrel recover --from usb") {
					t.Fatalf("push (dry run %v) = %v, want a refusal pointing at recover", dryRun, err)
				}
			}
			if f.readDest(t, "a.txt") != "written by another tool" {
				t.Fatal("the refused push moved the tree it found")
			}
			if _, err := os.Stat(f.dest(HistoryDirName)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the refused push displaced something: %v", err)
			}

			if err := os.Remove(f.dest("a.txt")); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(f.dest(StagingDirName+"/run-99"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.dest(StagingDirName+"/run-99/"+stagingKey("a.txt")), []byte("partial"), 0o644); err != nil {
				t.Fatal(err)
			}
			f.mustPush(t)
			f.checkInvariants(t, nil)
		})
	}
}

// TestMirrorSecondVolumeStartsFreshBesideAnother: a volume's first push to
// a root that already holds another volume's mirror is no adoption: it
// starts fresh in its own directory.
func TestMirrorSecondVolumeStartsFreshBesideAnother(t *testing.T) {
	f := setupMirrorFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if err := os.MkdirAll(filepath.Join(f.dst, "docs", "2024"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := volmark.Write(filepath.Join(f.dst, "docs"), volmark.Marker{Volume: "docs"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dst, "docs", "2024", "invoice.pdf"), []byte("another volume's"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.mustPush(t)
	f.checkInvariants(t, nil)
}

// TestMirrorRetriesAFirstPushThatCrashed: a first push that died after
// committing some paths left records, so its retry is no adoption: it
// settles what the crash left and lands the rest.
func TestMirrorRetriesAFirstPushThatCrashed(t *testing.T) {
	for _, b := range mirrorBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupMirrorFixtureOn(t, b)
			f.write(t, "a.txt", "alpha")
			f.write(t, "b.txt", "beta")
			f.index(t)
			commits := 0
			if rep, err := f.pushCrashing(t, func(c transportCall) bool {
				if c.op == "rename" && strings.Contains(c.name, StagingDirName+"/run-") {
					commits++
				}
				return commits == 2
			}, crashAfter); !crashedOn(rep, err) {
				t.Fatalf("crashing first push = %v, want the injected crash", err)
			}
			f.mustPush(t)
			f.checkInvariants(t, nil)
		})
	}
}

// TestMirrorLeavesAppleDoubleCompanionsToTheSystem: the "._X" files macOS
// keeps beside X on a disk without extended attributes are X's, not
// foreign: one beside the marker leaves a root empty, one beside a
// finished run's staging is not reported, and an index-less restore leaves
// them out, counted once, while an orphan with no X beside it is restored
// like any file no receipt names.
func TestMirrorLeavesAppleDoubleCompanionsToTheSystem(t *testing.T) {
	for _, b := range mirrorBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupMirrorFixtureOn(t, b)
			if err := os.WriteFile(f.dest("._"+volmark.MarkerName), []byte("attributes"), 0o644); err != nil {
				t.Fatal(err)
			}
			f.write(t, "a.txt", "alpha")
			f.index(t)
			first := f.mustPush(t)
			run := "run-" + strconv.FormatInt(first.RunID, 10)
			for _, rel := range []string{StagingDirName + "/._" + run, "._a.txt", "._" + HistoryDirName, "._orphan.txt"} {
				if err := os.WriteFile(f.dest(rel), []byte("attributes"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(f.dest(HistoryDirName), 0o755); err != nil {
				t.Fatal(err)
			}
			f.write(t, "b.txt", "beta")
			f.index(t)
			if rep := f.mustPush(t); warned(rep, "._"+run) {
				t.Fatalf("warnings = %v, want the companion of finished staging left alone", rep.Warnings)
			}

			to := t.TempDir()
			fresh, err := store.Open(filepath.Join(t.TempDir(), "fresh.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { fresh.Close() })
			rep, err := Restore(t.Context(), fresh, nil, f.pair.Volume, f.pair.Destination, RestoreOptions{ToPath: to})
			if err != nil || rep.Status != store.RunStatusSuccess {
				t.Fatalf("restore: status=%q err=%v", rep.Status, err)
			}
			if !warned(rep, "3 AppleDouble file(s)") || !warned(rep, "1 file(s) on \"usb\" are named in no receipt") {
				t.Fatalf("warnings = %v, want three companions left out and the orphan restored unchecked", rep.Warnings)
			}
			for rel, want := range map[string]bool{"a.txt": true, "b.txt": true, "._orphan.txt": true, "._a.txt": false, "._" + HistoryDirName: false, "._" + volmark.MarkerName: false} {
				if _, err := os.Stat(filepath.Join(to, rel)); (err == nil) != want {
					t.Errorf("%s restored = %v, want %v", rel, err == nil, want)
				}
			}
		})
	}
}

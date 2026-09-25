package sync

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/blake3"
)

func blake3Of(s string) []byte {
	sum := blake3.Sum256([]byte(s))
	return sum[:]
}

// placeFixture is a target directory whose a.txt holds "old".
func placeFixture(t *testing.T, preserve bool) (restorePlacer, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	return restorePlacer{target: dir, runID: 7, preserve: preserve}, dir
}

// onlyFiles asserts dir holds exactly the named entries: no temporary
// file was left behind.
func onlyFiles(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s holds %v, want %v", dir, got, want)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPlaceNeverLeavesAPartialFile: bytes that stop mid-stream, or that
// fail their check, never reach the path, and no temporary file stays.
func TestPlaceNeverLeavesAPartialFile(t *testing.T) {
	pl, dir := placeFixture(t, false)
	broken := io.MultiReader(strings.NewReader("new"), iotestErrReader{})
	if _, err := pl.place("a.txt", broken, nil, time.Now()); err == nil {
		t.Fatal("place of a broken stream succeeded")
	}
	if _, err := pl.place("a.txt", strings.NewReader("new"), blake3Of("other"), time.Now()); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("place of mismatching bytes = %v, want a hash refusal", err)
	}
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "old" {
		t.Fatalf("a.txt = %q after refused places, want old", got)
	}
	onlyFiles(t, dir, "a.txt")
}

type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, errors.New("the stream broke") }

// TestPlaceReplacesOrPreserves: a scratch restore renames the new bytes
// over the path; an in-place one first moves the old file into the run's
// restore history. Either way the path ends up with the new bytes and
// their mtime.
func TestPlaceReplacesOrPreserves(t *testing.T) {
	mtime := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	for _, preserve := range []bool{false, true} {
		pl, dir := placeFixture(t, preserve)
		n, err := pl.place("a.txt", strings.NewReader("new!"), blake3Of("new!"), mtime)
		if err != nil || n != 4 {
			t.Fatalf("preserve=%v: place = %d, %v", preserve, n, err)
		}
		dst := filepath.Join(dir, "a.txt")
		fi, err := os.Stat(dst)
		if err != nil || readFile(t, dst) != "new!" || !fi.ModTime().Equal(mtime) {
			t.Fatalf("preserve=%v: a.txt = %q at %v, want new! at %v", preserve, readFile(t, dst), fi.ModTime(), mtime)
		}
		if !preserve {
			onlyFiles(t, dir, "a.txt")
			continue
		}
		onlyFiles(t, dir, RestoreHistoryDirName, "a.txt")
		if got := readFile(t, filepath.Join(dir, RestoreHistoryDirName, "run-7", "a.txt")); got != "old" {
			t.Fatalf("restore history holds %q, want the replaced old", got)
		}
	}
}

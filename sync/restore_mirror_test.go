package sync

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

func (f *mirrorFixture) restore(t *testing.T, s *store.Store, opts RestoreOptions) (Report, error) {
	t.Helper()
	return Restore(context.Background(), s, nil, f.pair.Volume, f.pair.Destination, opts)
}

// scratchFile reads rel under a restore target, "" when it is absent.
func scratchFile(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestMirrorRestoreFromTheIndex: with an index, a native mirror restore
// fetches each present path through the transport with no rclone, checks
// it against the indexed hash, and leaves a path that already holds its
// bytes alone. A mirrored file whose bytes no longer match the index is
// refused, never placed.
func TestMirrorRestoreFromTheIndex(t *testing.T) {
	for _, b := range mirrorBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupMirrorFixtureOn(t, b)
			f.write(t, "a.txt", "alpha")
			f.write(t, "2024/cat.jpg", "meow")
			f.write(t, "b.txt", "beta")
			f.index(t)
			f.mustPush(t)

			scratch := t.TempDir()
			if err := os.WriteFile(filepath.Join(scratch, "a.txt"), []byte("alpha"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.dest("b.txt"), []byte("BETA"), 0o644); err != nil {
				t.Fatal(err)
			}
			rep, err := f.restore(t, f.store, RestoreOptions{ToPath: scratch})
			if err == nil || rep.RcloneResult.Errors != 1 || rep.RcloneResult.FailedFiles[0].Object != "b.txt" {
				t.Fatalf("restore = %v, failures %+v; want b.txt refused", err, rep.RcloneResult.FailedFiles)
			}
			if rep.RcloneResult.Transferred != 1 || rep.AlreadyCorrect != 1 {
				t.Fatalf("counters = %+v already_correct=%d, want cat.jpg fetched and a.txt left alone", rep.RcloneResult, rep.AlreadyCorrect)
			}
			if scratchFile(t, scratch, "2024/cat.jpg") != "meow" || scratchFile(t, scratch, "b.txt") != "" {
				t.Fatal("restore placed the wrong bytes")
			}
		})
	}
}

// TestMirrorRestoreOnAFreshMachine: with no index, a restore walks the
// mirrored tree, leaves squirrel's reserved entries out, checks every file
// a receipt names against it, and restores the rest unchecked, counting
// them in a warning. A file that contradicts its receipt is refused.
func TestMirrorRestoreOnAFreshMachine(t *testing.T) {
	for _, b := range mirrorBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupMirrorFixtureOn(t, b)
			f.write(t, "a.txt", "v1")
			f.write(t, "2024/cat.jpg", "meow")
			f.write(t, "c.txt", "gamma")
			f.index(t)
			f.mustPush(t)
			f.write(t, "a.txt", "v2")
			f.index(t)
			f.mustPush(t)
			if err := os.WriteFile(f.dest("foreign.txt"), []byte("not squirrel's"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.dest("c.txt"), []byte("GAMMA"), 0o644); err != nil {
				t.Fatal(err)
			}

			fresh, err := store.Open(filepath.Join(t.TempDir(), "fresh.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { fresh.Close() })
			scratch := t.TempDir()
			rep, err := f.restore(t, fresh, RestoreOptions{ToPath: scratch})
			if err == nil || rep.RcloneResult.Errors != 1 || rep.RcloneResult.FailedFiles[0].Object != "c.txt" {
				t.Fatalf("restore = %v, failures %+v; want c.txt refused", err, rep.RcloneResult.FailedFiles)
			}
			for rel, want := range map[string]string{"a.txt": "v2", "2024/cat.jpg": "meow", "foreign.txt": "not squirrel's", "c.txt": ""} {
				if got := scratchFile(t, scratch, rel); got != want {
					t.Fatalf("%s = %q, want %q", rel, got, want)
				}
			}
			entries, _ := os.ReadDir(scratch)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".squirrel") {
					t.Fatalf("restore brought back squirrel's own %s", e.Name())
				}
			}
			if !warned(rep, "1 file(s)") || !warned(rep, "restored unchecked") {
				t.Fatalf("warnings = %v, want the one unchecked file counted", rep.Warnings)
			}
		})
	}
}

// TestMirrorRestoreInPlacePreservesWhatItReplaces: an in-place restore
// moves every file it replaces into the run's restore history, and
// leaves a file that already holds its indexed bytes where it is.
func TestMirrorRestoreInPlacePreservesWhatItReplaces(t *testing.T) {
	f := setupMirrorFixtureOn(t, localBackend)
	f.write(t, "a.txt", "alpha")
	f.write(t, "b.txt", "beta")
	f.index(t)
	f.mustPush(t)
	f.write(t, "b.txt", "local edit")

	rep, err := f.restore(t, f.store, RestoreOptions{InPlace: true})
	if err != nil || rep.RcloneResult.Transferred != 1 || rep.AlreadyCorrect != 1 {
		t.Fatalf("restore = %v, counters %+v already_correct=%d; want b.txt replaced and a.txt left alone", err, rep.RcloneResult, rep.AlreadyCorrect)
	}
	if scratchFile(t, f.src, "b.txt") != "beta" {
		t.Fatal("b.txt was not restored")
	}
	history := filepath.Join(RestoreHistoryDirName, "run-"+strconv.FormatInt(rep.RunID, 10), "b.txt")
	if got := scratchFile(t, f.src, history); got != "local edit" {
		t.Fatalf("restore history holds %q, want the replaced local edit", got)
	}
}

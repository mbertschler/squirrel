package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/volmark"
)

// plantSymlink replaces what the destination holds at rel with a symlink
// to target.
func (f *mirrorFixture) plantSymlink(t *testing.T, rel, target string) {
	t.Helper()
	if err := os.RemoveAll(f.dest(rel)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.dest(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, f.dest(rel)); err != nil {
		t.Fatal(err)
	}
}

// outsideDir is a directory beside the destination that a symlink can
// point squirrel at; checkUntouched asserts nothing was written there.
func outsideDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "precious.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func checkUntouched(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "precious.txt" {
		t.Fatalf("outside directory holds %v, want only precious.txt", entries)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "precious.txt")); string(b) != "outside" {
		t.Fatalf("precious.txt = %q, want it unchanged", b)
	}
}

// TestMirrorNeverFollowsASymlinkAtATarget: a symlink the destination holds
// where a path, or a directory along it, should be is never followed. It
// moves into history as bytes squirrel did not write, a record it replaced
// becomes lost, and the path lands in a real directory; nothing is written
// where the symlink points.
func TestMirrorNeverFollowsASymlinkAtATarget(t *testing.T) {
	for _, b := range mirrorBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupMirrorFixtureOn(t, b)
			f.write(t, "dir/old.txt", "old")
			f.write(t, "a.txt", "a1")
			f.index(t)
			f.mustPush(t)
			outside := outsideDir(t)
			f.plantSymlink(t, "dir", outside)
			f.plantSymlink(t, "a.txt", filepath.Join(outside, "precious.txt"))
			f.write(t, "dir/new.txt", "new")
			f.write(t, "a.txt", "a2")
			f.index(t)

			rep := f.mustPush(t)
			checkUntouched(t, outside)
			if f.readDest(t, "dir/new.txt") != "new" || f.readDest(t, "a.txt") != "a2" {
				t.Fatal("the paths did not land in real entries")
			}
			run := strconv.FormatInt(rep.RunID, 10)
			for _, rel := range []string{"dir", "a.txt"} {
				if fi, err := os.Lstat(f.dest(HistoryDirName + "/run-" + run + "/" + rel)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("history/%s = %v, %v; want the symlink itself preserved", rel, fi, err)
				}
			}
			for _, rel := range []string{"dir/old.txt", "a.txt"} {
				if got := f.rowsAt(t, rel); len(got) == 0 || got[0] != store.RemotePathLost {
					t.Fatalf("%s rows = %v, want its first row lost", rel, got)
				}
			}
			f.checkRecordsVouch(t)

			f.mustPush(t)
			if f.readDest(t, "dir/old.txt") != "old" {
				t.Fatal("the next push did not repair the path the symlink took away")
			}
			f.checkInvariants(t, nil)
		})
	}
}

// TestMirrorNeverFollowsASymlinkedReservedDirectory: a symlink in place of
// the volume directory, or of its history, staging or index directory,
// fails the push instead of being followed, and nothing is written where
// it points. Once the symlink is gone, a clean push settles everything.
func TestMirrorNeverFollowsASymlinkedReservedDirectory(t *testing.T) {
	for _, b := range mirrorBackends {
		for _, rel := range []string{HistoryDirName, StagingDirName, IndexDirName, "."} {
			t.Run(b.name+"/"+rel, func(t *testing.T) {
				f := setupMirrorFixtureOn(t, b)
				f.write(t, "a.txt", "v1")
				f.index(t)
				f.mustPush(t)
				f.write(t, "a.txt", "v2, longer")
				f.index(t)
				outside := outsideDir(t)
				real := f.dest(rel) + ".real"
				if err := os.Rename(f.dest(rel), real); err != nil && !errors.Is(err, fs.ErrNotExist) {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, f.dest(rel)); err != nil {
					t.Fatal(err)
				}
				if rel == "." {
					if err := volmark.Write(outside, volmark.Marker{Volume: "pics"}); err != nil {
						t.Fatal(err)
					}
				}

				if rep, err := f.push(t, Options{}); err == nil || rep.Status == store.RunStatusSuccess {
					t.Fatalf("push through a symlinked %s: status=%q err=%v, want it failed", rel, rep.Status, err)
				}
				if rel == "." {
					_ = os.Remove(filepath.Join(outside, volmark.MarkerName))
				}
				checkUntouched(t, outside)
				if err := os.Remove(f.dest(rel)); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(real, f.dest(rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					t.Fatal(err)
				}
				f.checkRecordsVouch(t)
				f.mustPush(t)
				if f.readDest(t, "a.txt") != "v2, longer" {
					t.Fatal("the push after the symlink went did not land the change")
				}
				f.checkInvariants(t, nil)
			})
		}
	}
}

// TestMirrorNestedFileDirectorySwaps: a file deep in the tree that became a
// directory holding a deeper file, and a directory tree that became a
// file, each move aside as their records — a replaced directory with every
// version recorded under it, a version changed behind squirrel's back
// losing its record, and bytes squirrel never wrote kept beside them.
func TestMirrorNestedFileDirectorySwaps(t *testing.T) {
	for _, b := range mirrorBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupMirrorFixtureOn(t, b)
			f.write(t, "a/b/c", "file c")
			f.write(t, "x/y/z", "file z")
			f.write(t, "x/y/w", "file w")
			f.write(t, "x/v", "file v")
			f.index(t)
			f.mustPush(t)
			for _, rel := range []string{"a/b/c", "x"} {
				if err := os.RemoveAll(filepath.Join(f.src, rel)); err != nil {
					t.Fatal(err)
				}
			}
			f.write(t, "a/b/c/d/e", "deep under c")
			f.write(t, "x", "x is a file")
			f.index(t)
			if err := os.WriteFile(f.dest("x/y/w"), []byte("changed on the destination"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.dest("x/y/foreign"), []byte("not squirrel's"), 0o644); err != nil {
				t.Fatal(err)
			}
			before := f.contentHashes(t)

			rep := f.mustPush(t)
			run := HistoryDirName + "/run-" + strconv.FormatInt(rep.RunID, 10) + "/"
			if f.readDest(t, "a/b/c/d/e") != "deep under c" || f.readDest(t, "x") != "x is a file" {
				t.Fatal("the swapped paths did not land")
			}
			for rel, want := range map[string]string{"a/b/c": "file c", "x/y/z": "file z", "x/v": "file v", "x/y/foreign": "not squirrel's", "x/y/w": "changed on the destination"} {
				if got := f.readDest(t, run+rel); got != want {
					t.Errorf("history %s = %q, want %q", rel, got, want)
				}
			}
			for rel, want := range map[string]string{"a/b/c": store.RemotePathDisplaced, "x/y/z": store.RemotePathDisplaced, "x/v": store.RemotePathDisplaced, "x/y/w": store.RemotePathLost} {
				if got := f.rowsAt(t, rel); !slicesEqual(got, []string{want}) {
					t.Errorf("%s rows = %v, want %s", rel, got, want)
				}
			}
			f.checkInvariants(t, before)
		})
	}
}

// TestMirrorLeavesForeignStagingAlone: whatever squirrel did not name in
// staging — a file or directory at its top, a run directory of a run the
// index does not know, a symlink or directory in a finished run's staging
// — is reported and never removed, while the finished run's own staging
// beside it goes.
func TestMirrorLeavesForeignStagingAlone(t *testing.T) {
	for _, b := range mirrorBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupMirrorFixtureOn(t, b)
			f.write(t, "a.txt", "alpha")
			f.index(t)
			first := f.mustPush(t)
			finished := StagingDirName + "/run-" + strconv.FormatInt(first.RunID, 10) + "/"
			key := stagingKey("a.txt")
			foreign := []string{StagingDirName + "/notes.txt", StagingDirName + "/mine/x", StagingDirName + "/run-999999/" + key, finished + key + "0/x"}
			for _, rel := range foreign {
				if err := os.MkdirAll(filepath.Dir(f.dest(rel)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.dest(rel), []byte("foreign"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outsideDir(t), f.dest(finished+"link")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.dest(finished+key), []byte("a crashed run's partial copy"), 0o644); err != nil {
				t.Fatal(err)
			}

			f.write(t, "a.txt", "alpha, changed")
			f.index(t)
			rep := f.mustPush(t)
			for _, name := range []string{"notes.txt", "mine", "run-999999", key + "0", "link"} {
				if !warned(rep, name+" is in squirrel's staging") {
					t.Errorf("warnings = %v, want %s reported", rep.Warnings, name)
				}
			}
			for _, rel := range append(foreign, finished+"link") {
				if _, err := os.Lstat(f.dest(rel)); err != nil {
					t.Errorf("%s was removed: %v", rel, err)
				}
			}
			if _, err := os.Stat(f.dest(finished + key)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the finished run's own staged copy is still there: %v", err)
			}
		})
	}
}

// fullTransport fails a Put that full selects once it has taken limit
// bytes, as a disk that fills up, or a quota that runs out, mid-write.
type fullTransport struct {
	transport
	full  func(name string) bool
	limit int64
	errno syscall.Errno
}

func (f fullTransport) Put(ctx context.Context, name string, r io.Reader, mtime time.Time) error {
	if !f.full(name) {
		return f.transport.Put(ctx, name, r, mtime)
	}
	return f.transport.Put(ctx, name, &fillingReader{r: r, left: f.limit, errno: f.errno}, mtime)
}

type fillingReader struct {
	r     io.Reader
	left  int64
	errno syscall.Errno
}

func (f *fillingReader) Read(p []byte) (int, error) {
	if f.left <= 0 {
		return 0, &fs.PathError{Op: "write", Path: "staged", Err: f.errno}
	}
	if int64(len(p)) > f.left {
		p = p[:f.left]
	}
	n, err := f.r.Read(p)
	f.left -= int64(n)
	return n, err
}

// TestMirrorDiskFullMidWrite: a destination that fills up, or runs out of
// quota, part way through a path's staged copy, the name probe or the
// receipt fails the push with the disk's error. Nothing reaches a path,
// every version already there stays, and once there is room again a clean
// push lands everything and clears the partial copy.
func TestMirrorDiskFullMidWrite(t *testing.T) {
	staged := func(n string) bool { return strings.Contains(n, StagingDirName+"/run-") }
	cases := []struct {
		name  string
		full  func(string) bool
		limit int64
		errno syscall.Errno
	}{
		{"staged copy, disk full", staged, 100, syscall.ENOSPC},
		{"staged copy, quota", staged, 100, syscall.EDQUOT},
		{"name probe", func(n string) bool { return strings.HasSuffix(n, foldProbeBase) }, 0, syscall.ENOSPC},
		{"receipt", func(n string) bool { return strings.Contains(n, IndexDirName+"/run-") }, 100, syscall.ENOSPC},
	}
	for _, b := range mirrorBackends {
		for _, c := range cases {
			t.Run(b.name+"/"+c.name, func(t *testing.T) {
				f := setupMirrorFixtureOn(t, b)
				f.write(t, "a.txt", "v1")
				f.index(t)
				f.mustPush(t)
				f.write(t, "a.txt", strings.Repeat("v2 ", 1000))
				f.write(t, "b.txt", strings.Repeat("new ", 1000))
				f.index(t)
				before := f.contentHashes(t)
				rep, err := f.pushVia(t, Options{}, func(tr transport) transport {
					return fullTransport{transport: tr, full: c.full, limit: c.limit, errno: c.errno}
				})
				if err == nil || rep.Status == store.RunStatusSuccess {
					t.Fatalf("push onto a full disk: status=%q err=%v, want it failed", rep.Status, err)
				}
				if msg := err.Error() + fmt.Sprint(rep.RcloneResult.FailedFiles); !strings.Contains(msg, c.errno.Error()) {
					t.Fatalf("failure %q, want the disk's error %q named", msg, c.errno.Error())
				}
				if c.name != "receipt" && f.readDest(t, "a.txt") != "v1" {
					t.Fatal("a path changed although its new version never fully landed")
				}
				f.checkRecordsVouch(t)
				f.checkOnlyGrew(t, before)

				clean := f.mustPush(t)
				if f.readDest(t, "a.txt") != strings.Repeat("v2 ", 1000) || f.readDest(t, "b.txt") != strings.Repeat("new ", 1000) {
					t.Fatal("the push with room again did not land everything")
				}
				f.checkInvariants(t, before)
				f.checkStagingEmpty(t, clean.RunID)
			})
		}
	}
}

// TestMirrorHistoryUnwritable: when the history directory can't be
// written, a path whose recorded version must move there fails and keeps
// that version in place; nothing else is displaced for it. Once history is
// writable again, the next push settles the interrupted move and lands the
// change.
func TestMirrorHistoryUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	for _, b := range mirrorBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupMirrorFixtureOn(t, b)
			f.write(t, "a.txt", "v1")
			f.index(t)
			f.mustPush(t)
			f.write(t, "a.txt", "v2")
			f.index(t)
			f.mustPush(t)
			history := f.dest(HistoryDirName)
			if err := os.Chmod(history, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(history, 0o755) })
			f.write(t, "a.txt", "v3, longer")
			f.index(t)
			before := f.contentHashes(t)

			rep, err := f.push(t, Options{})
			if err == nil || len(rep.RcloneResult.FailedFiles) != 1 {
				t.Fatalf("push: err=%v failures=%+v, want a.txt failed", err, rep.RcloneResult.FailedFiles)
			}
			if f.readDest(t, "a.txt") != "v2" {
				t.Fatal("the recorded version left its path although history could not take it")
			}
			f.checkRecordsVouch(t)
			f.checkOnlyGrew(t, before)

			if err := os.Chmod(history, 0o755); err != nil {
				t.Fatal(err)
			}
			f.mustPush(t)
			if f.readDest(t, "a.txt") != "v3, longer" {
				t.Fatal("the push with history writable did not land the change")
			}
			f.checkInvariants(t, before)
		})
	}
}

// driftingTransport changes the source file of the path staged at a
// staging key once its Put has taken the first chunk: the source drifts
// while it streams.
type driftingTransport struct {
	transport
	src string // the source file that drifts
}

func (d driftingTransport) Put(ctx context.Context, name string, r io.Reader, mtime time.Time) error {
	if !strings.Contains(name, StagingDirName+"/run-") {
		return d.transport.Put(ctx, name, r, mtime)
	}
	return d.transport.Put(ctx, name, &driftingReader{r: r, src: d.src}, mtime)
}

type driftingReader struct {
	r       io.Reader
	src     string
	drifted bool
}

func (d *driftingReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if !d.drifted && n > 0 {
		d.drifted = true
		b, rerr := os.ReadFile(d.src)
		if rerr != nil {
			return n, rerr
		}
		for i := range b {
			b[i] ^= 0xff
		}
		if werr := os.WriteFile(d.src, b, 0o644); werr != nil {
			return n, werr
		}
	}
	return n, err
}

// TestMirrorSourceDriftWhileStreaming: a source file that changes while
// its bytes stream to the destination is refused — the hash covers exactly
// the bytes sent — and its path keeps the version it had. The run fails
// before its receipt; after an index run the next push lands the new
// bytes and clears the refused copy.
func TestMirrorSourceDriftWhileStreaming(t *testing.T) {
	for _, b := range mirrorBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupMirrorFixtureOn(t, b)
			f.write(t, "big.bin", "v1")
			f.index(t)
			f.mustPush(t)
			f.write(t, "big.bin", strings.Repeat("0123456789abcdef", 256<<10))
			f.index(t)
			before := f.contentHashes(t)
			rep, err := f.pushVia(t, Options{}, func(tr transport) transport {
				return driftingTransport{transport: tr, src: filepath.Join(f.src, "big.bin")}
			})
			if err == nil || !strings.Contains(err.Error(), "drifting") || !warned(rep, "big.bin sent") {
				t.Fatalf("push: err=%v warnings=%v, want big.bin refused for drifting", err, rep.Warnings)
			}
			if f.readDest(t, "big.bin") != "v1" {
				t.Fatal("the path changed although its source drifted")
			}
			f.checkRecordsVouch(t)
			f.checkOnlyGrew(t, before)

			f.index(t)
			clean := f.mustPush(t)
			want, _ := os.ReadFile(filepath.Join(f.src, "big.bin"))
			if f.readDest(t, "big.bin") != string(want) {
				t.Fatal("the push after the index did not land the drifted bytes")
			}
			f.checkInvariants(t, before)
			f.checkStagingEmpty(t, clean.RunID)
		})
	}
}

// TestNativeContentNeverFollowsASymlinkAtAnArtifactName: a symlink where
// an object, or the objects directory, should be is never followed or
// replaced: the artifact fails, nothing is recorded for it, and nothing is
// written where the symlink points.
func TestNativeContentNeverFollowsASymlinkAtAnArtifactName(t *testing.T) {
	for _, b := range contentBackends {
		for _, at := range []string{"object", "objects directory"} {
			t.Run(b.name+"/"+at, func(t *testing.T) {
				f := setupNativeContentFixture(t, b, config.LayoutContentAddressed)
				f.write(t, "a.txt", "alpha")
				f.index(t)
				outside := outsideDir(t)
				link, target := f.objectPath("alpha"), filepath.Join(outside, "precious.txt")
				if at == "objects directory" {
					link, target = filepath.Dir(link), outside
				} else if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				if rep, err := f.push(t, Options{}); err == nil || rep.Status == store.RunStatusSuccess {
					t.Fatalf("push: status=%q err=%v, want the object refused", rep.Status, err)
				}
				checkUntouched(t, outside)
				if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("the symlink was replaced: %v, %v", fi, err)
				}
				if _, ok := f.remoteObject(t, "alpha"); ok {
					t.Fatal("the refused object was recorded")
				}
			})
		}
	}
}

// TestNativeContentSourceDriftWhileStreaming: a content layout refuses an
// object whose source changed while it streamed; nothing lands at the
// object's name.
func TestNativeContentSourceDriftWhileStreaming(t *testing.T) {
	for _, b := range contentBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupNativeContentFixture(t, b, config.LayoutContentAddressed)
			body := strings.Repeat("0123456789abcdef", 256<<10)
			f.write(t, "big.bin", body)
			f.index(t)
			rep, err := f.pushVia(t, func(tr transport) transport {
				return driftingTransport{transport: tr, src: filepath.Join(f.src, "big.bin")}
			})
			if err == nil || rep.Status == store.RunStatusSuccess {
				t.Fatalf("push: status=%q err=%v, want the drifted object refused", rep.Status, err)
			}
			if _, err := os.Stat(f.objectPath(body)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("drifted bytes reached the object's name: %v", err)
			}
			if _, ok := f.remoteObject(t, body); ok {
				t.Fatal("the drifted object was recorded")
			}
		})
	}
}

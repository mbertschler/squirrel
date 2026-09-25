package sync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// transportHarness opens a fresh, empty destination root for one contract
// case. plant puts a symlink named name, pointing at target, on the
// destination — something no transport call creates.
type transportHarness func(t *testing.T) (tr transport, plant func(target, name string))

// runTransportContract is the behaviour every transport implementation
// must show. It pins the two properties the layouts' safety rests on —
// nothing replaces an existing name, and no symlink is ever followed —
// plus the plain semantics of each call.
func runTransportContract(t *testing.T, open transportHarness) {
	cases := []struct {
		name string
		run  func(t *testing.T, tr transport, plant func(target, name string))
	}{
		{"PutThenStatListGet", contractPutThenStatListGet},
		{"PutLandsALargeBodyIntact", contractPutLarge},
		{"PutNeverReplaces", contractPutNeverReplaces},
		{"StatAbsent", contractStatAbsent},
		{"RenameMovesAndCreatesParents", contractRenameMoves},
		{"RenameNeverReplaces", contractRenameNeverReplaces},
		{"RenameMovesDirectories", contractRenameDirectory},
		{"RemoveFilesAndEmptyDirectoriesOnly", contractRemove},
		{"RefusesInvalidNames", contractInvalidNames},
		{"NeverFollowsSymlinks", contractNoSymlinks},
		{"ParentThatIsAFile", contractParentNotDir},
		{"PutStopsOnCancel", contractPutCancel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, plant := open(t)
			t.Cleanup(func() { _ = tr.Close() })
			c.run(t, tr, plant)
		})
	}
}

func TestLocalTransportContract(t *testing.T) {
	runTransportContract(t, func(t *testing.T) (transport, func(target, name string)) {
		dir := t.TempDir()
		tr, err := openLocalTransport(dir)
		if err != nil {
			t.Fatalf("openLocalTransport: %v", err)
		}
		return tr, func(target, name string) {
			p := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, p); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}
	})
}

var contractMtime = time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)

func mustPut(t *testing.T, tr transport, name, body string) {
	t.Helper()
	if err := tr.Put(context.Background(), name, strings.NewReader(body), contractMtime); err != nil {
		t.Fatalf("Put %s: %v", name, err)
	}
}

func mustRead(t *testing.T, tr transport, name string) string {
	t.Helper()
	rc, err := tr.Get(context.Background(), name)
	if err != nil {
		t.Fatalf("Get %s: %v", name, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func contractPutThenStatListGet(t *testing.T, tr transport, _ func(string, string)) {
	ctx := context.Background()
	mustPut(t, tr, "a/b/c.txt", "hello")
	e, err := tr.Stat(ctx, "a/b/c.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if e.kind != kindFile || e.size != 5 || e.name != "c.txt" {
		t.Fatalf("Stat = %+v, want file c.txt of 5 bytes", e)
	}
	// sftp carries whole seconds, so the contract allows truncation.
	if d := contractMtime.Sub(e.mtime); d < 0 || d >= time.Second {
		t.Fatalf("mtime = %v, want %v to the second", e.mtime, contractMtime)
	}
	list, err := tr.List(ctx, "a/b")
	if err != nil || len(list) != 1 || list[0].name != "c.txt" {
		t.Fatalf("List = %+v, %v; want [c.txt]", list, err)
	}
	root, err := tr.List(ctx, ".")
	if err != nil || len(root) != 1 || root[0].name != "a" || root[0].kind != kindDir {
		t.Fatalf("List(.) = %+v, %v; want the directory a", root, err)
	}
	if got := mustRead(t, tr, "a/b/c.txt"); got != "hello" {
		t.Fatalf("Get = %q, want hello", got)
	}
}

// contractPutLarge lands a body spanning many write requests, which an
// sftp transport sends concurrently and a server may apply out of order.
func contractPutLarge(t *testing.T, tr transport, _ func(string, string)) {
	body := make([]byte, 5<<20+123)
	for i := range body {
		body[i] = byte(i * 7 % 251)
	}
	if err := tr.Put(context.Background(), "big", bytes.NewReader(body), contractMtime); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := mustRead(t, tr, "big"); got != string(body) {
		t.Fatalf("read back %d bytes, want the %d written", len(got), len(body))
	}
}

func contractPutNeverReplaces(t *testing.T, tr transport, _ func(string, string)) {
	mustPut(t, tr, "x", "first")
	err := tr.Put(context.Background(), "x", strings.NewReader("second"), contractMtime)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("Put over an existing name = %v, want fs.ErrExist", err)
	}
	if got := mustRead(t, tr, "x"); got != "first" {
		t.Fatalf("bytes after a refused Put = %q, want first", got)
	}
}

func contractStatAbsent(t *testing.T, tr transport, _ func(string, string)) {
	for _, name := range []string{"nope", "a/nope/x"} {
		if _, err := tr.Stat(context.Background(), name); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Stat %s = %v, want fs.ErrNotExist", name, err)
		}
	}
}

func contractRenameMoves(t *testing.T, tr transport, _ func(string, string)) {
	ctx := context.Background()
	mustPut(t, tr, "x", "body")
	if err := tr.Rename(ctx, "x", "d/e/y"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := tr.Stat(ctx, "x"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("source after Rename: %v, want gone", err)
	}
	if got := mustRead(t, tr, "d/e/y"); got != "body" {
		t.Fatalf("target = %q, want body", got)
	}
}

func contractRenameNeverReplaces(t *testing.T, tr transport, _ func(string, string)) {
	mustPut(t, tr, "x", "from")
	mustPut(t, tr, "y", "to")
	if err := tr.Rename(context.Background(), "x", "y"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("Rename onto an existing name = %v, want fs.ErrExist", err)
	}
	if mustRead(t, tr, "x") != "from" || mustRead(t, tr, "y") != "to" {
		t.Fatal("a refused Rename changed either side")
	}
}

func contractRenameDirectory(t *testing.T, tr transport, _ func(string, string)) {
	mustPut(t, tr, "d/f", "inside")
	if err := tr.Rename(context.Background(), "d", "h/d"); err != nil {
		t.Fatalf("Rename directory: %v", err)
	}
	if got := mustRead(t, tr, "h/d/f"); got != "inside" {
		t.Fatalf("moved file = %q, want inside", got)
	}
}

func contractRemove(t *testing.T, tr transport, _ func(string, string)) {
	ctx := context.Background()
	mustPut(t, tr, "d/f", "x")
	if err := tr.Remove(ctx, "d"); err == nil {
		t.Fatal("Remove of a non-empty directory succeeded")
	}
	if err := tr.Remove(ctx, "d/f"); err != nil {
		t.Fatalf("Remove file: %v", err)
	}
	if err := tr.Remove(ctx, "d"); err != nil {
		t.Fatalf("Remove empty directory: %v", err)
	}
	if _, err := tr.Stat(ctx, "d"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed directory: %v, want gone", err)
	}
}

func contractInvalidNames(t *testing.T, tr transport, _ func(string, string)) {
	ctx := context.Background()
	mustPut(t, tr, "ok", "x")
	for _, name := range []string{"", ".", "../x", "/abs", "a/../b", "a//b", "a/"} {
		if _, err := tr.Stat(ctx, name); !errors.Is(err, errInvalidName) {
			t.Errorf("Stat %q = %v, want errInvalidName", name, err)
		}
		if err := tr.Put(ctx, name, strings.NewReader("x"), contractMtime); !errors.Is(err, errInvalidName) {
			t.Errorf("Put %q = %v, want errInvalidName", name, err)
		}
		if err := tr.Rename(ctx, "ok", name); !errors.Is(err, errInvalidName) {
			t.Errorf("Rename onto %q = %v, want errInvalidName", name, err)
		}
	}
}

func contractNoSymlinks(t *testing.T, tr transport, plant func(string, string)) {
	ctx := context.Background()
	mustPut(t, tr, "real/file", "target bytes")
	plant("real", "link")
	plant("file", "real/filelink")

	if e, err := tr.Stat(ctx, "link"); err != nil || e.kind != kindSymlink {
		t.Fatalf("Stat of the link = %+v, %v; want the symlink itself", e, err)
	}
	if _, err := tr.Stat(ctx, "link/file"); !errors.Is(err, errSymlinkInPath) {
		t.Fatalf("Stat through the link = %v, want errSymlinkInPath", err)
	}
	if err := tr.Put(ctx, "link/new", strings.NewReader("x"), contractMtime); !errors.Is(err, errSymlinkInPath) {
		t.Fatalf("Put through the link = %v, want errSymlinkInPath", err)
	}
	if _, err := tr.Stat(ctx, "real/new"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a Put through the link landed in its target: %v", err)
	}
	mustPut(t, tr, "x", "moving")
	if err := tr.Rename(ctx, "x", "link/x"); !errors.Is(err, errSymlinkInPath) {
		t.Fatalf("Rename through the link = %v, want errSymlinkInPath", err)
	}
	if _, err := tr.Get(ctx, "real/filelink"); err == nil {
		t.Fatal("Get followed a symlink to a file")
	}
	if _, err := tr.List(ctx, "link"); err == nil {
		t.Fatal("List followed a symlink to a directory")
	}
	// Renaming the link itself moves the link, never its target.
	if err := tr.Rename(ctx, "link", "moved"); err != nil {
		t.Fatalf("Rename the link: %v", err)
	}
	if got := mustRead(t, tr, "real/file"); got != "target bytes" {
		t.Fatalf("the link's target changed: %q", got)
	}
}

func contractParentNotDir(t *testing.T, tr transport, _ func(string, string)) {
	ctx := context.Background()
	mustPut(t, tr, "f", "a file")
	if _, err := tr.Stat(ctx, "f/x"); !errors.Is(err, errParentNotDir) {
		t.Fatalf("Stat under a file = %v, want errParentNotDir", err)
	}
	if err := tr.Put(ctx, "f/x", strings.NewReader("x"), contractMtime); !errors.Is(err, errParentNotDir) {
		t.Fatalf("Put under a file = %v, want errParentNotDir", err)
	}
}

func contractPutCancel(t *testing.T, tr transport, _ func(string, string)) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := bytes.NewReader(bytes.Repeat([]byte("x"), 1<<20))
	if err := tr.Put(ctx, "big", body, contractMtime); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put with a cancelled context = %v, want context.Canceled", err)
	}
	list, _ := tr.List(context.Background(), ".")
	if i := slices.IndexFunc(list, func(e entry) bool { return e.name == "big" && e.size == 1<<20 }); i >= 0 {
		t.Fatal("a cancelled Put landed the whole body")
	}
}

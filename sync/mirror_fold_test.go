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
	"testing"
	"time"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/store"
)

// foldingTransport stands in for a destination that folds names, as APFS
// and exFAT do: each name resolves, element by element, to the entry
// already on disk whose name folds to the same key. disk is where the
// destination root is on this machine.
type foldingTransport struct {
	transport
	disk string
	fold nameFolding
}

func (f foldingTransport) resolve(name string) string {
	parts := strings.Split(name, "/")
	dir := f.disk
	for i, p := range parts {
		entries, err := os.ReadDir(dir)
		if err != nil {
			break
		}
		for _, e := range entries {
			if f.fold.key(e.Name()) == f.fold.key(p) {
				parts[i] = e.Name()
				break
			}
		}
		dir = filepath.Join(dir, parts[i])
	}
	return strings.Join(parts, "/")
}

func (f foldingTransport) Stat(ctx context.Context, name string) (entry, error) {
	return f.transport.Stat(ctx, f.resolve(name))
}

func (f foldingTransport) List(ctx context.Context, dir string) ([]entry, error) {
	return f.transport.List(ctx, f.resolve(dir))
}

func (f foldingTransport) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	return f.transport.Get(ctx, f.resolve(name))
}

func (f foldingTransport) Put(ctx context.Context, name string, r io.Reader, mtime time.Time) error {
	return f.transport.Put(ctx, f.resolve(name), r, mtime)
}

func (f foldingTransport) Rename(ctx context.Context, from, to string) error {
	return f.transport.Rename(ctx, f.resolve(from), f.resolve(to))
}

func (f foldingTransport) Remove(ctx context.Context, name string) error {
	return f.transport.Remove(ctx, f.resolve(name))
}

func (f foldingTransport) ServerHash(ctx context.Context, name string) (remoteChecksum, error) {
	return f.transport.ServerHash(ctx, f.resolve(name))
}

var (
	caseAndNormFolding = nameFolding{caseless: true, normless: true}
	composed           = "caf\u00e9.txt"
	decomposed         = "cafe\u0301.txt"
)

func TestNameFoldingKey(t *testing.T) {
	cases := []struct {
		fold nameFolding
		a, b string
		same bool
	}{
		{nameFolding{}, "a.jpg", "A.jpg", false},
		{nameFolding{caseless: true}, "a.jpg", "A.jpg", true},
		{nameFolding{caseless: true}, "Straße/Ä", "STRASSE/ä", false},
		{nameFolding{caseless: true}, "ärger/Σ", "Ärger/ς", true},
		{nameFolding{caseless: true}, composed, decomposed, false},
		{nameFolding{normless: true}, composed, decomposed, true},
		{nameFolding{normless: true}, "a.jpg", "A.jpg", false},
		{caseAndNormFolding, "Caf\u00e9/X", "cafe\u0301/x", true},
	}
	for _, c := range cases {
		if got := c.fold.key(c.a) == c.fold.key(c.b); got != c.same {
			t.Errorf("%+v: %q and %q fold together = %v, want %v", c.fold, c.a, c.b, got, c.same)
		}
	}
}

// TestMirrorProbesHowTheDestinationComparesNames: a push learns from one
// file in staging whether the destination folds case, normalization,
// both or neither, and leaves no probe behind.
func TestMirrorProbesHowTheDestinationComparesNames(t *testing.T) {
	for _, b := range mirrorBackends {
		for _, fold := range []nameFolding{{}, {caseless: true}, {normless: true}, caseAndNormFolding} {
			t.Run(fmt.Sprintf("%s/%+v", b.name, fold), func(t *testing.T) {
				f := setupMirrorFixtureOn(t, b)
				f.fold = fold
				f.write(t, "a.txt", "alpha")
				f.index(t)
				var got nameFolding
				_, err := f.pushVia(t, Options{}, func(tr transport) transport {
					return probeRecorder{transport: tr, got: &got}
				})
				if err != nil {
					t.Fatal(err)
				}
				disk := diskFolding(t, f.dst)
				want := nameFolding{caseless: fold.caseless || disk.caseless, normless: fold.normless || disk.normless}
				if got != want {
					t.Fatalf("probe = %+v, want %+v", got, want)
				}
				if _, err := os.Stat(f.dest(StagingDirName + "/" + foldProbeBase)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("the probe stayed in staging: %v", err)
				}
			})
		}
	}
}

// probeRecorder records what the name probe's two lookups found.
type probeRecorder struct {
	transport
	got *nameFolding
}

func (p probeRecorder) Stat(ctx context.Context, name string) (entry, error) {
	e, err := p.transport.Stat(ctx, name)
	switch {
	case strings.HasSuffix(name, "/"+foldProbeCase):
		p.got.caseless = err == nil
	case strings.HasSuffix(name, "/"+foldProbeNorm):
		p.got.normless = err == nil
	}
	return e, err
}

// setupFoldingMirror is a mirror fixture on b whose destination folds case
// and normalization.
func setupFoldingMirror(t *testing.T, b mirrorBackend) *mirrorFixture {
	t.Helper()
	f := setupMirrorFixtureOn(t, b)
	f.fold = caseAndNormFolding
	return f
}

// observe records body as present at rel in a run of its own, as an index
// run over a source that holds rel would; it writes no source file, so a
// push that tried to send rel would fail to read it.
func (f *mirrorFixture) observe(t *testing.T, rel, body string) {
	t.Helper()
	ctx := context.Background()
	volID := f.volumeID(t)
	runID, err := f.store.BeginIndexRun(ctx, store.RunKindIndex, volID, false)
	if err != nil {
		t.Fatal(err)
	}
	sum := blake3.Sum256([]byte(body))
	now := time.Now().UnixNano()
	row := store.FileRow{VolumeID: volID, Path: rel, Blake3: sum[:], SizeBytes: int64(len(body)), MtimeNs: now,
		Status: store.StatusPresent, FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: now}
	if err := f.store.Upsert(ctx, row, nil); err != nil {
		t.Fatalf("Upsert %s: %v", rel, err)
	}
	if err := f.store.FinishRun(ctx, runID, store.RunStatusSuccess, "", 1); err != nil {
		t.Fatal(err)
	}
}

// TestMirrorRefusesNamesTheDestinationCannotTellApart: on a destination
// that folds case and normalization, a present path whose name — or a
// parent's — folds onto another present path's is refused, and only that
// path: nothing is displaced, the path already there keeps its bytes and
// its record, every other path lands, and the run fails before its
// receipt, so the watermark holds until one of them is renamed.
func TestMirrorRefusesNamesTheDestinationCannotTellApart(t *testing.T) {
	cases := []struct {
		name             string
		first, colliding string
	}{
		{"case", "a.jpg", "A.jpg"},
		{"normalization", composed, decomposed},
		{"a file where a directory is", "d/x.txt", "D"},
		{"a directory where a file is", "f", "F/y.txt"},
	}
	for _, b := range mirrorBackends {
		for _, c := range cases {
			t.Run(b.name+"/"+c.name, func(t *testing.T) {
				f := setupFoldingMirror(t, b)
				f.write(t, c.first, "first")
				f.index(t)
				f.mustPush(t)
				f.write(t, "other.txt", "other")
				f.index(t)
				f.observe(t, c.colliding, "colliding")
				before := f.contentHashes(t)

				rep, err := f.push(t, Options{})
				if err == nil || rep.Status != store.RunStatusFailed || !strings.Contains(err.Error(), "cannot tell their names apart") {
					t.Fatalf("push: status=%q err=%v, want a failed run naming the collision", rep.Status, err)
				}
				if !warned(rep, c.colliding+" is not written") {
					t.Fatalf("warnings = %v, want %s refused", rep.Warnings, c.colliding)
				}
				if f.readDest(t, c.first) != "first" || f.readDest(t, "other.txt") != "other" {
					t.Fatal("the path already there changed, or the other path did not land")
				}
				if got := f.rowsAt(t, c.first); !slicesEqual(got, []string{store.RemotePathLive}) {
					t.Fatalf("%s rows = %v, want its one live row untouched", c.first, got)
				}
				if got := f.rowsAt(t, c.colliding); len(got) != 0 {
					t.Fatalf("%s rows = %v, want none", c.colliding, got)
				}
				if _, err := os.Stat(f.dest(HistoryDirName)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("a refused path displaced something: %v", err)
				}
				if after := f.contentHashes(t); len(after) != len(before)+1 {
					t.Fatalf("destination holds %d files, want the %d before plus other.txt", len(after), len(before))
				}
				if _, err := os.Stat(f.dest(".squirrel-index/run-" + strconv.FormatInt(rep.RunID, 10))); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("the failed run left a receipt: %v", err)
				}
			})
		}
	}
}

// TestMirrorRefusesTheSecondOfTwoNewNames: two new present paths that fold
// together land as one: the first by spelling, the second refused.
func TestMirrorRefusesTheSecondOfTwoNewNames(t *testing.T) {
	f := setupFoldingMirror(t, localBackend)
	f.write(t, "keep.txt", "keep")
	f.index(t)
	f.observe(t, "X.jpg", "upper")
	f.observe(t, "x.jpg", "lower")
	f.write(t, "X.jpg", "upper")
	rep, err := f.push(t, Options{})
	if err == nil || !warned(rep, "x.jpg is not written") {
		t.Fatalf("push: err=%v warnings=%v, want x.jpg refused", err, rep.Warnings)
	}
	if f.readDest(t, "X.jpg") != "upper" {
		t.Fatal("the first spelling did not land")
	}
}

// TestMirrorFollowsARenameThatChangesOnlyCase: a file or a directory
// renamed at the source in case alone moves the recorded versions under the
// old spelling into history as their records, then lands under the new
// one, on a destination that cannot hold both. Every record keeps
// vouching for the bytes it names, on the folding the fixture emulates and
// on this machine's own disk when that folds.
func TestMirrorFollowsARenameThatChangesOnlyCase(t *testing.T) {
	for _, b := range mirrorBackends {
		for _, emulate := range []bool{true, false} {
			t.Run(b.name+"/emulated="+strconv.FormatBool(emulate), func(t *testing.T) {
				f := setupMirrorFixtureOn(t, b)
				if emulate {
					f.fold = caseAndNormFolding
				} else if !diskFolding(t, f.dst).caseless {
					t.Skip("this machine's disk tells names apart by case")
				}
				f.write(t, "a.jpg", "file")
				f.write(t, "Photos/x.jpg", "in a directory")
				f.index(t)
				f.mustPush(t)
				f.rename(t, "a.jpg", "A.jpg")
				f.rename(t, "Photos", "photos")
				f.index(t)
				before := f.contentHashes(t)

				rep := f.mustPush(t)
				run := strconv.FormatInt(rep.RunID, 10)
				if f.readDest(t, "A.jpg") != "file" || f.readDest(t, "photos/x.jpg") != "in a directory" {
					t.Fatal("the renamed paths did not land")
				}
				for _, rel := range []string{"a.jpg", "Photos/x.jpg"} {
					if got := f.rowsAt(t, rel); !slicesEqual(got, []string{store.RemotePathDisplaced}) {
						t.Fatalf("%s rows = %v, want displaced", rel, got)
					}
				}
				if f.readDest(t, HistoryDirName+"/run-"+run+"/a.jpg") != "file" {
					t.Fatal("the old spelling's version is not in history")
				}
				f.checkInvariants(t, before)
			})
		}
	}
}

func (f *mirrorFixture) rename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(filepath.Join(f.src, from), filepath.Join(f.src, to)); err != nil {
		t.Fatal(err)
	}
}

// diskFolding is how the disk under dir compares names.
func diskFolding(t *testing.T, dir string) nameFolding {
	t.Helper()
	found := func(name, lookup string) bool {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(p) }()
		_, err := os.Stat(filepath.Join(dir, lookup))
		return err == nil
	}
	return nameFolding{caseless: found("case-probe", "CASE-PROBE"), normless: found(composed, decomposed)}
}

// TestMirrorFoldingMovesAFileOutOfADirectorysWay: a recorded file whose
// path is gone at the source moves aside as its record when a new path's
// directory folds onto its name, and a recorded directory moves with every
// version in it when a new file's name folds onto it.
func TestMirrorFoldingMovesAFileOutOfADirectorysWay(t *testing.T) {
	for _, b := range mirrorBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupFoldingMirror(t, b)
			f.write(t, "a", "file a")
			f.write(t, "D/x", "inside D")
			f.index(t)
			f.mustPush(t)
			for _, rel := range []string{"a", "D"} {
				if err := os.RemoveAll(filepath.Join(f.src, rel)); err != nil {
					t.Fatal(err)
				}
			}
			f.write(t, "A/b", "under A")
			f.write(t, "d", "file d")
			f.index(t)
			before := f.contentHashes(t)

			f.mustPush(t)
			if f.readDest(t, "A/b") != "under A" || f.readDest(t, "d") != "file d" {
				t.Fatal("the new paths did not land")
			}
			for _, rel := range []string{"a", "D/x"} {
				if got := f.rowsAt(t, rel); !slicesEqual(got, []string{store.RemotePathDisplaced}) {
					t.Fatalf("%s rows = %v, want displaced as its record", rel, got)
				}
			}
			f.checkInvariants(t, before)
		})
	}
}

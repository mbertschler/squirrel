package sync

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

// crashModesFor is every way a call can die that leaves a different
// destination behind: a read fails the same way before or after it runs,
// a write can also land, and a Put can land part of its body.
func crashModesFor(c transportCall) []crashMode {
	switch c.op {
	case "stat", "list":
		return []crashMode{crashBefore}
	case "put":
		return []crashMode{crashBefore, crashMidway, crashAfter}
	default:
		return []crashMode{crashBefore, crashAfter}
	}
}

var crashModeNames = map[crashMode]string{crashBefore: "before", crashAfter: "after", crashMidway: "midway"}

// crashAtCall selects the i-th call a push makes.
func crashAtCall(i int) func(transportCall) bool {
	n := -1
	return func(transportCall) bool {
		n++
		return n == i
	}
}

// mirrorCrashScenario lands a first tree with one clean push, then changes
// the source and the destination so the next push takes every kind of
// step: a confirm, a write displacing its recorded version, a write
// displacing bytes squirrel never wrote, a record changed behind
// squirrel's back, a file becoming a directory and a directory becoming a
// file, a rename that changes only case, and a path deleted at the source.
func mirrorCrashScenario(t *testing.T, f *mirrorFixture) {
	t.Helper()
	for rel, body := range map[string]string{
		"keep.txt": "keep", "change.txt": "v1", "tampered.txt": "t1",
		"swap": "file swap", "dir/x": "in dir", "gone.txt": "gone", "Case.txt": "case",
	} {
		f.write(t, rel, body)
	}
	f.index(t)
	f.mustPush(t)
	for _, rel := range []string{"swap", "dir", "gone.txt"} {
		if err := os.RemoveAll(filepath.Join(f.src, rel)); err != nil {
			t.Fatal(err)
		}
	}
	f.write(t, "change.txt", "v2 is longer")
	f.write(t, "tampered.txt", "t2 at the source")
	f.write(t, "swap/inner", "swap is a directory now")
	f.write(t, "dir", "dir is a file now")
	f.write(t, "new.txt", "new")
	f.rename(t, "Case.txt", "case.txt")
	f.index(t)
	for rel, body := range map[string]string{"tampered.txt": "tampered on the destination", "new.txt": "foreign"} {
		if err := os.WriteFile(f.dest(rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// recordMirrorCalls runs the scenario's push once without a crash and returns
// every transport call it made.
func recordMirrorCalls(t *testing.T, b mirrorBackend, fold nameFolding) []transportCall {
	t.Helper()
	f := setupMirrorFixtureOn(t, b)
	f.fold = fold
	mirrorCrashScenario(t, f)
	var rec *faultTransport
	rep, err := f.pushVia(t, Options{}, func(tr transport) transport {
		rec = &faultTransport{transport: tr}
		return rec
	})
	if err != nil || rep.Status != store.RunStatusSuccess {
		t.Fatalf("recording push: status=%q err=%v warnings=%v", rep.Status, err, rep.Warnings)
	}
	return rec.calls
}

// TestMirrorCrashAtEveryTransportCall: a push that dies at any one of its
// transport calls, in any way that call can die, leaves every record
// vouching for its bytes and every byte where it was; the next clean push
// then settles what the crash left and satisfies all three invariants.
// Both transports run it, on a destination that tells names apart and on
// one that folds them.
func TestMirrorCrashAtEveryTransportCall(t *testing.T) {
	for _, b := range mirrorBackends {
		for _, fold := range []nameFolding{{}, caseAndNormFolding} {
			t.Run(fmt.Sprintf("%s/folds=%v", b.name, fold.folds()), func(t *testing.T) {
				calls := recordMirrorCalls(t, b, fold)
				for i, c := range calls {
					for _, mode := range crashModesFor(c) {
						t.Run(fmt.Sprintf("%03d-%s-%s-%s", i, c.op, path.Base(c.name), crashModeNames[mode]), func(t *testing.T) {
							t.Parallel()
							runMirrorCrashAt(t, b, fold, i, mode)
						})
					}
				}
			})
		}
	}
}

func runMirrorCrashAt(t *testing.T, b mirrorBackend, fold nameFolding, i int, mode crashMode) {
	f := setupMirrorFixtureOn(t, b)
	f.fold = fold
	mirrorCrashScenario(t, f)
	before := f.contentHashes(t)
	if rep, err := f.pushCrashing(t, crashAtCall(i), mode); !crashedOn(rep, err) {
		t.Fatalf("crashing push: status=%q err=%v failures=%+v, want the injected crash", rep.Status, err, rep.RcloneResult.FailedFiles)
	}
	f.checkRecordsVouch(t, "tampered.txt")
	f.checkOnlyGrew(t, before)

	clean := f.mustPush(t)
	f.checkInvariants(t, before)
	f.checkStagingEmpty(t, clean.RunID)
	for rel, want := range map[string]string{
		"keep.txt": "keep", "change.txt": "v2 is longer", "tampered.txt": "t2 at the source",
		"swap/inner": "swap is a directory now", "dir": "dir is a file now", "new.txt": "new",
		"case.txt": "case", "gone.txt": "gone",
	} {
		if got := f.readDest(t, rel); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

// contentCrashScenario lands a first tree on a content destination with
// one clean push, then adds, changes and deletes files so the next push
// uploads objects and, on a packed destination, a pack and its map.
func contentCrashScenario(t *testing.T, f *nativeContentFixture) {
	t.Helper()
	f.write(t, "a.txt", "alpha")
	f.write(t, "big.txt", "a larger file")
	f.write(t, "gone.txt", "gone, soon")
	f.index(t)
	f.mustPush(t)
	if err := os.Remove(filepath.Join(f.src, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	f.write(t, "a.txt", "ALPHA")
	f.write(t, "b.txt", "beta")
	f.write(t, "2024/big.txt", "another larger file")
	f.index(t)
}

func recordContentCalls(t *testing.T, b contentBackend, layout string) []transportCall {
	t.Helper()
	f := setupNativeContentFixture(t, b, layout)
	contentCrashScenario(t, f)
	var calls []transportCall
	rep, err := f.pushCrashing(t, func(c transportCall) bool {
		calls = append(calls, c)
		return false
	}, crashBefore)
	if err != nil || rep.Status != store.RunStatusSuccess {
		t.Fatalf("recording push: status=%q err=%v", rep.Status, err)
	}
	return calls
}

// TestNativeContentCrashAtEveryTransportCall: a content-addressed or
// packed push that dies at any one of its transport calls leaves every
// recorded artifact at its name with its bytes and every byte where it
// was. The next clean push lands the rest, a verify pass re-confirms every
// artifact, and a restore returns the volume.
func TestNativeContentCrashAtEveryTransportCall(t *testing.T) {
	for _, b := range contentBackends {
		for _, layout := range contentLayouts {
			t.Run(b.name+"/"+layout, func(t *testing.T) {
				for i, c := range recordContentCalls(t, b, layout) {
					for _, mode := range crashModesFor(c) {
						t.Run(fmt.Sprintf("%03d-%s-%s-%s", i, c.op, path.Base(c.name), crashModeNames[mode]), func(t *testing.T) {
							t.Parallel()
							runContentCrashAt(t, b, layout, i, mode)
						})
					}
				}
			})
		}
	}
}

func runContentCrashAt(t *testing.T, b contentBackend, layout string, i int, mode crashMode) {
	f := setupNativeContentFixture(t, b, layout)
	contentCrashScenario(t, f)
	before := hashesOutsideStaging(t, f.dst)
	if rep, err := f.pushCrashing(t, crashAtCall(i), mode); !crashedOn(rep, err) {
		t.Fatalf("crashing push: status=%q err=%v failures=%+v, want the injected crash", rep.Status, err, rep.RcloneResult.FailedFiles)
	}
	f.checkArtifactsVouch(t)
	checkStillHeld(t, before, hashesOutsideStaging(t, f.dst))

	f.mustPush(t)
	f.checkArtifactsVouch(t)
	checkStillHeld(t, before, hashesOutsideStaging(t, f.dst))
	to := t.TempDir()
	rep, err := Restore(context.Background(), f.store, nil, f.pair.Volume, f.pair.Destination, RestoreOptions{ToPath: to})
	if err != nil || rep.Status != store.RunStatusSuccess {
		t.Fatalf("restore: status=%q err=%v", rep.Status, err)
	}
	for rel, want := range map[string]string{"a.txt": "ALPHA", "b.txt": "beta", "big.txt": "a larger file", "2024/big.txt": "another larger file"} {
		if got, err := os.ReadFile(filepath.Join(to, filepath.FromSlash(rel))); err != nil || string(got) != want {
			t.Errorf("restored %s = %q, %v; want %q", rel, got, err, want)
		}
	}
}

// checkArtifactsVouch asserts that every artifact squirrel recorded on the
// destination is at its name with the bytes recorded: a verify pass
// re-confirms each one and finds nothing gone or changed.
func (f *nativeContentFixture) checkArtifactsVouch(t *testing.T) {
	t.Helper()
	rep, err := VerifyRemote(context.Background(), f.store, nil, f.pair.Destination)
	if err != nil || !rep.Clean() || rep.Unchecked != 0 {
		t.Fatalf("verify = %+v, %v; want every recorded artifact re-confirmed", rep, err)
	}
}

// checkStillHeld asserts that every content in before is still in after.
func checkStillHeld(t *testing.T, before, after map[string]bool) {
	t.Helper()
	for h := range before {
		if !after[h] {
			t.Errorf("content %s left the destination", h)
		}
	}
}

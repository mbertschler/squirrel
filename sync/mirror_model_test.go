package sync

import (
	"encoding/hex"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/store"
)

// mirrorModel is the reference a native mirror is checked against: the
// source's present files, and the live tree a clean push leaves on the
// destination. A destination keeps what the source deleted, until a
// present path needs its name.
type mirrorModel struct {
	fold    nameFolding
	present map[string]string // source path → body
	dest    map[string]string // destination path → body
}

// push applies one push to the model: every present path, except the
// refused ones, lands, and every other entry whose name it needs — the same
// name under another spelling, a file where its directory goes, or a
// directory where it goes — moves into history.
func (m *mirrorModel) push(refused map[string]bool) {
	for _, p := range sortedKeys(m.present) {
		if refused[p] {
			continue
		}
		for q := range m.dest {
			if q != p && m.conflicts(p, q) {
				delete(m.dest, q)
			}
		}
		m.dest[p] = m.present[p]
	}
}

func (m *mirrorModel) conflicts(p, q string) bool {
	kp, kq := m.fold.key(p), m.fold.key(q)
	return kp == kq || strings.HasPrefix(kp, kq+"/") || strings.HasPrefix(kq, kp+"/")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// modelRun drives one random history against a mirror fixture and its
// model.
type modelRun struct {
	t     *testing.T
	f     *mirrorFixture
	m     *mirrorModel
	rng   *rand.Rand
	step  int
	clock time.Time
}

// sourceFolding is how the generator keeps the source free of spellings
// any destination would fold together, whatever this machine's disk does.
var sourceFolding = caseAndNormFolding

// TestMirrorModel runs random histories — adds, changes, deletes, re-adds,
// file↔directory swaps, nested, renames that change only case, and on a
// folding destination names that collide — through a native mirror
// writing several paths at once, each push preceded now and then by one
// that crashes or loses power at a random transport call. After every
// clean push the destination's live records and its tree match the
// reference model, and the three invariants hold.
func TestMirrorModel(t *testing.T) {
	for _, b := range mirrorBackends {
		for _, fold := range []nameFolding{{}, caseAndNormFolding} {
			for seed := uint64(1); seed <= 3; seed++ {
				t.Run(fmt.Sprintf("%s/folds=%v/seed=%d", b.name, fold.folds(), seed), func(t *testing.T) {
					t.Parallel()
					f := setupMirrorFixtureOn(t, b)
					f.fold = fold
					disk := diskFolding(t, f.dst)
					r := &modelRun{t: t, f: f, rng: rand.New(rand.NewPCG(seed, seed)), clock: time.Unix(1_700_000_000, 0),
						m: &mirrorModel{
							fold:    nameFolding{caseless: fold.caseless || disk.caseless, normless: fold.normless || disk.normless},
							present: map[string]string{}, dest: map[string]string{},
						}}
					for r.step = 0; r.step < 25; r.step++ {
						r.changeSource()
						r.push()
					}
				})
			}
		}
	}
}

// changeSource applies one to three random changes to the source.
func (r *modelRun) changeSource() {
	for range 1 + r.rng.IntN(3) {
		switch r.rng.IntN(10) {
		case 0, 1, 2, 3:
			r.write(r.randomPath())
		case 4, 5:
			if p, ok := r.pickPresent(); ok {
				r.write(p)
			}
		case 6, 7:
			if p, ok := r.pickPresent(); ok {
				r.remove(p)
			}
		case 8:
			if p, ok := r.pickGone(); ok {
				r.write(p)
			}
		case 9:
			r.renameCase()
		}
	}
}

func (r *modelRun) randomPath() string {
	elems := make([]string, 1+r.rng.IntN(3))
	for i := range elems {
		elems[i] = string(rune('a' + r.rng.IntN(3)))
	}
	return strings.Join(elems, "/")
}

func (r *modelRun) pickPresent() (string, bool) {
	keys := sortedKeys(r.m.present)
	if len(keys) == 0 {
		return "", false
	}
	return keys[r.rng.IntN(len(keys))], true
}

// pickGone picks a path the destination still holds that the source no
// longer has.
func (r *modelRun) pickGone() (string, bool) {
	var gone []string
	for _, p := range sortedKeys(r.m.dest) {
		if _, ok := r.m.present[p]; !ok {
			gone = append(gone, p)
		}
	}
	if len(gone) == 0 {
		return "", false
	}
	return gone[r.rng.IntN(len(gone))], true
}

// write puts a new body at p, spelled as the source already spells its
// existing parts, first removing a file where p's directory goes and a
// directory where p goes: the file↔directory swaps.
func (r *modelRun) write(p string) {
	p = foldingTransport{disk: r.f.src, fold: sourceFolding}.resolve(p)
	for q := range r.m.present {
		if strings.HasPrefix(p, q+"/") || strings.HasPrefix(q, p+"/") {
			delete(r.m.present, q)
		}
	}
	parts := strings.Split(p, "/")
	for i := 1; i <= len(parts); i++ {
		at := filepath.Join(r.f.src, filepath.FromSlash(strings.Join(parts[:i], "/")))
		if fi, err := os.Lstat(at); err == nil && (i < len(parts)) != fi.IsDir() {
			if err := os.RemoveAll(at); err != nil {
				r.t.Fatal(err)
			}
		}
	}
	body := fmt.Sprintf("%s at step %d %s", p, r.step, strings.Repeat("x", r.rng.IntN(40)))
	r.f.write(r.t, p, body)
	r.touch(p)
	r.m.present[p] = body
}

// touch gives p an mtime of its own, so the index sees every change.
func (r *modelRun) touch(p string) {
	r.clock = r.clock.Add(time.Second)
	if err := os.Chtimes(filepath.Join(r.f.src, filepath.FromSlash(p)), r.clock, r.clock); err != nil {
		r.t.Fatal(err)
	}
}

func (r *modelRun) remove(p string) {
	if err := os.Remove(filepath.Join(r.f.src, filepath.FromSlash(p))); err != nil {
		r.t.Fatal(err)
	}
	delete(r.m.present, p)
}

// renameCase renames one element of a present path — the file, or a
// directory above it — to its other case, when no other present path
// would fold onto the new spelling.
func (r *modelRun) renameCase() {
	p, ok := r.pickPresent()
	if !ok {
		return
	}
	parts := strings.Split(p, "/")
	i := r.rng.IntN(len(parts))
	from := strings.Join(parts[:i+1], "/")
	parts[i] = otherCase(parts[i])
	to := strings.Join(parts[:i+1], "/")
	moved := map[string]string{}
	for q, body := range r.m.present {
		switch {
		case q == from || strings.HasPrefix(q, from+"/"):
			moved[to+strings.TrimPrefix(q, from)] = body
		case sourceFolding.key(q) == sourceFolding.key(to) || sourceFolding.under(q, to):
			return
		}
	}
	if err := os.Rename(filepath.Join(r.f.src, filepath.FromSlash(from)), filepath.Join(r.f.src, filepath.FromSlash(to))); err != nil {
		r.t.Fatal(err)
	}
	for q := range r.m.present {
		if q == from || strings.HasPrefix(q, from+"/") {
			delete(r.m.present, q)
		}
	}
	for q, body := range moved {
		r.m.present[q] = body
	}
}

func otherCase(s string) string {
	rs := []rune(s)
	if unicode.IsUpper(rs[0]) {
		rs[0] = unicode.ToLower(rs[0])
	} else {
		rs[0] = unicode.ToUpper(rs[0])
	}
	return string(rs)
}

// push indexes the source, sometimes pushes once with a crash at a random
// transport call, then pushes cleanly and checks the destination against
// the model. On a folding destination it sometimes records, after the
// index, a second spelling of a file the destination holds: that push
// refuses the second spelling and fails, and the next index drops it,
// since no source file holds it.
func (r *modelRun) push() {
	t, f := r.t, r.f
	f.index(t)
	before := f.contentHashes(t)
	if r.m.fold.folds() && r.rng.IntN(6) == 0 {
		if r.collide() {
			return
		}
	}
	if r.rng.IntN(3) == 0 {
		r.crashingPush()
		f.checkRecordsVouch(t)
		f.checkOnlyGrew(t, before)
	}
	rep, err := f.push(t, Options{})
	if err != nil || rep.Status != store.RunStatusSuccess {
		t.Fatalf("step %d: push: status=%q err=%v warnings=%v", r.step, rep.Status, err, rep.Warnings)
	}
	r.m.push(nil)
	f.checkInvariants(t, before)
	r.checkAgainstModel()
}

// crashingPush pushes once with a crash at a random transport call: the
// process dies, or half the time the power goes, keeping a random oldest
// part of what no Flush covered.
func (r *modelRun) crashingPush() {
	at := crashAtCall(r.rng.IntN(60))
	if r.rng.IntN(2) == 0 {
		mode := []crashMode{crashBefore, crashAfter, crashMidway}[r.rng.IntN(3)]
		_, _ = r.f.pushCrashing(r.t, at, mode)
		return
	}
	seed := r.rng.Uint64()
	keep := func(n int) int { return rand.New(rand.NewPCG(seed, seed)).IntN(n + 1) }
	_, _ = r.f.pushCuttingPower(r.t, at, powerCutModes[r.rng.IntN(2)], keep)
}

// collide records another spelling of a file the destination holds as
// present and pushes: the push refuses that spelling alone and fails.
func (r *modelRun) collide() bool {
	var held []string
	for _, p := range sortedKeys(r.m.present) {
		if r.m.dest[p] == r.m.present[p] {
			held = append(held, p)
		}
	}
	if len(held) == 0 {
		return false
	}
	p := held[r.rng.IntN(len(held))]
	variant := path.Join(path.Dir(p), otherCase(path.Base(p)))
	r.f.observe(r.t, variant, "a spelling the destination cannot hold")
	before := r.f.contentHashes(r.t)
	rep, err := r.f.push(r.t, Options{})
	if err == nil || !warned(rep, variant+" is not written") {
		r.t.Fatalf("step %d: push with %s beside %s: err=%v warnings=%v, want %s refused", r.step, variant, p, err, rep.Warnings, variant)
	}
	r.m.push(map[string]bool{variant: true})
	r.f.checkRecordsVouch(r.t)
	r.f.checkOnlyGrew(r.t, before)
	return true
}

// checkAgainstModel compares the live records and the destination's tree
// with the model, name by name as the destination resolves names.
func (r *modelRun) checkAgainstModel() {
	t, f := r.t, r.f
	want := map[string]string{}
	for p, body := range r.m.dest {
		want[r.m.fold.key(p)] = p + " " + bodyHash(body)
	}
	live := map[string]string{}
	volID := f.volumeID(t)
	for _, row := range f.rows(t) {
		if row.State == store.RemotePathLive {
			live[r.m.fold.key(row.Path)] = row.Path + " " + f.contentHashOf(t, volID, row)
		}
	}
	if !mapsEqual(live, want) {
		t.Fatalf("step %d: live records = %v, want %v", r.step, live, want)
	}
	tree := r.destTree()
	for k, v := range want {
		if got := tree[k]; got != strings.SplitN(v, " ", 2)[1] {
			t.Errorf("step %d: destination holds %q at %s, want %s", r.step, got, v, bodyHash(r.m.dest[strings.SplitN(v, " ", 2)[0]]))
		}
	}
	if len(tree) != len(want) {
		t.Fatalf("step %d: destination tree = %v, want %v", r.step, tree, want)
	}
}

// destTree is the hash of every file in the volume's mirrored tree, keyed
// by folded name, outside the reserved directories and the marker.
func (r *modelRun) destTree() map[string]string {
	root := filepath.Join(r.f.dst, "pics")
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() && isReservedFolderPath(rel) {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() && rel != ".squirrel-volume" {
			out[r.m.fold.key(rel)] = hashOnDisk(p)
		}
		return nil
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

func bodyHash(body string) string {
	sum := blake3.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

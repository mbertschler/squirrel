package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/mbertschler/squirrel/store"
)

// foldProbeBase is the file a mirror push writes into staging to learn how
// the destination compares names. foldProbeCase is the same name with its
// ASCII letters in upper case, foldProbeNorm the same name decomposed.
const (
	foldProbeBase = "fold-probe-\u00e9"
	foldProbeCase = "FOLD-PROBE-\u00e9"
	foldProbeNorm = "fold-probe-e\u0301"
)

// foldProbeName is where a push probes the destination's names:
// <volume>/.squirrel-staging/fold-probe-é.
func foldProbeName(volumeDir string) string {
	return path.Join(volumeDir, StagingDirName, foldProbeBase)
}

// isFoldProbe reports whether a staging entry is the name probe, as a
// destination that stores names decomposed lists it.
func isFoldProbe(name string) bool {
	return norm.NFC.String(name) == foldProbeBase
}

// nameFolding is how a destination compares names: whether it resolves
// names that differ only by case, or only by Unicode normalization, to the
// same entry (APFS does both, exFAT folds case).
type nameFolding struct {
	caseless, normless bool
}

func (f nameFolding) folds() bool { return f.caseless || f.normless }

// key is the one spelling of s the destination resolves every equivalent
// spelling to.
func (f nameFolding) key(s string) string {
	if f.normless {
		s = norm.NFC.String(s)
	}
	if f.caseless {
		s = strings.Map(foldRune, s)
	}
	return s
}

// under reports whether rel lies below dir, as the destination resolves
// both.
func (f nameFolding) under(rel, dir string) bool {
	return strings.HasPrefix(f.key(rel), f.key(dir)+"/")
}

// foldRune maps r to the smallest rune of its simple case folding orbit,
// so every case variant of a rune maps to the same one.
func foldRune(r rune) rune {
	least := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		least = min(least, f)
	}
	return least
}

// probeFolding writes the name probe and looks it up by its upper-case and
// its decomposed spelling: whichever the destination finds, it folds. The
// probe is removed again; one a crashed push left behind is removed first.
func (w *mirrorWriter) probeFolding(ctx context.Context) (nameFolding, error) {
	dir := path.Join(w.h.vol.Name, StagingDirName)
	name := foldProbeName(w.h.vol.Name)
	if err := w.removeProbe(ctx, name); err != nil {
		return nameFolding{}, err
	}
	if err := w.tr.Put(ctx, name, strings.NewReader(""), time.Now()); err != nil {
		return nameFolding{}, fmt.Errorf("write the name probe %s: %w", name, err)
	}
	var f nameFolding
	var err error
	if f.caseless, err = present(ctx, w.tr, path.Join(dir, foldProbeCase)); err != nil {
		return nameFolding{}, fmt.Errorf("probe case folding: %w", err)
	}
	if f.normless, err = present(ctx, w.tr, path.Join(dir, foldProbeNorm)); err != nil {
		return nameFolding{}, fmt.Errorf("probe normalization folding: %w", err)
	}
	return f, w.removeProbe(ctx, name)
}

func (w *mirrorWriter) removeProbe(ctx context.Context, name string) error {
	left, err := present(ctx, w.tr, name)
	if err != nil || !left {
		return err
	}
	if err := w.tr.Remove(ctx, name); err != nil {
		return fmt.Errorf("remove the name probe %s: %w", name, err)
	}
	return nil
}

// foldPlan is how a push writes to a destination that folds names: the
// planned paths it refuses, each with the spelling it collides with, and
// for each path it writes, the versions recorded under another spelling of
// its name or a parent's, which move aside before it lands.
type foldPlan struct {
	refused  map[string]string
	displace map[string][]string
}

// foldMember is one path's claim on a folded name: a file at it, or a
// directory the path lies under.
type foldMember struct {
	path     string
	spelling string // how the path spells the name
	dir      bool
	planned  bool // the push plans the path: it is present at the source
	live     bool // a live record holds the path
}

// foldEntity is one thing the destination can hold at a folded name: a
// file under one spelling, or a directory under any.
type foldEntity struct {
	name    string
	members []foldMember
	present bool // a present path claims it
	onDest  bool // a live record of a present path claims it
}

// planFolding decides how every planned path lands on a folding
// destination. Two present paths whose names, or whose parents' names,
// fold together where one is a file cannot both land: the one the
// destination already holds wins, then the first by spelling, and every
// other is refused, displacing nothing. A version recorded under another
// spelling whose path is no longer present — a rename that changed only
// case — moves aside as its record before the planned path lands.
func (w *mirrorWriter) planFolding(ctx context.Context, ops *mirrorOps) (foldPlan, error) {
	plan := foldPlan{refused: map[string]string{}, displace: map[string][]string{}}
	groups := w.foldGroups(ops)
	isPresent := w.presentAtSource(ctx)
	var candidates []string
	for key, members := range groups {
		if collides(members) {
			candidates = append(candidates, key)
		}
	}
	sort.Strings(candidates)
	for _, key := range candidates {
		entities, err := foldEntities(groups[key], isPresent)
		if err != nil {
			return foldPlan{}, err
		}
		plan.refuse(entities)
	}
	for _, key := range candidates {
		plan.displaceOthers(groups[key], isPresent)
	}
	return plan, nil
}

// foldGroups gathers, for every folded name along a planned path, the
// claims of every planned path and live record on it.
func (w *mirrorWriter) foldGroups(ops *mirrorOps) map[string][]foldMember {
	interest := map[string]bool{}
	claims := map[string]*foldMember{}
	for _, p := range ops.paths {
		rel := p.delta.Path
		claims[rel] = &foldMember{path: rel, planned: true}
		key := w.fold.key(rel)
		for i := range key {
			if key[i] == '/' {
				interest[key[:i]] = true
			}
		}
		interest[key] = true
	}
	for rel := range w.live {
		if c, ok := claims[rel]; ok {
			c.live = true
			continue
		}
		claims[rel] = &foldMember{path: rel, live: true}
	}
	groups := map[string][]foldMember{}
	for rel, c := range claims {
		key := w.fold.key(rel)
		for i := range len(key) + 1 {
			if i < len(key) && key[i] != '/' {
				continue
			}
			if !interest[key[:i]] {
				continue
			}
			m := *c
			m.spelling, m.dir = spellingAt(rel, strings.Count(key[:i], "/")), i < len(key)
			groups[key[:i]] = append(groups[key[:i]], m)
		}
	}
	return groups
}

// spellingAt is rel cut after its first depth+1 elements.
func spellingAt(rel string, depth int) string {
	parts := strings.SplitN(rel, "/", depth+2)
	return strings.Join(parts[:depth+1], "/")
}

// collides reports whether the claims on one folded name can clash: two
// files under different spellings, or a file where a directory is.
func collides(members []foldMember) bool {
	files := map[string]bool{}
	dirs := false
	for _, m := range members {
		if m.dir {
			dirs = true
		} else {
			files[m.spelling] = true
		}
	}
	return len(files) > 1 || (len(files) == 1 && dirs)
}

// foldEntities groups one folded name's claims into what the destination
// can hold there: a file per spelling, and one directory for every
// directory spelling, which the destination merges.
func foldEntities(members []foldMember, isPresent func(string) (bool, error)) ([]foldEntity, error) {
	byName := map[string]*foldEntity{}
	var dir *foldEntity
	for _, m := range members {
		e := byName[m.spelling]
		if m.dir {
			if dir == nil {
				dir = &foldEntity{name: m.spelling}
			}
			dir.name = min(dir.name, m.spelling)
			e = dir
		} else if e == nil {
			e = &foldEntity{name: m.spelling}
			byName[m.spelling] = e
		}
		e.members = append(e.members, m)
	}
	var out []foldEntity
	if dir != nil {
		out = append(out, *dir)
	}
	for _, e := range byName {
		out = append(out, *e)
	}
	for i := range out {
		if err := out[i].weigh(isPresent); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (e *foldEntity) weigh(isPresent func(string) (bool, error)) error {
	for _, m := range e.members {
		if e.onDest {
			return nil
		}
		p := m.planned
		if !p {
			var err error
			if p, err = isPresent(m.path); err != nil {
				return err
			}
		}
		e.present = e.present || p
		e.onDest = e.onDest || (p && m.live)
	}
	return nil
}

// refuse picks the winner among the present entities at one folded name,
// the one the destination holds first, then the first by spelling, and
// refuses every planned path that claims another.
func (p foldPlan) refuse(entities []foldEntity) {
	var present []foldEntity
	for _, e := range entities {
		if e.present {
			present = append(present, e)
		}
	}
	if len(present) < 2 {
		return
	}
	sort.Slice(present, func(i, j int) bool {
		if present[i].onDest != present[j].onDest {
			return present[i].onDest
		}
		return present[i].name < present[j].name
	})
	for _, e := range present[1:] {
		for _, m := range e.members {
			if m.planned {
				if _, done := p.refused[m.path]; !done {
					p.refused[m.path] = present[0].name
				}
			}
		}
	}
}

// displaceOthers plans, for every planned path that still lands at or
// under one folded name, the move of each version recorded as a file there
// under another spelling whose path is no longer present.
func (p foldPlan) displaceOthers(members []foldMember, isPresent func(string) (bool, error)) {
	for _, stale := range members {
		if stale.dir || stale.planned || !stale.live {
			continue
		}
		if present, err := isPresent(stale.path); err != nil || present {
			continue
		}
		for _, m := range members {
			if _, refused := p.refused[m.path]; m.planned && !refused && m.path != stale.path {
				p.displace[m.path] = append(p.displace[m.path], stale.path)
			}
		}
	}
}

// presentAtSource reports whether a path is present in the volume's
// index, remembering each answer.
func (w *mirrorWriter) presentAtSource(ctx context.Context) func(string) (bool, error) {
	seen := map[string]bool{}
	return func(rel string) (bool, error) {
		if p, ok := seen[rel]; ok {
			return p, nil
		}
		r, err := w.h.store.GetByPath(ctx, w.volumeID, rel)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("look up %s: %w", rel, err)
		}
		seen[rel] = err == nil && r.Status == store.StatusPresent
		return seen[rel], nil
	}
}

// displaceSpelling moves aside the version recorded at rel, another
// spelling of a planned path's name, unless an earlier path of this push
// already did.
func (w *mirrorWriter) displaceSpelling(ctx context.Context, rel string) error {
	if _, ok := w.live[rel]; !ok {
		return nil
	}
	return w.displace(ctx, rel)
}

func (w *mirrorWriter) refuseCollision(rel, other string) {
	w.collided++
	w.rep.Warnings = append(w.rep.Warnings, fmt.Sprintf("destination %q: %s is not written: the destination cannot tell its name apart from %s (it ignores case or Unicode normalization); rename one of them at the source",
		w.h.dest.Name, rel, other))
}

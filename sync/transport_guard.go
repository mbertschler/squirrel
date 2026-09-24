package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"github.com/mbertschler/squirrel/volmark"
)

// errGuardRefused marks a move or removal the name guard does not permit.
var errGuardRefused = errors.New("the name guard refuses this operation")

// nameGuard is the only gate on what may move or disappear on a
// destination. Layouts never hold the raw transport: they get a
// guardedTransport, whose Rename and Remove pass through permit first.
type nameGuard struct {
	volumeDir string // the volume's directory under the destination root
	runID     int64  // the push holding the guard
	// content marks a content-addressed or packed push, whose commits land
	// on artifact names (see artifact); displacement is a mirror's alone.
	content bool
	// bootstrap lets a push under --init write the volume marker, before
	// it holds a run.
	bootstrap bool
	// finished reports whether a run has ended, so its staging may go.
	finished func(runID int64) bool
}

type guardOp int

const (
	opRemove guardOp = iota
	opRename
)

// permit is the whole audit surface for destroying or moving bytes on a
// destination. A bootstrap guard allows, run or not:
//
//   - Remove of the staged marker, <volume>/.squirrel-staging/volume-marker,
//     and its Rename onto <volume>/.squirrel-volume.
//
// Beyond that, a guard held by no run (runID 0, a dry run's reads)
// refuses everything; otherwise it allows:
//
//   - Remove of a staging entry, or an emptied staging run directory,
//     of a run that has finished: <volume>/.squirrel-staging/run-<id>[/<key>];
//   - Remove of a ride-along snapshot: <volume>/.squirrel-index/index-*.db;
//   - Rename from this run's staging onto a snapshot name (the ride-along),
//     or onto what the layout commits: a live name for a mirror, an
//     artifact name for a content layout (see artifact);
//   - for a mirror, Rename of a live name to the same path under this
//     run's history (a displacement):
//     <volume>/.squirrel-history/run-<this run>/<path>.
//
// Every Rename lands on a name that does not exist yet: the transport
// fails one onto an existing name.
func (g nameGuard) permit(op guardOp, name, to string) error {
	switch {
	case g.bootstrap && name == markerStagingName(g.volumeDir) && (op == opRemove || to == path.Join(g.volumeDir, volmark.MarkerName)):
		return nil
	case g.runID == 0:
	case op == opRemove:
		if g.removable(name) {
			return nil
		}
	case op == opRename:
		if rel, ok := g.liveRel(name); ok && !g.content && to == historyName(g.volumeDir, g.runID, rel) {
			return nil
		}
		if g.stagedByThisRun(name) && (g.snapshot(to) || g.commitTarget(to)) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s %q → %q", errGuardRefused, [...]string{"remove", "rename"}[op], name, to)
}

// commitTarget reports whether a staged file may be committed onto name.
func (g nameGuard) commitTarget(name string) bool {
	if g.content {
		return g.artifact(name)
	}
	_, ok := g.liveRel(name)
	return ok
}

// artifact reports whether name is one a content layout's run commits: a
// content object, objects/<hex>; a pack, packs/<hex>; this run's placement
// map, packs/map-<run>; or this run's manifest segment,
// <volume>/index/run-<run>.
func (g nameGuard) artifact(name string) bool {
	dir, base := path.Split(name)
	run := strconv.FormatInt(g.runID, 10)
	switch dir {
	case ObjectsDirName + "/":
		return isStagingKey(base)
	case PacksDirName + "/":
		return isStagingKey(base) || base == packMapPrefix+run
	case g.volumeDir + "/" + ManifestDirName + "/":
		return base == "run-"+run
	}
	return false
}

// snapshot reports whether name is a ride-along snapshot of the volume:
// <volume>/.squirrel-index/index-*.db.
func (g nameGuard) snapshot(name string) bool {
	rel, ok := strings.CutPrefix(name, g.volumeDir+"/"+IndexDirName+"/")
	return ok && !strings.Contains(rel, "/") && isSnapshotName(rel)
}

func (g nameGuard) removable(name string) bool {
	if g.snapshot(name) {
		return true
	}
	runID, key, ok := g.stagingParts(name)
	if !ok || runID == g.runID || !g.finished(runID) {
		return false
	}
	return key == "" || isStagingKey(key)
}

func (g nameGuard) stagedByThisRun(name string) bool {
	runID, key, ok := g.stagingParts(name)
	return ok && runID == g.runID && isStagingKey(key)
}

// stagingParts splits <volume>/.squirrel-staging/run-<id>[/<key>].
func (g nameGuard) stagingParts(name string) (runID int64, key string, ok bool) {
	rest, found := strings.CutPrefix(name, g.volumeDir+"/"+StagingDirName+"/run-")
	if !found {
		return 0, "", false
	}
	idText, key, _ := strings.Cut(rest, "/")
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != idText || strings.Contains(key, "/") {
		return 0, "", false
	}
	return id, key, true
}

// liveRel returns the volume-relative path of a name in the mirrored tree:
// inside the volume directory, outside every reserved directory, and not
// the volume marker.
func (g nameGuard) liveRel(name string) (string, bool) {
	rel, found := strings.CutPrefix(name, g.volumeDir+"/")
	if !found || !fs.ValidPath(rel) || rel == "." || rel == volmark.MarkerName {
		return "", false
	}
	top, _, _ := strings.Cut(rel, "/")
	if isReservedFolderPath(top) {
		return "", false
	}
	return rel, true
}

// historyName is where a displacement by runID moves the volume-relative
// path rel: <volume>/.squirrel-history/run-<runID>/<rel>.
func historyName(volumeDir string, runID int64, rel string) string {
	return path.Join(volumeDir, HistoryDirName, "run-"+strconv.FormatInt(runID, 10), rel)
}

// markerStagingBase is the staging name the volume marker is written to
// before it is renamed onto .squirrel-volume.
const markerStagingBase = "volume-marker"

// markerStagingName is where a --init push stages the volume marker:
// <volume>/.squirrel-staging/volume-marker.
func markerStagingName(volumeDir string) string {
	return path.Join(volumeDir, StagingDirName, markerStagingBase)
}

// stagingName is where runID stages the path whose key is key:
// <volume>/.squirrel-staging/run-<runID>/<key>.
func stagingName(volumeDir string, runID int64, key string) string {
	return path.Join(volumeDir, StagingDirName, "run-"+strconv.FormatInt(runID, 10), key)
}

// isStagingKey reports whether key is the staging name of one path: the
// lowercase hex BLAKE3 of the volume-relative path, so it stays unique on a
// destination that folds case.
func isStagingKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, c := range key {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// guardedTransport is the transport a layout holds: every Rename and
// Remove passes the name guard first.
type guardedTransport struct {
	transport
	guard nameGuard
}

func (t guardedTransport) Rename(ctx context.Context, from, to string) error {
	if err := t.guard.permit(opRename, from, to); err != nil {
		return err
	}
	return t.transport.Rename(ctx, from, to)
}

func (t guardedTransport) Remove(ctx context.Context, name string) error {
	if err := t.guard.permit(opRemove, name, ""); err != nil {
		return err
	}
	return t.transport.Remove(ctx, name)
}

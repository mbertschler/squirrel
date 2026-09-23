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
	// finished reports whether a run has ended, so its staging may go.
	finished func(runID int64) bool
}

type guardOp int

const (
	opRemove guardOp = iota
	opRename
)

// permit is the whole audit surface for destroying or moving bytes on a
// destination. A guard held by no run (runID 0, a dry run's reads) refuses
// everything; otherwise it allows:
//
//   - Remove of a staging entry, or an emptied staging run directory,
//     of a run that has finished: <volume>/.squirrel-staging/run-<id>[/<key>];
//   - Remove of a ride-along snapshot: <volume>/.squirrel-index/index-*.db;
//   - Rename from this run's staging onto a live name (a commit);
//   - Rename of a live name to the same path under this run's history
//     (a displacement): <volume>/.squirrel-history/run-<this run>/<path>.
func (g nameGuard) permit(op guardOp, name, to string) error {
	switch {
	case g.runID == 0:
	case op == opRemove:
		if g.removable(name) {
			return nil
		}
	case op == opRename:
		if rel, ok := g.liveRel(name); ok && to == historyName(g.volumeDir, g.runID, rel) {
			return nil
		}
		if _, ok := g.liveRel(to); ok && g.stagedByThisRun(name) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s %q → %q", errGuardRefused, [...]string{"remove", "rename"}[op], name, to)
}

func (g nameGuard) removable(name string) bool {
	if rel, ok := strings.CutPrefix(name, g.volumeDir+"/"+IndexDirName+"/"); ok {
		return !strings.Contains(rel, "/") && isSnapshotName(rel)
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

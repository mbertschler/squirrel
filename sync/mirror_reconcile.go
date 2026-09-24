package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"

	"github.com/mbertschler/squirrel/store"
)

// reconcile settles, once at push start, every move an earlier push
// recorded but may not have finished, deciding each from where the bytes
// actually are, then removes the staging finished runs left behind. The
// earlier push may have stopped before flushing its moves, so the
// directories they touched are flushed before any settlement is recorded.
func (h *mirrorHandler) reconcile(ctx context.Context, rep *Report, volumeID, runID int64) error {
	tr, err := h.root(ctx, runID)
	if err != nil {
		return err
	}
	rows, err := h.store.ListUnsettledRemotePaths(ctx, h.dest.Name, volumeID)
	if err != nil {
		return fmt.Errorf("list unsettled mirror paths on %q: %w", h.dest.Name, err)
	}
	settlements := make([]settlement, len(rows))
	var dirs []string
	for i, r := range rows {
		if settlements[i], err = h.settle(ctx, tr, r); err != nil {
			return fmt.Errorf("reconcile %s on %q: %w", r.Path, h.dest.Name, err)
		}
		dirs = append(dirs, h.movedThrough(r)...)
	}
	if len(rows) > 0 {
		if err := tr.Flush(ctx, dirs...); err != nil {
			return fmt.Errorf("flush the moves an earlier push left in flight on %q: %w", h.dest.Name, err)
		}
	}
	for i, r := range rows {
		if err := h.recordSettlement(ctx, r, settlements[i]); err != nil {
			return fmt.Errorf("reconcile %s on %q: %w", r.Path, h.dest.Name, err)
		}
	}
	return h.clearFinishedStaging(ctx, rep, tr)
}

// movedThrough is every directory along the two places an unsettled
// row's version can be: the rename that moved it may have moved a
// directory above it.
func (h *mirrorHandler) movedThrough(r store.RemotePath) []string {
	other := stagingName(h.vol.Name, r.WrittenRunID, stagingKey(r.Path))
	if r.State == store.RemotePathDisplacing {
		other = historyName(h.vol.Name, r.DisplacedRunID.Int64, r.Path)
	}
	return append(ancestors(h.liveName(r.Path)), ancestors(other)...)
}

// ancestors is every directory above name, below the root.
func ancestors(name string) []string {
	var out []string
	for d := path.Dir(name); d != "."; d = path.Dir(d) {
		out = append(out, d)
	}
	return out
}

// settlement is how reconcile resolves one unsettled row.
type settlement int

const (
	settleLive settlement = iota
	settleDisplaced
	settleLost
	settleNothing
)

// settle looks at both places an unsettled row's version can be and
// decides where it is.
func (h *mirrorHandler) settle(ctx context.Context, tr transport, r store.RemotePath) (settlement, error) {
	at, err := statMatch(ctx, tr, h.liveName(r.Path), r)
	if err != nil {
		return settleNothing, err
	}
	switch r.State {
	case store.RemotePathCommitting:
		staged, err := present(ctx, tr, stagingName(h.vol.Name, r.WrittenRunID, stagingKey(r.Path)))
		if err != nil {
			return settleNothing, err
		}
		return settleCommitting(staged, at), nil
	case store.RemotePathDisplacing:
		inHistory, err := statMatch(ctx, tr, historyName(h.vol.Name, r.DisplacedRunID.Int64, r.Path), r)
		if err != nil {
			return settleNothing, err
		}
		return settleDisplacing(inHistory, at), nil
	default:
		return settleNothing, nil
	}
}

// settleCommitting decides a committing row: its version is live once the
// staged file is gone and the path holds the recorded version. Otherwise
// the commit never happened, so the intent is withdrawn — the path stays
// in the delta, since no receipt sealed the run, and is planned again.
func settleCommitting(staged, atPath bool) settlement {
	if !staged && atPath {
		return settleLive
	}
	return settleLost
}

// settleDisplacing decides a displacing row: displaced once history holds
// the version, back to live while the path still holds it, lost when
// neither does.
func settleDisplacing(inHistory, atPath bool) settlement {
	switch {
	case inHistory:
		return settleDisplaced
	case atPath:
		return settleLive
	default:
		return settleLost
	}
}

func (h *mirrorHandler) recordSettlement(ctx context.Context, r store.RemotePath, s settlement) error {
	switch {
	case s == settleNothing:
		return nil
	case s == settleLost:
		return h.store.MarkRemotePathsLost(ctx, r.ID)
	case r.State == store.RemotePathCommitting:
		return h.store.ConfirmRemotePathsLive(ctx, r.ID)
	case s == settleDisplaced:
		return h.store.ConfirmRemotePathsDisplaced(ctx, r.ID)
	default:
		return h.store.RevertRemotePathsLive(ctx, r.ID)
	}
}

// statMatch reports whether name holds the version r records.
func statMatch(ctx context.Context, tr transport, name string, r store.RemotePath) (bool, error) {
	e, err := tr.Stat(ctx, name)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errParentNotDir) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return matchesRecord(e, r), nil
}

func present(ctx context.Context, tr transport, name string) (bool, error) {
	_, err := tr.Stat(ctx, name)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errParentNotDir) {
		return false, nil
	}
	return err == nil, err
}

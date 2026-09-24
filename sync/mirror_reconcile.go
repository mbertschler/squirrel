package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/mbertschler/squirrel/store"
)

// reconcile settles, once at push start, every move an earlier push
// recorded but may not have finished, deciding each from where the bytes
// actually are, then removes the staging finished runs left behind.
func (h *mirrorHandler) reconcile(ctx context.Context, rep *Report, volumeID, runID int64) error {
	tr, err := h.root(ctx, runID)
	if err != nil {
		return err
	}
	rows, err := h.store.ListUnsettledRemotePaths(ctx, h.dest.Name, volumeID)
	if err != nil {
		return fmt.Errorf("list unsettled mirror paths on %q: %w", h.dest.Name, err)
	}
	for _, r := range rows {
		if err := h.settle(ctx, tr, r); err != nil {
			return fmt.Errorf("reconcile %s on %q: %w", r.Path, h.dest.Name, err)
		}
	}
	return h.clearFinishedStaging(ctx, rep, tr)
}

// settlement is how reconcile resolves one unsettled row.
type settlement int

const (
	settleLive settlement = iota
	settleDisplaced
	settleLost
)

// settle looks at both places an unsettled row's version can be and
// records where it is.
func (h *mirrorHandler) settle(ctx context.Context, tr transport, r store.RemotePath) error {
	at, err := statMatch(ctx, tr, h.liveName(r.Path), r)
	if err != nil {
		return err
	}
	var s settlement
	switch r.State {
	case store.RemotePathCommitting:
		staged, err := present(ctx, tr, stagingName(h.vol.Name, r.WrittenRunID, stagingKey(r.Path)))
		if err != nil {
			return err
		}
		s = settleCommitting(staged, at)
	case store.RemotePathDisplacing:
		inHistory, err := statMatch(ctx, tr, historyName(h.vol.Name, r.DisplacedRunID.Int64, r.Path), r)
		if err != nil {
			return err
		}
		s = settleDisplacing(inHistory, at)
	default:
		return nil
	}
	return h.recordSettlement(ctx, r, s)
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

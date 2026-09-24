package sync

import (
	"context"
	"fmt"

	"github.com/mbertschler/squirrel/store"
)

// mirrorOps is the mirror translation of a plan: one operation per present
// delta path and per repair, and the live records the operations were
// decided against. execute keeps live current as it moves versions.
type mirrorOps struct {
	paths []mirrorPath
	live  map[string]store.RemotePath // volume-relative path → live row
	// inSync counts the live rows that hold their path's present content,
	// in the delta or not: what the report calls already correct.
	inSync int64
	// repairs counts the paths planned because the destination lost their
	// current content, beyond the delta.
	repairs int64
}

// mirrorPath is one planned path. When the live record already holds the
// delta's content, the path is only confirmed; otherwise the content is
// written, displacing whatever the path holds.
type mirrorPath struct {
	delta store.PathDelta
	// current: the path's live record holds the delta's content.
	current bool
}

// translate applies the mirror's translation rules to the delta, then adds
// the repairs: present paths whose current content the destination lost
// (a push or a verify pass found other bytes there, or none), written
// again although the index did not change. Rows that are missing,
// offloaded or superseded produce nothing: a destination keeps content the
// source no longer has.
//
//	present, live record holds C       → confirm (already correct)
//	present, live record holds other   → write C, displacing the recorded version
//	present, no live record            → write C, displacing any unrecorded entry
//	repair: a lost record holds C      → write C, displacing whatever is there
func (h *mirrorHandler) translate(ctx context.Context, p pushPlan) (*mirrorOps, error) {
	rows, err := h.store.ListLiveRemotePaths(ctx, h.dest.Name, p.volumeID)
	if err != nil {
		return nil, fmt.Errorf("list live mirror paths on %q: %w", h.dest.Name, err)
	}
	inSync, err := h.store.CountInSyncRemotePaths(ctx, h.dest.Name, p.volumeID)
	if err != nil {
		return nil, err
	}
	ops := &mirrorOps{live: make(map[string]store.RemotePath, len(rows)), inSync: inSync}
	for _, r := range rows {
		ops.live[r.Path] = r
	}
	planned := make(map[string]bool, len(p.delta))
	for _, d := range p.delta {
		if d.Status != store.StatusPresent {
			continue
		}
		planned[d.Path] = true
		live, ok := ops.live[d.Path]
		ops.paths = append(ops.paths, mirrorPath{delta: d, current: ok && live.ContentID == d.ContentID})
	}
	return ops, h.addRepairs(ctx, p.volumeID, ops, planned)
}

// addRepairs plans every repair the delta does not already cover.
func (h *mirrorHandler) addRepairs(ctx context.Context, volumeID int64, ops *mirrorOps, planned map[string]bool) error {
	repairs, err := h.store.ListRemotePathRepairs(ctx, h.dest.Name, volumeID)
	if err != nil {
		return fmt.Errorf("list lost mirror paths on %q: %w", h.dest.Name, err)
	}
	for _, d := range repairs {
		if planned[d.Path] {
			continue
		}
		ops.paths = append(ops.paths, mirrorPath{delta: d})
		ops.repairs++
	}
	return nil
}

// preview counts what execute would do: written paths as Transferred and
// Bytes, confirmed ones as Checked, and every in-sync path as already
// correct.
func (o *mirrorOps) preview(rep *Report) {
	rep.AlreadyCorrect = o.inSync
	for _, p := range o.paths {
		if p.current {
			rep.RcloneResult.Checked++
			continue
		}
		rep.RcloneResult.Transferred++
		rep.RcloneResult.Bytes += p.delta.SizeBytes
	}
}

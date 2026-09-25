package sync

import (
	"context"
	"fmt"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// pushPlan is what one push must land: the volume's index changes since the
// destination's last confirmed run, and the durability snapshot the push
// advances to once they land.
type pushPlan struct {
	volumeID  int64
	watermark int64                   // last confirmed run; 0 for a fresh destination
	delta     []store.PathDelta       // ListPathDeltaSince(volumeID, watermark)
	advance   []store.OriginComponent // captured before any transfer; nil on a dry run
}

// operations is one layout's translation of a plan into work.
type operations interface {
	// preview records on rep what executing the operations would transfer.
	// It is all a dry run reports.
	preview(rep *Report)
}

// layout is one destination layout's part of a push. pushThrough runs every
// layout through the same sequence, so the watermark rule, run allocation
// and durability advance are decided once for all of them.
type layout[O operations] interface {
	// markers gates the push on the destination's markers, writing missing
	// ones under opts.Init. A dry run only checks.
	markers(ctx context.Context, rep *Report, volumeID int64, opts Options) error
	// landed reports whether runID left this layout's landing evidence.
	landed(ctx context.Context, runID int64) (bool, error)
	// rootEmpty reports whether the destination root holds nothing beyond
	// squirrel's markers.
	rootEmpty(ctx context.Context) (bool, error)
	// foreignHistory is the refusal for a last success at runID that left
	// no landing evidence on a destination that is no fresh start.
	foreignHistory(runID int64) error
	// firstPush gates a push of a volume that never synced to the
	// destination, from watermark 0, on what the destination already
	// holds: nil, or the refusal.
	firstPush(ctx context.Context, volumeID int64) error
	// reconcile settles, once at push start, whatever an earlier push left
	// in flight on the destination, so translate reads settled records.
	reconcile(ctx context.Context, rep *Report, volumeID, runID int64) error
	// translate turns the plan into this layout's operations. It reads
	// squirrel's records and writes nothing: a dry run is translate alone.
	translate(ctx context.Context, p pushPlan) (O, error)
	// execute performs the operations and records each confirmed one.
	execute(ctx context.Context, rep *Report, runID int64, ops O) error
	// seal writes the run's landing evidence once every operation is
	// confirmed.
	seal(ctx context.Context, rep *Report, runID int64, p pushPlan, ops O) error
	// advanceMethod names the evidence the confirmed landing earns. An
	// empty method holds the durability vector where it is.
	advanceMethod(ctx context.Context, rep *Report, p pushPlan) (string, error)
	// shelf is the destination's .squirrel-index/ directory, where
	// runID's ride-along index snapshot lands.
	shelf(runID int64) snapshotShelf
}

// pushTarget is the (volume, destination) pair one layout push serves.
type pushTarget struct {
	store *store.Store
	vol   *config.Volume
	dest  *config.Destination
}

// pushThrough is the single push driver every layout shares:
//
//	requireIndexedVolume → markers → begin run → reconcile → plan → translate → execute → seal → advance → finish → ride-along
//
// A dry run stops after translate and reports the operations' preview; it
// writes no runs row. The runs row records shallow=true: every layout
// confirms what landed by presence and size, and the audit trail says so.
func pushThrough[O operations](ctx context.Context, t pushTarget, l layout[O], opts Options) (Report, error) {
	rep := Report{Volume: t.vol.Name, Destination: t.dest.Name, Layout: t.dest.Layout}
	// Stamped up front so output renderers key their formatting off the
	// method even when the push fails early.
	rep.Verification.Method = VerifyMethodPresenceSize
	volID, err := requireIndexedVolume(ctx, t.store, t.vol)
	if err != nil {
		return rep, err
	}
	if err := l.markers(ctx, &rep, volID, opts); err != nil {
		return rep, err
	}
	if opts.DryRun {
		return rep, previewPush(ctx, t, l, &rep, volID)
	}
	runID, err := beginSyncRunGuarded(ctx, t.store, false, store.SyncRunSpec{
		VolumeID:    volID,
		Destination: t.dest.Name,
		Shallow:     true,
	}, t.vol.Name)
	if err != nil {
		return rep, err
	}
	rep.RunID = runID
	if opts.OnRunID != nil {
		opts.OnRunID(runID)
	}
	err = landPush(ctx, t, l, &rep, volID, runID)
	finishHandlerRun(ctx, t.store, &rep, err)
	opts.Snapshot.afterSync(ctx, &rep, l.shelf(runID))
	return rep, err
}

// previewPush computes the same plan and translation a real push would,
// then reports the translation's preview. The watermark rule still reads
// the destination, so a dry run refuses exactly what a push would.
func previewPush[O operations](ctx context.Context, t pushTarget, l layout[O], rep *Report, volID int64) error {
	p, err := planPush(ctx, t, l, volID, false)
	if err != nil {
		return err
	}
	rep.Verification.Files = int64(len(p.delta))
	ops, err := l.translate(ctx, p)
	if err != nil {
		return err
	}
	ops.preview(rep)
	rep.Status = store.RunStatusSuccess
	return nil
}

// landPush is the transactional landing: plan → operations → evidence →
// vector. rep.Status starts failed and is promoted only after the vector
// advanced: a confirmed landing whose evidence failed to record must not
// present as success, or the next watermark would skip past it.
func landPush[O operations](ctx context.Context, t pushTarget, l layout[O], rep *Report, volID, runID int64) error {
	rep.Status = store.RunStatusFailed
	if err := l.reconcile(ctx, rep, volID, runID); err != nil {
		return err
	}
	p, err := planPush(ctx, t, l, volID, true)
	if err != nil {
		return err
	}
	rep.Verification.Files = int64(len(p.delta))
	// The delta is what this push newly records at the destination, so an
	// empty one is a genuine no-op, which is what the runs fold keys on
	// (#182).
	rep.Changed = knownChanged(int64(len(p.delta)))
	ops, err := l.translate(ctx, p)
	if err != nil {
		return err
	}
	if err := l.execute(ctx, rep, runID, ops); err != nil {
		return err
	}
	if err := l.seal(ctx, rep, runID, p, ops); err != nil {
		return err
	}
	method, err := l.advanceMethod(ctx, rep, p)
	if err != nil {
		return err
	}
	if method != "" && len(p.advance) > 0 {
		if err := t.store.AdvanceDestinationVectorTo(ctx, volID, t.dest.Name, method, p.advance); err != nil {
			return fmt.Errorf("advance destination vector for %s: %w", t.dest.Name, err)
		}
	}
	rep.Status = store.RunStatusSuccess
	rep.Verification.Bytes = rep.RcloneResult.Bytes
	return nil
}

// planPush resolves the watermark and the delta after it. A real push also
// captures the durability advance, before the delta, so a row committed
// mid-push is never folded into it.
func planPush[O operations](ctx context.Context, t pushTarget, l layout[O], volID int64, captureAdvance bool) (pushPlan, error) {
	watermark, err := pushWatermark(ctx, t, l, volID)
	if err != nil {
		return pushPlan{}, err
	}
	p := pushPlan{volumeID: volID, watermark: watermark}
	if captureAdvance {
		if p.advance, err = captureDurabilityAdvance(ctx, t.store, volID); err != nil {
			return pushPlan{}, err
		}
	}
	if p.delta, err = t.store.ListPathDeltaSince(ctx, volID, watermark); err != nil {
		return pushPlan{}, fmt.Errorf("compute path delta since run %d: %w", watermark, err)
	}
	return p, nil
}

// pushWatermark applies the watermark rule every layout shares:
//
//  1. no successful sync of this (volume, destination): 0, once the
//     layout's firstPush gate passes;
//  2. the last success left its landing evidence: that run's id;
//  3. the evidence is absent but the destination is a fresh start
//     (freshStart): 0;
//  4. otherwise refuse, because a delta computed against a history this
//     destination no longer shows would silently skip content.
func pushWatermark[O operations](ctx context.Context, t pushTarget, l layout[O], volID int64) (int64, error) {
	last, err := t.store.LatestSuccessfulSyncRun(ctx, volID, t.dest.Name)
	if store.IsNotFound(err) {
		return 0, l.firstPush(ctx, volID)
	}
	if err != nil {
		return 0, fmt.Errorf("lookup last successful sync of %s: %w", t.dest.Name, err)
	}
	landed, err := l.landed(ctx, last.ID)
	if err != nil {
		return 0, fmt.Errorf("destination %q: look up the landing evidence of run %d: %w", t.dest.Name, last.ID, err)
	}
	if landed {
		return last.ID, nil
	}
	fresh, err := freshStart(ctx, t, l)
	if err != nil {
		return 0, err
	}
	if fresh {
		return 0, nil
	}
	return 0, l.foreignHistory(last.ID)
}

// freshStart reports whether a destination whose last success left no
// landing evidence may start over from watermark 0: its root holds nothing
// beyond squirrel's markers, and squirrel holds no upload records for it.
// An empty root with records left behind was wiped without a `squirrel
// destination reset`; starting over would skip every upload those records
// still claim and close the run as success.
func freshStart[O operations](ctx context.Context, t pushTarget, l layout[O]) (bool, error) {
	empty, err := l.rootEmpty(ctx)
	if err != nil {
		return false, fmt.Errorf("destination %q: check whether the root is empty: %w", t.dest.Name, err)
	}
	if !empty {
		return false, nil
	}
	recorded, err := t.store.DestinationHasUploadRecords(ctx, t.dest.Name)
	if err != nil {
		return false, err
	}
	return !recorded, nil
}

package offload

import (
	"context"
	"time"

	"github.com/mbertschler/squirrel/store"
)

// ReadinessOptions selects the volume and durability policy for a
// Readiness evaluation. It mirrors the durability-relevant subset of
// Options: no path/age selectors (readiness is always whole-volume) and no
// DryRun flag (readiness never touches disk or opens a run).
type ReadinessOptions struct {
	// VolumeID is the local volumes row the gate is evaluated against.
	VolumeID int64
	// Require is the volume's offload_requires policy. Empty means the
	// volume has no offload policy, and Readiness reports Applicable=false
	// without evaluating anything.
	Require []string
	// MaxEvidenceAge is the volume's offload_max_evidence_age; it feeds the
	// same time-based staleness gate an offload would apply. Zero disables
	// the staleness policy.
	MaxEvidenceAge time.Duration
	// VerifyCadenced is Options.VerifyCadenced: the required targets under
	// an effective local verify cadence, from
	// config.Config.VerifyCadencedTargets. It must be the same set the real
	// offload passes — it widens the gate for locally-advanced
	// fingerprint-verified components, so a readiness call that omitted it
	// would report fewer bytes than an offload would actually move. Nil is
	// a valid empty set.
	VerifyCadenced map[string]bool
}

// ReadinessReport totals what an offload of the whole volume would move
// right now. OffloadableFiles/Bytes are the subset of the present set that
// passes the durability gate; PresentFiles/Bytes are the whole gated
// candidate set, so a UI can render "N of M".
type ReadinessReport struct {
	// Applicable is false when the volume declares no offload policy — the
	// caller can then distinguish "nothing offloadable" from "offload not
	// configured here" without inspecting the totals.
	Applicable       bool
	OffloadableFiles int
	OffloadableBytes int64
	PresentFiles     int
	PresentBytes     int64
}

// Readiness reports how many present files, and how many bytes, currently
// pass the offload gate for the volume — the "N GB offloadable now"
// decision-support number (friction log F17) without the side effects of
// `offload --dry-run`. It reads only the local index (durability vectors,
// scan-back fingerprints), opens no run, reads no disk bytes, validates no
// volume marker, and deletes nothing, so it is safe to call from a
// read-only introspection surface at any time.
//
// The gate is evaluated exactly as Offload's dry-run path evaluates it —
// same loadGate, same present-and-not-reserved candidate predicate, same
// per-file check — so the totals match what an offload would actually move.
// Totals are order-independent, so this walks the index map in a single
// pass rather than building and sorting an intermediate candidate slice.
func Readiness(ctx context.Context, s *store.Store, opts ReadinessOptions) (ReadinessReport, error) {
	if len(opts.Require) == 0 {
		return ReadinessReport{Applicable: false}, nil
	}
	now := time.Now()
	g, err := loadGate(ctx, s, opts.VolumeID, opts.Require, now.UnixNano(), opts.MaxEvidenceAge, opts.VerifyCadenced)
	if err != nil {
		return ReadinessReport{}, err
	}
	rows, err := s.LoadVolumeIndex(ctx, opts.VolumeID)
	if err != nil {
		return ReadinessReport{}, err
	}
	rep := ReadinessReport{Applicable: true}
	for p, row := range rows {
		if err := ctx.Err(); err != nil {
			return ReadinessReport{}, err
		}
		if row.Status != store.StatusPresent || underReservedSubtree(p) {
			continue
		}
		rep.PresentFiles++
		rep.PresentBytes += row.SizeBytes
		failures, err := g.check(ctx, row)
		if err != nil {
			return ReadinessReport{}, err
		}
		if len(failures) == 0 {
			rep.OffloadableFiles++
			rep.OffloadableBytes += row.SizeBytes
		}
	}
	return rep, nil
}

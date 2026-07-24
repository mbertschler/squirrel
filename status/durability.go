package status

import (
	"context"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// buildDurability computes the local-content durability of a volume on one
// target. Durability is tracked when the target is offload-required or when
// a vector component for it already exists; a non-required target with no
// recorded evidence returns nil (nothing to report).
func buildDurability(ctx context.Context, s *store.Store, volID int64, self store.Node, selfMax int64, vol *config.Volume, name string, now time.Time) (*Durability, error) {
	comp, hasComp, err := selfComponent(ctx, s, volID, name, self.ID)
	if err != nil {
		return nil, err
	}
	required := contains(vol.OffloadRequires, name)
	if !required && !hasComp {
		return nil, nil
	}
	d := &Durability{
		LocalContent:   selfMax > 0,
		MaxEvidenceAge: vol.OffloadMaxEvidenceAge,
	}
	if hasComp {
		d.Method = comp.VerifyMethod
		d.Covered = comp.OriginRunID >= selfMax
		d.EvidenceAge = evidenceAge(comp.VerifiedAtNs, now)
	}
	d.Stale = d.LocalContent && evidenceStale(d)
	d.Level = durabilityLevel(d, required)
	return d, nil
}

// evidenceStale reports whether a max-evidence-age policy is set and the
// covering component's verification is older than it. It is fail-closed:
// an unknown evidence age (no component, or one that never carried a
// verification instant) counts as stale.
func evidenceStale(d *Durability) bool {
	if d.MaxEvidenceAge <= 0 {
		return false
	}
	if d.EvidenceAge == nil {
		return true
	}
	return *d.EvidenceAge > d.MaxEvidenceAge
}

// durabilityLevel scores the durability dimension. Content that does not
// originate locally is vacuously safe (neutral). A required target
// escalates to amber when local content is not yet covered or the evidence
// has aged out; a non-required target never escalates "am I safe?" — it
// shows green when healthy and neutral otherwise, since offload does not
// gate on it.
func durabilityLevel(d *Durability, required bool) Level {
	if !d.LocalContent {
		return LevelNeutral
	}
	if !required {
		if d.Covered && !d.Stale {
			return LevelOK
		}
		return LevelNeutral
	}
	if !d.Covered || d.Stale {
		return LevelWarn
	}
	return LevelOK
}

// selfComponent returns the target's durability-vector component for this
// node's own origin — the coverage of locally-introduced content, which is
// what the offload gate checks on an edge machine. The bool is false when
// the target has no component for the self node.
func selfComponent(ctx context.Context, s *store.Store, volID int64, name string, selfID int64) (store.DestinationRunID, bool, error) {
	comps, err := s.ListDestinationRunIDs(ctx, volID, name)
	if err != nil {
		return store.DestinationRunID{}, false, err
	}
	for _, c := range comps {
		if c.OriginNodeID == selfID {
			return c, true, nil
		}
	}
	return store.DestinationRunID{}, false, nil
}

// selfOriginMax returns the highest origin run this node contributes to the
// volume's present set — the coverage floor the durability vector must
// reach for local content to count as durable. Zero means this node
// originates no present content in the volume.
func selfOriginMax(ctx context.Context, s *store.Store, volID, selfID int64) (int64, error) {
	comps, err := s.PresentOriginMaxima(ctx, volID, selfID)
	if err != nil {
		return 0, err
	}
	for _, c := range comps {
		if c.OriginNodeID == selfID {
			return c.OriginRunID, nil
		}
	}
	return 0, nil
}

package status

import (
	"context"
	"sort"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// The fleet view (#187): standing on any machine, for a volume that machine
// holds, name every other place the volume lives and say how current each
// copy is. It closes the standing open problem in design/ux-principles.md
// §3 — five databases, one question — without a new exchange or a new
// table.
//
// The whole answer is already in the local index. A place's durability
// vector records, per origin node, the highest origin run known durable
// there; the volume's own present files carry those same coordinates. What
// the vector does not reach has not arrived; what it reaches beyond this
// node's own knowledge is content held somewhere this node has never seen.
// Both directions fall out of comparing two sets of watermarks.
//
// The price of computing it locally is currency: every figure is only as
// recent as the last exchange with the place it describes. That is why
// AsOfAgo is not optional and why a place gone dark reads unknown rather
// than fine — a trust surface presenting stale hearsay as fact would fail
// principle 3 more thoroughly than having no fleet view at all.

// buildFleet assembles the volume's fleet rows: one per other place the
// volume lives, its configured targets first (in the grid's order) and then
// any place the index knows about that the config no longer names. Only
// called for a volume with an index row — a volume that has never been
// indexed lives nowhere yet, and its targets already say so.
func buildFleet(ctx context.Context, s *store.Store, cfg *config.Config, vol *config.Volume, volID, selfID int64, targets []TargetStatus, now time.Time) ([]FleetPlace, error) {
	ev, err := readFleetEvidence(ctx, s, volID, selfID)
	if err != nil {
		return nil, err
	}
	var out []FleetPlace
	for _, ref := range fleetRefs(cfg, targets, ev) {
		place, err := buildFleetPlace(ctx, s, vol, volID, ref, ev, now)
		if err != nil {
			return nil, err
		}
		out = append(out, place)
	}
	return out, nil
}

// fleetEvidence is everything one volume's fleet rows are computed from,
// read once per build rather than once per place.
type fleetEvidence struct {
	// comps is the volume's whole durability vector keyed by place name:
	// what this node believes each place holds, per origin node.
	comps map[string][]store.DestinationRunID
	// present buckets the volume's present files by origin coordinate, so
	// the components a place is missing convert into a file count.
	present []store.OriginFileCount
	// known is the highest origin run per origin node this volume has ever
	// observed here — the floor the "ahead" inference reads against.
	known map[int64]int64
	// peers is the peers this volume has exchanged with, keyed by name. On
	// a hub this is the only trace of an edge machine that pushes to it.
	peers map[string]store.VolumePeerSync
}

func readFleetEvidence(ctx context.Context, s *store.Store, volID, selfID int64) (fleetEvidence, error) {
	ev := fleetEvidence{comps: map[string][]store.DestinationRunID{}, known: map[int64]int64{}, peers: map[string]store.VolumePeerSync{}}
	comps, err := s.ListVolumeDestinationRunIDs(ctx, volID)
	if err != nil {
		return fleetEvidence{}, err
	}
	for _, c := range comps {
		ev.comps[c.Destination] = append(ev.comps[c.Destination], c)
	}
	// The same present set PresentOriginMaxima reduces to per-origin
	// maxima for the durability gate, bucketed instead of reduced: the
	// fleet needs how many files sit above a watermark, not just whether
	// any do.
	if ev.present, err = s.PresentFilesByOrigin(ctx, volID, selfID); err != nil {
		return fleetEvidence{}, err
	}
	maxima, err := s.KnownOriginMaxima(ctx, volID, selfID)
	if err != nil {
		return fleetEvidence{}, err
	}
	for _, m := range maxima {
		ev.known[m.OriginNodeID] = m.OriginRunID
	}
	peers, err := s.ListVolumePeerSyncStates(ctx, volID)
	if err != nil {
		return fleetEvidence{}, err
	}
	for _, p := range peers {
		ev.peers[p.PeerName] = p
	}
	return ev, nil
}

// fleetRef names one place to build a row for, carrying its target row when
// the config declares it as one (so the row reuses facts the grid already
// read) and nil when it does not.
type fleetRef struct {
	name   string
	kind   TargetKind
	target *TargetStatus
}

// fleetRefs orders the places: the volume's configured targets first, in
// the grid's own order, then every other place the index knows the volume
// lives on, by name. The second group is what makes the view a fleet rather
// than a restatement of the grid — a peer that pushes here and is named in
// no target list, or a destination dropped from the config that still holds
// copies of this content, both belong in the answer to "where else does
// this volume live".
func fleetRefs(cfg *config.Config, targets []TargetStatus, ev fleetEvidence) []fleetRef {
	refs := make([]fleetRef, 0, len(targets)+len(ev.peers))
	seen := make(map[string]struct{}, len(targets))
	for i := range targets {
		refs = append(refs, fleetRef{name: targets[i].Name, kind: targets[i].Kind, target: &targets[i]})
		seen[targets[i].Name] = struct{}{}
	}
	var extra []string
	for name := range ev.comps {
		if _, dup := seen[name]; !dup {
			seen[name] = struct{}{}
			extra = append(extra, name)
		}
	}
	for name := range ev.peers {
		if _, dup := seen[name]; !dup {
			seen[name] = struct{}{}
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		refs = append(refs, fleetRef{name: name, kind: targetKind(cfg, name)})
	}
	return refs
}

// buildFleetPlace assembles one row: what the place is missing, whether it
// holds anything this node has not seen, the three ages, and the severity.
func buildFleetPlace(ctx context.Context, s *store.Store, vol *config.Volume, volID int64, ref fleetRef, ev fleetEvidence, now time.Time) (FleetPlace, error) {
	comps := ev.comps[ref.name]
	p := FleetPlace{
		Name:         ref.name,
		Kind:         ref.kind,
		MissingKnown: len(comps) > 0,
		Ahead:        fleetAhead(ev.known, comps),
	}
	if p.MissingKnown {
		p.Missing = fleetMissing(ev.present, comps)
	}
	if ref.target != nil {
		p.SyncTarget, p.Required = ref.target.SyncTarget, ref.target.Required
	}
	p.State = fleetState(p)
	lastSync, err := fleetLastSync(ctx, s, volID, ref, now)
	if err != nil {
		return FleetPlace{}, err
	}
	peer, hasPeer := ev.peers[ref.name]
	p.LastChangeAgo, p.LastVerifiedAgo, p.AsOfAgo = fleetTimes(comps, lastSync, hasPeer, peer.LastSyncedAtNs, now)
	budget := fleetStaleBudget(p, vol)
	p.Stale = budget > 0 && (p.AsOfAgo == nil || *p.AsOfAgo > budget)
	p.Level = fleetLevel(p, vol)
	return p, nil
}

// fleetLastSync is the age of this node's last successful push to the
// place. A place with a target row already carries it; one without needs
// the lookup, which is why the extra places are the rare ones.
func fleetLastSync(ctx context.Context, s *store.Store, volID int64, ref fleetRef, now time.Time) (*time.Duration, error) {
	if ref.target != nil {
		return ref.target.LastSyncAgo, nil
	}
	run, err := latestSuccessfulSync(ctx, s, volID, ref.name)
	if err != nil {
		return nil, err
	}
	return ageOf(run, now), nil
}

// fleetMissing counts the present files whose origin coordinate the place's
// recorded coverage does not reach. An origin node with no component at all
// covers nothing, so all of its files count — no evidence is not the same
// as evidence of arrival.
func fleetMissing(present []store.OriginFileCount, comps []store.DestinationRunID) int {
	covered := make(map[int64]int64, len(comps))
	for _, c := range comps {
		covered[c.OriginNodeID] = c.OriginRunID
	}
	var missing int
	for _, bucket := range present {
		if covered[bucket.OriginNodeID] < bucket.OriginRunID {
			missing += int(bucket.Files)
		}
	}
	return missing
}

// fleetAhead reports whether the place's recorded coverage runs past what
// this node has ever seen from that origin — content it holds and this one
// does not. It fires only on evidence that arrived relayed (this node's own
// pushes can never claim more than it holds), which is precisely the case
// the hub and the roaming laptop each want: somebody else put content
// there.
//
// A watermark is not an inventory, so this cannot say how many files, only
// that there are some. Counting them needs the folder Merkle work (#44) or
// a sync-plan negotiation; until then the row reports the direction and
// scores nothing on it.
func fleetAhead(known map[int64]int64, comps []store.DestinationRunID) bool {
	for _, c := range comps {
		if c.OriginRunID > known[c.OriginNodeID] {
			return true
		}
	}
	return false
}

// fleetState folds the two directions into the row's headline. Coverage
// this node cannot speak to at all is unknown rather than "same" — the
// zero value of the comparison must not read as agreement.
func fleetState(p FleetPlace) FleetState {
	switch {
	case !p.MissingKnown && p.Ahead:
		return FleetAhead
	case !p.MissingKnown:
		return FleetUnknown
	case p.Missing > 0 && p.Ahead:
		return FleetDiverged
	case p.Missing > 0:
		return FleetBehind
	case p.Ahead:
		return FleetAhead
	default:
		return FleetSame
	}
}

// fleetTimes derives the row's three ages. LastChange is the freshest
// moment content there is known to have moved — this node's own successful
// push, or an exchange the peer initiated. AsOf additionally counts every
// durability component that landed for the place, because a component
// arriving from a peer refreshes what this node knows without anything
// moving there. LastVerified is the *oldest* verification among the
// components, and unknown as soon as one carries none: a place must not
// read freshly checked because one of its components was.
func fleetTimes(comps []store.DestinationRunID, lastSync *time.Duration, hasPeer bool, peerSyncedAtNs int64, now time.Time) (lastChange, lastVerified, asOf *time.Duration) {
	lastChange = lastSync
	if hasPeer {
		lastChange = fresher(lastChange, ageSince(peerSyncedAtNs, now))
	}
	asOf = lastChange
	anyUnverified := len(comps) == 0
	for _, c := range comps {
		asOf = fresher(asOf, ageSince(c.UpdatedAtNs, now))
		if !c.VerifiedAtNs.Valid {
			anyUnverified = true
			continue
		}
		lastVerified = older(lastVerified, ageSince(c.VerifiedAtNs.Int64, now))
	}
	if anyUnverified {
		lastVerified = nil
	}
	return lastChange, lastVerified, asOf
}

// fleetStaleBudget is how old a row's facts may get before it stops
// claiming them as current, taken from what the operator actually declared
// about the relationship: the sync cadence for a place this node pushes to
// (at the same lateFactor multiple past which the grid calls a pair
// stalled), the offload evidence policy for a place whose evidence only
// arrives relayed. Zero — no declared expectation — means no staleness
// judgement, the same stance cadenceLevel takes toward an unscheduled pair.
func fleetStaleBudget(p FleetPlace, vol *config.Volume) time.Duration {
	switch {
	case p.SyncTarget && vol.SyncEvery > 0:
		return lateFactor * vol.SyncEvery
	case p.Required && vol.OffloadMaxEvidenceAge > 0:
		return vol.OffloadMaxEvidenceAge
	default:
		return 0
	}
}

// fleetLevel scores the row. The dimension a fleet row adds over the target
// grid is *currency*, not coverage: files not yet at a place this node
// syncs to are the normal state between two syncs, and it is the cadence —
// not the backlog — that says whether that is fine. So the score is the age
// of what this node knows, judged against the relationship's own budget.
//
// Coverage this node cannot speak to deliberately does not escalate here.
// A pair that verifies by size+mtime — a shallow sync, or any crypt
// destination, which cannot do better — records no durability component at
// all, so its row reads unknown forever; scoring that amber would leave the
// reference household permanently off green for a destination behaving
// exactly as configured. The grid already ambers evidence an offload policy
// actually needs, and FleetStateLabel says "unknown" out loud, which is the
// honest report: not fine, not an alarm.
//
// A place this node puts nothing on (an edge machine that pushes here) is
// informational for the same reason — it is not this node's job to keep
// content there.
//
// The consequence is that a fleet row never reddens a report the grid calls
// green: its as-of is at least as fresh as the last successful sync the
// grid scores, so this can only agree with it or be gentler.
func fleetLevel(p FleetPlace, vol *config.Volume) Level {
	if !p.SyncTarget && !p.Required {
		return LevelNeutral
	}
	if p.AsOfAgo == nil {
		return LevelWarn
	}
	if p.SyncTarget {
		return cadenceLevel(*p.AsOfAgo, vol.SyncEvery)
	}
	if vol.OffloadMaxEvidenceAge > 0 && *p.AsOfAgo > vol.OffloadMaxEvidenceAge {
		return LevelWarn
	}
	return LevelOK
}

// fresher returns the more recent of two ages (the smaller duration), nil
// meaning "no such moment".
func fresher(a, b *time.Duration) *time.Duration {
	if a == nil || (b != nil && *b < *a) {
		return b
	}
	return a
}

// older returns the less recent of two ages (the larger duration), nil
// meaning "no such moment".
func older(a, b *time.Duration) *time.Duration {
	if a == nil || (b != nil && *b > *a) {
		return b
	}
	return a
}

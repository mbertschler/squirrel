package sync

import (
	"context"
	"errors"
	"fmt"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/syncproto"
)

// maxDurabilityDropSamples caps how many dropped entries the report
// retains as detail. The Dropped count stays exact; only the sampled
// Drops slice is bounded, so an adversarial peer flooding out-of-scope
// destinations cannot blow up the report or the output that renders it.
const maxDurabilityDropSamples = 16

// maxOriginNodesPerPull bounds how many distinct origin-node names one
// durability pull will resolve (and so, for novel names, create as local
// nodes rows). A real volume's content originates on a handful of nodes;
// the cap is deliberately generous and exists only to convert a runaway
// peer into a loud refusal rather than to police a legitimate topology.
const maxOriginNodesPerPull = 256

// DurabilityPullReport summarises one durability metadata pull from a
// peer: how many entries (vector components and freshness coordinates)
// were fetched, how many components landed in the local
// destination_run_ids (advanced or re-confirmed), how many were refused
// as rewinds, and how many entries were dropped because the peer named a
// destination outside this volume's accepted target set. Every fetched
// entry lands in exactly one of the applied / rewind / dropped buckets
// (a merged freshness coordinate counts as applied).
type DurabilityPullReport struct {
	Volume  string
	Peer    string
	Fetched int
	Applied int
	Dropped int
	Rewinds []DurabilityRewind
	// Drops samples the dropped entries up to maxDurabilityDropSamples;
	// Dropped is the exact total.
	Drops []DurabilityDrop
}

// recordDrop counts a dropped entry and samples it into Drops up to the
// cap, so the exact total survives while the detail stays bounded.
func (r *DurabilityPullReport) recordDrop(d DurabilityDrop) {
	r.Dropped++
	if len(r.Drops) < maxDurabilityDropSamples {
		r.Drops = append(r.Drops, d)
	}
}

// DurabilityDrop is one pulled entry the merge discarded because its
// destination falls outside the volume's accepted target set
// (offload_requires ∪ sync_to). Drops are counted and sampled so a peer
// asserting evidence for destinations this node uses for neither offload
// nor sync stays observable.
type DurabilityDrop struct {
	Destination string
	OriginNode  string
	Kind        string // "component" or "freshness"
}

func (d DurabilityDrop) String() string {
	return fmt.Sprintf("%s for unconfigured destination %s origin %s",
		d.Kind, d.Destination, d.OriginNode)
}

// DurabilityRewind is one component the pull refused because the peer
// reported a value below the locally recorded one. The local value
// stays; re-running with the allow-rewind opt-in accepts the peer's.
type DurabilityRewind struct {
	Destination string
	OriginNode  string
	Current     int64
	Attempted   int64
}

func (r DurabilityRewind) String() string {
	return fmt.Sprintf("destination %s origin %s: recorded %d, peer reports %d",
		r.Destination, r.OriginNode, r.Current, r.Attempted)
}

// PullDurability fetches the peer's destination durability vectors for
// the volume and merges them into the local destination_run_ids under
// the same destination names — peers and buckets share one flat target
// namespace, so a component about a destination only the peer can see
// lands as locally cached evidence for offline decisions. The merge is
// metadata-only and monotonic: each component routes through the
// watermark store, refused rewinds are reported on the result (not
// applied), and allowRewind is the explicit recovery override.
//
// The merge is scoped to destinations this volume actually references
// (offload_requires ∪ sync_to); evidence for any other destination is
// dropped, so a buggy or compromised peer cannot pollute the local
// vector with rows for destinations this node neither requires for
// offload nor syncs to.
//
// The standalone `peer-sync pull-durability` command and the automatic
// post-close pull share this implementation.
func PullDurability(ctx context.Context, s *store.Store, vol *config.Volume, node *config.Node, allowRewind bool) (DurabilityPullReport, error) {
	v, err := s.GetVolumeByName(ctx, vol.Name)
	if err != nil {
		if store.IsNotFound(err) {
			return DurabilityPullReport{}, fmt.Errorf("volume %q has no local index row; index it before pulling durability", vol.Name)
		}
		return DurabilityPullReport{}, fmt.Errorf("lookup volume %q: %w", vol.Name, err)
	}
	return pullDurability(ctx, s, newNodeClient(node), vol.Name, v.ID, node.Name, acceptedDestinations(vol), allowRewind)
}

// acceptedDestinations is the set of destination names this volume
// references: the union of its offload_requires and sync_to entries.
// A pulled durability entry for any name outside this set has no bearing
// on the volume's local decisions and is dropped by the pull.
func acceptedDestinations(vol *config.Volume) map[string]struct{} {
	accepted := make(map[string]struct{}, len(vol.OffloadRequires)+len(vol.SyncTo))
	for _, name := range vol.OffloadRequires {
		accepted[name] = struct{}{}
	}
	for _, name := range vol.SyncTo {
		accepted[name] = struct{}{}
	}
	return accepted
}

// pullDurability is the transport-injected body of PullDurability,
// shared with the node-sync driver (which already holds a client).
// accepted scopes which destinations the merge will store (see
// acceptedDestinations).
func pullDurability(ctx context.Context, s *store.Store, client *nodeClient, volumeName string, volumeID int64, peerName string, accepted map[string]struct{}, allowRewind bool) (DurabilityPullReport, error) {
	rep := DurabilityPullReport{Volume: volumeName, Peer: peerName}
	resp, err := client.durability(ctx, syncproto.DurabilityRequest{Volume: volumeName})
	if err != nil {
		return rep, err
	}
	sourceNode, err := s.GetOrCreateOriginNode(ctx, peerName)
	if err != nil {
		return rep, fmt.Errorf("resolve source peer %q: %w", peerName, err)
	}
	rep.Fetched = len(resp.Components) + len(resp.Freshness)
	originIDs := make(map[string]int64, 4)
	resolveOrigin := func(name string) (int64, error) {
		if id, ok := originIDs[name]; ok {
			return id, nil
		}
		// GetOrCreateOriginNode creates a local nodes row for any name
		// not seen before. Bound how many distinct origins one pull will
		// resolve so a peer bug (or hostile peer) flooding novel names
		// cannot grow the local nodes table without limit — a real
		// volume references only a handful of origins. Fails the pull
		// rather than truncating, so the cap is observable.
		if len(originIDs) >= maxOriginNodesPerPull {
			return 0, fmt.Errorf("durability pull names more than %d distinct origin nodes; refusing to create unbounded node rows from one pull", maxOriginNodesPerPull)
		}
		node, err := s.GetOrCreateOriginNode(ctx, name)
		if err != nil {
			return 0, fmt.Errorf("resolve origin node %q: %w", name, err)
		}
		originIDs[name] = node.ID
		return node.ID, nil
	}
	for _, c := range resp.Components {
		if _, ok := accepted[c.Destination]; !ok {
			rep.recordDrop(DurabilityDrop{Destination: c.Destination, OriginNode: c.OriginNode, Kind: "component"})
			continue
		}
		if err := validateComponent(c); err != nil {
			return rep, fmt.Errorf("component %+v: %w", c, err)
		}
		nodeID, err := resolveOrigin(c.OriginNode)
		if err != nil {
			return rep, err
		}
		err = s.UpsertDestinationRunIDPulled(ctx, volumeID, c.Destination, nodeID, c.OriginRun, c.VerifyMethod, sourceNode.ID, allowRewind)
		var rewind *store.DestinationRewindError
		if errors.As(err, &rewind) {
			rep.Rewinds = append(rep.Rewinds, DurabilityRewind{
				Destination: c.Destination,
				OriginNode:  c.OriginNode,
				Current:     rewind.Current,
				Attempted:   rewind.Attempted,
			})
			continue
		}
		if err != nil {
			return rep, fmt.Errorf("apply component for destination %q origin %q: %w", c.Destination, c.OriginNode, err)
		}
		rep.Applied++
	}
	for _, f := range resp.Freshness {
		if _, ok := accepted[f.Destination]; !ok {
			rep.recordDrop(DurabilityDrop{Destination: f.Destination, OriginNode: f.OriginNode, Kind: "freshness"})
			continue
		}
		if err := validateFreshness(f); err != nil {
			return rep, fmt.Errorf("freshness %+v: %w", f, err)
		}
		nodeID, err := resolveOrigin(f.OriginNode)
		if err != nil {
			return rep, err
		}
		if err := s.MergeDestinationPushFreshness(ctx, volumeID, f.Destination, nodeID, f.OriginRun); err != nil {
			return rep, fmt.Errorf("apply freshness for destination %q origin %q: %w", f.Destination, f.OriginNode, err)
		}
		rep.Applied++
	}
	return rep, nil
}

// validateComponent guards the wire-supplied component before it
// touches the local vector: destination and origin names are
// identities, the run id must be a positive origin-space id, and the
// verify method must be empty or a method this build recognises. The
// method check is defence-in-depth, not a trust boundary (the peer is
// trusted to assert its own durability — see SAFETY-AUDIT.md D1): the
// gate already refuses to offload on an unrecognised method, so the
// only effect of an unknown non-empty method reaching the store is a
// silently-inert row. Refusing it here turns a peer bug or a
// version-skew method string into a loud error at the pull instead.
func validateComponent(c syncproto.DurabilityComponent) error {
	if c.Destination == "" {
		return errors.New("destination must be non-empty")
	}
	if !store.ValidNodeName(c.OriginNode) {
		return fmt.Errorf("origin_node %q is not a valid node name", c.OriginNode)
	}
	if c.OriginRun <= 0 {
		return fmt.Errorf("origin_run %d must be positive", c.OriginRun)
	}
	if c.VerifyMethod != "" && !store.KnownVerifyMethod(c.VerifyMethod) {
		return fmt.Errorf("verify_method %q is not a recognised verification method", c.VerifyMethod)
	}
	return nil
}

// validateFreshness guards a wire-supplied freshness coordinate before it
// merges into the local table: same identity and positive-run-id rules as
// validateComponent.
func validateFreshness(f syncproto.DurabilityFreshness) error {
	if f.Destination == "" {
		return errors.New("destination must be non-empty")
	}
	if !store.ValidNodeName(f.OriginNode) {
		return fmt.Errorf("origin_node %q is not a valid node name", f.OriginNode)
	}
	if f.OriginRun <= 0 {
		return fmt.Errorf("origin_run %d must be positive", f.OriginRun)
	}
	return nil
}

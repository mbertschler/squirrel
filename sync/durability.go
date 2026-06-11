package sync

import (
	"context"
	"errors"
	"fmt"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/syncproto"
)

// DurabilityPullReport summarises one durability metadata pull from a
// peer: how many vector components were fetched, how many landed in
// the local destination_run_ids (advanced or re-confirmed), and which
// were refused as rewinds. Every fetched component lands in exactly
// one of the two buckets.
type DurabilityPullReport struct {
	Volume  string
	Peer    string
	Fetched int
	Applied int
	Rewinds []DurabilityRewind
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
	return pullDurability(ctx, s, newNodeClient(node), vol.Name, v.ID, node.Name, allowRewind)
}

// pullDurability is the transport-injected body of PullDurability,
// shared with the node-sync driver (which already holds a client).
func pullDurability(ctx context.Context, s *store.Store, client *nodeClient, volumeName string, volumeID int64, peerName string, allowRewind bool) (DurabilityPullReport, error) {
	rep := DurabilityPullReport{Volume: volumeName, Peer: peerName}
	resp, err := client.durability(ctx, syncproto.DurabilityRequest{Volume: volumeName})
	if err != nil {
		return rep, err
	}
	rep.Fetched = len(resp.Components)
	originIDs := make(map[string]int64, 4)
	for _, c := range resp.Components {
		if err := validateComponent(c); err != nil {
			return rep, fmt.Errorf("component %+v: %w", c, err)
		}
		nodeID, ok := originIDs[c.OriginNode]
		if !ok {
			node, err := s.GetOrCreateOriginNode(ctx, c.OriginNode)
			if err != nil {
				return rep, fmt.Errorf("resolve origin node %q: %w", c.OriginNode, err)
			}
			nodeID = node.ID
			originIDs[c.OriginNode] = nodeID
		}
		err := s.UpsertDestinationRunID(ctx, volumeID, c.Destination, nodeID, c.OriginRun, allowRewind)
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
	return rep, nil
}

// validateComponent guards the wire-supplied component before it
// touches the local vector: destination and origin names are
// identities, the run id must be a positive origin-space id.
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
	return nil
}

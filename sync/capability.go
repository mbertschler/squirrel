package sync

import (
	"context"
	"fmt"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/syncproto"
)

// RelayedCapability is one peer-relayed target's offload-gating capability
// as advertised by the peer that owns it. It is the wire
// syncproto.DestinationCapability tagged with the peer it came from, so an
// offload pre-check can fail fast on a relayed required target that can
// never gate while naming the owning peer (#145).
type RelayedCapability struct {
	// Target is the destination name (an offload_requires entry).
	Target string
	// Peer is the node name the capability was advertised by.
	Peer string
	// CanGate is that peer's CanEverGateOffload verdict for Target.
	CanGate bool
	// Reason names the structural gap when CanGate is false.
	Reason string
}

// GatherRelayedCapabilities probes the given peer nodes for the advertised
// offload-gating capability of the named targets, for the offload
// pre-check's peer-relayed half (#145). For each reachable peer it reads the
// durability endpoint's Capabilities list and keeps every entry whose
// destination is in targets, tagged with that peer's name.
//
// The probe is strictly best-effort and never fails the caller: a peer that
// is unreachable or errors contributes a one-line advisory to the returned
// slice (so the caller can tell the operator the pre-check was skipped for
// it) and no capability, and a peer that predates the capability field
// simply advertises none. A target no peer advertises is therefore absent
// from the result, leaving it to the per-file gate — missing capability
// information never becomes an abort.
func GatherRelayedCapabilities(ctx context.Context, volumeName string, nodes []*config.Node, targets map[string]struct{}) ([]RelayedCapability, []string) {
	var caps []RelayedCapability
	var softErrs []string
	for _, node := range nodes {
		resp, err := newNodeClient(node).durability(ctx, syncproto.DurabilityRequest{Volume: volumeName})
		if err != nil {
			softErrs = append(softErrs, fmt.Sprintf("peer %q: %v", node.Name, err))
			continue
		}
		for _, c := range resp.Capabilities {
			if _, ok := targets[c.Destination]; !ok {
				continue
			}
			caps = append(caps, RelayedCapability{
				Target:  c.Destination,
				Peer:    node.Name,
				CanGate: c.CanGate,
				Reason:  c.Reason,
			})
		}
	}
	return caps, softErrs
}

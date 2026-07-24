package sync

import (
	"context"
	"fmt"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/syncproto"
)

// peerProbeTimeout bounds a single peer's capability probe. Unlike the
// long-running sync exchanges (which stream data and rely on caller
// cancellation), this is a quick preflight metadata read: the shared
// nodeClient sets no request timeout, so a peer that is reachable but
// unresponsive — it completes the TCP/TLS handshake yet never returns a
// response header — would otherwise hang the offload preflight forever.
// Bounding it keeps the probe strictly best-effort: a timed-out peer
// becomes a soft advisory and the target falls back to the per-file gate,
// never a hard block. It is a var (not a const) only so tests can shorten
// it. Mirrors the TUI agent client, which likewise bounds agent queries.
var peerProbeTimeout = 15 * time.Second

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
		// Each peer gets its own bounded budget so one unresponsive peer
		// cannot starve the others or hang the whole preflight. The full
		// response is decoded before durability returns, so cancelling
		// immediately after is safe.
		probeCtx, cancel := context.WithTimeout(ctx, peerProbeTimeout)
		resp, err := newNodeClient(node).durability(probeCtx, syncproto.DurabilityRequest{Volume: volumeName})
		cancel()
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

package agent

import (
	"context"
	"fmt"
	"net/http"

	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/syncproto"
)

// handleDurability implements POST /v1/sync/durability: a session-less,
// read-only listing of this node's recorded destination durability
// vectors for one volume. Peers pull it (after a sync, or standalone)
// to hold offline evidence about destinations only this node can see.
// Node identity travels as names — local node ids mean nothing to the
// caller.
func (r *peerSyncRouter) handleDurability(w http.ResponseWriter, req *http.Request) {
	var body syncproto.DurabilityRequest
	if err := decodeJSON(w, req, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Volume == "" {
		writeError(w, http.StatusBadRequest, "volume is required")
		return
	}
	if _, ok := r.srv.live.Get().Volumes[body.Volume]; !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("volume %q is not declared on this node", body.Volume))
		return
	}
	resp, err := r.durabilityResponse(req.Context(), body.Volume)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// durabilityResponse assembles the wire components for one volume. A
// declared volume with no store row (never indexed or synced) yields an
// empty component list rather than an error — "no recorded durability"
// is a valid answer.
func (r *peerSyncRouter) durabilityResponse(ctx context.Context, volumeName string) (syncproto.DurabilityResponse, error) {
	// Capability is a structural property of the config, independent of any
	// recorded evidence — so it is advertised even for a volume with no
	// store row yet (never indexed or synced on this node). That is exactly
	// the crypt-mirror case a relayed pre-check must catch: incapable, and
	// with no components it would otherwise never have anything to report.
	caps := r.destinationCapabilities(volumeName)
	v, err := r.srv.store.GetVolumeByName(ctx, volumeName)
	if store.IsNotFound(err) {
		return syncproto.DurabilityResponse{Capabilities: caps}, nil
	}
	if err != nil {
		return syncproto.DurabilityResponse{}, fmt.Errorf("lookup volume: %w", err)
	}
	rows, err := r.srv.store.ListVolumeDestinationRunIDs(ctx, v.ID)
	if err != nil {
		return syncproto.DurabilityResponse{}, fmt.Errorf("list destination vectors: %w", err)
	}
	fresh, err := r.srv.store.ListVolumeDestinationPushFreshness(ctx, v.ID)
	if err != nil {
		return syncproto.DurabilityResponse{}, fmt.Errorf("list push freshness: %w", err)
	}
	// The effective verify cadence per destination, relayed so a puller can
	// apply the fingerprint-verified cadence coupling against this
	// responder's own re-confirmation schedule (see DurabilityComponent).
	live := r.srv.live.Get()
	cadences := resolveVerifyCadences(live.Destinations, live.AgentVerifyEvery())
	names := make(map[int64]string, 4)
	resolve := func(nodeID int64) (string, error) {
		if name, ok := names[nodeID]; ok {
			return name, nil
		}
		node, err := r.srv.store.GetNodeByID(ctx, nodeID)
		if err != nil {
			return "", fmt.Errorf("resolve origin node %d: %w", nodeID, err)
		}
		names[nodeID] = node.Name
		return node.Name, nil
	}
	resp := syncproto.DurabilityResponse{
		Components:   make([]syncproto.DurabilityComponent, 0, len(rows)),
		Freshness:    make([]syncproto.DurabilityFreshness, 0, len(fresh)),
		Capabilities: caps,
	}
	for _, row := range rows {
		name, err := resolve(row.OriginNodeID)
		if err != nil {
			return syncproto.DurabilityResponse{}, err
		}
		resp.Components = append(resp.Components, syncproto.DurabilityComponent{
			Destination:   row.Destination,
			OriginNode:    name,
			OriginRun:     row.OriginRunID,
			UpdatedAtNs:   row.UpdatedAtNs,
			VerifyMethod:  row.VerifyMethod,
			VerifiedAtNs:  row.VerifiedAtNs.Int64, // zero when NULL (unknown)
			VerifyEveryNs: int64(cadences[row.Destination]),
		})
	}
	for _, row := range fresh {
		name, err := resolve(row.OriginNodeID)
		if err != nil {
			return syncproto.DurabilityResponse{}, err
		}
		resp.Freshness = append(resp.Freshness, syncproto.DurabilityFreshness{
			Destination: row.Destination,
			OriginNode:  name,
			OriginRun:   row.OriginRunID,
			UpdatedAtNs: row.UpdatedAtNs,
		})
	}
	return resp, nil
}

// destinationCapabilities advertises, for each destination this node
// syncs the volume to, whether it can ever gate offload — the raw
// config.Destination.CanEverGateOffload verdict. A peer gating on a
// relayed required target reads this to fail fast when the owning
// destination is structurally incapable (#145). The set is scoped to the
// volume's sync_to destinations (the ones this node's content for the
// volume actually lands on), so a puller only ever hears about targets
// relevant to that volume; node targets in sync_to and names with no
// local destination resolve to nothing and are skipped. Returns nil when
// the volume is unknown or names no local destinations — a puller then
// falls back to the per-file gate.
func (r *peerSyncRouter) destinationCapabilities(volumeName string) []syncproto.DestinationCapability {
	live := r.srv.live.Get()
	vol, ok := live.Volumes[volumeName]
	if !ok {
		return nil
	}
	var caps []syncproto.DestinationCapability
	seen := make(map[string]struct{}, len(vol.SyncTo))
	for _, name := range vol.SyncTo {
		if _, dup := seen[name]; dup {
			continue
		}
		d, ok := live.Destinations[name]
		if !ok {
			continue
		}
		seen[name] = struct{}{}
		canGate, reason := d.CanEverGateOffload()
		caps = append(caps, syncproto.DestinationCapability{
			Destination: name,
			CanGate:     canGate,
			Reason:      reason,
		})
	}
	return caps
}

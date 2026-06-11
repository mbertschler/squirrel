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
	if err := decodeJSON(req, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Volume == "" {
		writeError(w, http.StatusBadRequest, "volume is required")
		return
	}
	if _, ok := r.volumes[body.Volume]; !ok {
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
	v, err := r.srv.store.GetVolumeByName(ctx, volumeName)
	if store.IsNotFound(err) {
		return syncproto.DurabilityResponse{}, nil
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
		Components: make([]syncproto.DurabilityComponent, 0, len(rows)),
		Freshness:  make([]syncproto.DurabilityFreshness, 0, len(fresh)),
	}
	for _, row := range rows {
		name, err := resolve(row.OriginNodeID)
		if err != nil {
			return syncproto.DurabilityResponse{}, err
		}
		resp.Components = append(resp.Components, syncproto.DurabilityComponent{
			Destination:  row.Destination,
			OriginNode:   name,
			OriginRun:    row.OriginRunID,
			UpdatedAtNs:  row.UpdatedAtNs,
			VerifyMethod: row.VerifyMethod,
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

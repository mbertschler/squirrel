package store

import (
	"context"
	"database/sql"
	"fmt"
)

// PeerSyncState is one row in the peer_sync_state table — the
// per-(volume, peer) watermark used by plan negotiation to
// distinguish *supersede* (the receiver's prior row at a path was
// last written by this peer at or before the watermark) from
// *conflict* (it wasn't). LastSharedRunID carries the initiator's
// local run id, not the receiver's — see the schema comment for
// why this is deliberately not FK-bound to local runs.
type PeerSyncState struct {
	VolumeID        int64
	PeerNodeID      int64
	LastSharedRunID sql.NullInt64
	LastSyncedAtNs  int64
}

// GetPeerSyncState returns the watermark for one (volume, peer)
// pair, or sql.ErrNoRows on first contact. The zero LastSharedRunID
// (NULL) signals "no prior sync"; the planner treats that the same
// as "watermark is zero", i.e. any peer-sourced row passes the
// "≤ watermark" disposition check.
func (s *Store) GetPeerSyncState(ctx context.Context, volumeID, peerNodeID int64) (PeerSyncState, error) {
	var p PeerSyncState
	err := s.db.QueryRowContext(ctx,
		`SELECT volume_id, peer_node_id, last_shared_run_id, last_synced_at
		 FROM peer_sync_state WHERE volume_id = ? AND peer_node_id = ?`,
		volumeID, peerNodeID).
		Scan(&p.VolumeID, &p.PeerNodeID, &p.LastSharedRunID, &p.LastSyncedAtNs)
	return p, err
}

// UpsertPeerSyncState advances the watermark for one (volume, peer)
// pair to the supplied initiator run-id, stamped with the current
// time. Called only at a successful sync close — failed or partial
// runs leave the prior watermark in place so the next run replans
// against the last point of agreement.
func (s *Store) UpsertPeerSyncState(ctx context.Context, volumeID, peerNodeID, lastSharedRunID int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO peer_sync_state (volume_id, peer_node_id, last_shared_run_id, last_synced_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(volume_id, peer_node_id) DO UPDATE SET
			last_shared_run_id = excluded.last_shared_run_id,
			last_synced_at     = excluded.last_synced_at
	`, volumeID, peerNodeID, lastSharedRunID, NowNs())
	if err != nil {
		return fmt.Errorf("upsert peer_sync_state: %w", err)
	}
	return nil
}

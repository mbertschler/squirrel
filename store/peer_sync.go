package store

import (
	"context"
	"database/sql"
	"errors"
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

// ErrWatermarkRewind is the shared sentinel for a refused backwards
// watermark move, returned (wrapped) by UpsertPeerSyncState and
// UpsertDestinationRunID when the supplied value is below the one
// already recorded and allowRewind was not set. Watermarks are meant to
// advance monotonically — a backwards move usually signals a misordered
// close or a hostile peer claiming a run id we never agreed to, and
// silently accepting it would re-anchor drift detection against the bad
// value (SAFETY-AUDIT H6). Genuine recovery passes allowRewind to
// override. Matchable via errors.Is.
var ErrWatermarkRewind = errors.New("watermark would move backwards")

// WatermarkRewindError carries the rejected and current watermarks
// alongside ErrWatermarkRewind so a caller (or CLI) can report exactly
// what move was refused without re-reading the row. It wraps the
// sentinel so errors.Is(err, ErrWatermarkRewind) matches.
type WatermarkRewindError struct {
	VolumeID   int64
	PeerNodeID int64
	Current    int64
	Attempted  int64
}

func (e *WatermarkRewindError) Error() string {
	return fmt.Sprintf(
		"peer-sync watermark for volume %d peer %d would move backwards from %d to %d; pass allowRewind to override",
		e.VolumeID, e.PeerNodeID, e.Current, e.Attempted)
}

func (e *WatermarkRewindError) Unwrap() error { return ErrWatermarkRewind }

// PeerSyncStateHistory is one row of the insert-only
// peer_sync_state_history log: a single watermark advance recorded for a
// (volume, peer) pair. LastSharedRunID is nullable for the same reason
// it is on PeerSyncState (first contact carries no shared run id). AtNs
// is the insertion timestamp, distinct from LastSyncedAtNs so rows
// written in the same tick still order by id.
type PeerSyncStateHistory struct {
	ID              int64
	VolumeID        int64
	PeerNodeID      int64
	LastSharedRunID sql.NullInt64
	LastSyncedAtNs  int64
	AtNs            int64
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

// VolumePeerSync is one peer this node has exchanged the volume with,
// with the peer's name resolved. It is PeerSyncState plus the identity the
// fleet view names the row by, and exists so a surface can enumerate the
// peers a volume has actually met without joining the nodes table itself.
//
// The listing matters because the receiver of a peer sync records this row
// and nothing else: on a hub, an edge machine that pushes to it appears in
// no config target list and in no durability vector, so this is the only
// evidence the volume also lives there.
type VolumePeerSync struct {
	PeerNodeID      int64
	PeerName        string
	LastSharedRunID sql.NullInt64
	LastSyncedAtNs  int64
}

// ListVolumePeerSyncStates returns every peer with recorded exchange state
// for the volume, ordered by peer name. An empty slice means the volume has
// never synced with a peer in either direction.
func (s *Store) ListVolumePeerSyncStates(ctx context.Context, volumeID int64) ([]VolumePeerSync, error) {
	return queryRows(ctx, s.db, `
		SELECT p.peer_node_id, n.name, p.last_shared_run_id, p.last_synced_at
		FROM peer_sync_state p
		JOIN nodes n ON n.id = p.peer_node_id
		WHERE p.volume_id = ?
		ORDER BY n.name
	`, scanVolumePeerSync, volumeID)
}

func scanVolumePeerSync(s rowScanner) (VolumePeerSync, error) {
	var p VolumePeerSync
	err := s.Scan(&p.PeerNodeID, &p.PeerName, &p.LastSharedRunID, &p.LastSyncedAtNs)
	return p, err
}

// UpsertPeerSyncState advances the watermark for one (volume, peer)
// pair to the supplied initiator run-id, stamped with the current
// time. Called only at a successful sync close — failed or partial
// runs leave the prior watermark in place so the next run replans
// against the last point of agreement.
//
// The watermark is meant to advance monotonically. When the supplied
// lastSharedRunID is below the value already recorded, the upsert is
// refused with a *WatermarkRewindError (wrapping ErrWatermarkRewind)
// unless allowRewind is set — the opt-in for genuine recovery. Existing
// callers (the receiver's successful-close path) pass false.
//
// The upsert and an insert-only peer_sync_state_history row are written
// in one transaction so the append-only watermark log can never diverge
// from the live row (SAFETY-AUDIT H6).
func (s *Store) UpsertPeerSyncState(ctx context.Context, volumeID, peerNodeID, lastSharedRunID int64, allowRewind bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin upsert peer_sync_state: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if !allowRewind {
		if err := guardWatermarkMonotonicTx(ctx, tx, volumeID, peerNodeID, lastSharedRunID); err != nil {
			return err
		}
	}

	atNs := NowNs()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO peer_sync_state (volume_id, peer_node_id, last_shared_run_id, last_synced_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(volume_id, peer_node_id) DO UPDATE SET
			last_shared_run_id = excluded.last_shared_run_id,
			last_synced_at     = excluded.last_synced_at
	`, volumeID, peerNodeID, lastSharedRunID, atNs); err != nil {
		return fmt.Errorf("upsert peer_sync_state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO peer_sync_state_history
			(volume_id, peer_node_id, last_shared_run_id, last_synced_at, at_ns)
		VALUES (?, ?, ?, ?, ?)
	`, volumeID, peerNodeID, lastSharedRunID, atNs, atNs); err != nil {
		return fmt.Errorf("append peer_sync_state_history: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit upsert peer_sync_state: %w", err)
	}
	return nil
}

// guardWatermarkMonotonicTx reads the current watermark inside tx and
// returns a *WatermarkRewindError when attempted is strictly below it. A
// NULL current watermark (first contact) or no row at all imposes no
// floor — any value advances from "no prior sync".
func guardWatermarkMonotonicTx(ctx context.Context, tx *sql.Tx, volumeID, peerNodeID, attempted int64) error {
	var current sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT last_shared_run_id FROM peer_sync_state
		 WHERE volume_id = ? AND peer_node_id = ?`,
		volumeID, peerNodeID).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("read current watermark: %w", err)
	}
	if current.Valid && attempted < current.Int64 {
		return &WatermarkRewindError{
			VolumeID:   volumeID,
			PeerNodeID: peerNodeID,
			Current:    current.Int64,
			Attempted:  attempted,
		}
	}
	return nil
}

// ListPeerSyncStateHistory returns every peer_sync_state_history row for
// the (volume, peer) pair, oldest first (ascending id, which is
// insertion order). Provided so the `squirrel peer-sync history` CLI can
// read the watermark transition log without reaching into the table
// directly; an empty slice means no recorded advances.
func (s *Store) ListPeerSyncStateHistory(ctx context.Context, volumeID, peerNodeID int64) ([]PeerSyncStateHistory, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, volume_id, peer_node_id, last_shared_run_id, last_synced_at, at_ns
		FROM peer_sync_state_history
		WHERE volume_id = ? AND peer_node_id = ?
		ORDER BY id
	`, volumeID, peerNodeID)
	if err != nil {
		return nil, fmt.Errorf("list peer_sync_state_history: %w", err)
	}
	defer rows.Close()
	var out []PeerSyncStateHistory
	for rows.Next() {
		var h PeerSyncStateHistory
		if err := rows.Scan(&h.ID, &h.VolumeID, &h.PeerNodeID,
			&h.LastSharedRunID, &h.LastSyncedAtNs, &h.AtNs); err != nil {
			return nil, fmt.Errorf("scan peer_sync_state_history row: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DestinationRunID is one component of a destination's durability
// version vector: the highest origin-space run id of OriginNodeID's
// content known durable on Destination for VolumeID. Destination is the
// unified target name — a bucket destination or a peer node name, the
// same namespace runs.destination uses. OriginRunID is in the origin
// node's run space, so like contents.origin_run_id it is not a local
// runs FK. Content with origin (N, r) is durable on a destination iff
// the vector's component for N is ≥ r.
type DestinationRunID struct {
	VolumeID     int64
	Destination  string
	OriginNodeID int64
	OriginRunID  int64
	UpdatedAtNs  int64
}

// DestinationRunIDHistory is one row of the insert-only
// destination_run_ids_history log: a single vector-component advance.
// AtNs is the insertion timestamp; rows written in the same tick still
// order by id.
type DestinationRunIDHistory struct {
	ID           int64
	VolumeID     int64
	Destination  string
	OriginNodeID int64
	OriginRunID  int64
	AtNs         int64
}

// DestinationRewindError carries the rejected and current vector
// components when UpsertDestinationRunID refuses a backwards move. It
// wraps ErrWatermarkRewind so errors.Is matches the shared sentinel.
type DestinationRewindError struct {
	VolumeID     int64
	Destination  string
	OriginNodeID int64
	Current      int64
	Attempted    int64
}

func (e *DestinationRewindError) Error() string {
	return fmt.Sprintf(
		"destination %q watermark for volume %d origin node %d would move backwards from %d to %d; pass allowRewind to override",
		e.Destination, e.VolumeID, e.OriginNodeID, e.Current, e.Attempted)
}

func (e *DestinationRewindError) Unwrap() error { return ErrWatermarkRewind }

// GetDestinationRunID returns one vector component, or sql.ErrNoRows
// when the destination has never recorded durability for content
// originating at originNodeID. "No row" imposes no floor: any origin
// run id advances from it.
func (s *Store) GetDestinationRunID(ctx context.Context, volumeID int64, destination string, originNodeID int64) (DestinationRunID, error) {
	var d DestinationRunID
	err := s.db.QueryRowContext(ctx,
		`SELECT volume_id, destination, origin_node_id, origin_run_id, updated_at_ns
		 FROM destination_run_ids
		 WHERE volume_id = ? AND destination = ? AND origin_node_id = ?`,
		volumeID, destination, originNodeID).
		Scan(&d.VolumeID, &d.Destination, &d.OriginNodeID, &d.OriginRunID, &d.UpdatedAtNs)
	return d, err
}

// ListDestinationRunIDs returns the full durability vector for one
// (volume, destination), ordered by origin node id. An empty slice
// means the destination has no recorded durability yet.
func (s *Store) ListDestinationRunIDs(ctx context.Context, volumeID int64, destination string) ([]DestinationRunID, error) {
	return queryRows(ctx, s.db,
		`SELECT volume_id, destination, origin_node_id, origin_run_id, updated_at_ns
		 FROM destination_run_ids
		 WHERE volume_id = ? AND destination = ?
		 ORDER BY origin_node_id`,
		scanDestinationRunID, volumeID, destination)
}

func scanDestinationRunID(s rowScanner) (DestinationRunID, error) {
	var d DestinationRunID
	err := s.Scan(&d.VolumeID, &d.Destination, &d.OriginNodeID, &d.OriginRunID, &d.UpdatedAtNs)
	return d, err
}

// UpsertDestinationRunID advances one component of a destination's
// durability vector to originRunID. Callers invoke it only once the
// destination has verifiably landed every piece of content up to that
// origin run — a failed or partial push leaves the prior value in place.
//
// The component is meant to advance monotonically: the upsert statement
// itself only applies when originRunID is at or above the recorded value
// (or allowRewind is set — the opt-in for genuine recovery, mirroring
// UpsertPeerSyncState), so a racing writer cannot regress the vector. A
// refused rewind surfaces as a *DestinationRewindError (wrapping
// ErrWatermarkRewind).
//
// The upsert and an insert-only destination_run_ids_history row are
// written in one transaction so the append-only advance log can never
// diverge from the live vector.
func (s *Store) UpsertDestinationRunID(ctx context.Context, volumeID int64, destination string, originNodeID, originRunID int64, allowRewind bool) error {
	if destination == "" {
		return fmt.Errorf("UpsertDestinationRunID: destination must be non-empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin upsert destination_run_ids: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	atNs := NowNs()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO destination_run_ids (volume_id, destination, origin_node_id, origin_run_id, updated_at_ns)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(volume_id, destination, origin_node_id) DO UPDATE SET
			origin_run_id = excluded.origin_run_id,
			updated_at_ns = excluded.updated_at_ns
		WHERE excluded.origin_run_id >= destination_run_ids.origin_run_id OR ?
	`, volumeID, destination, originNodeID, originRunID, atNs, allowRewind)
	if err != nil {
		return fmt.Errorf("upsert destination_run_ids: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("upsert destination_run_ids rows: %w", err)
	}
	if n == 0 {
		if err := guardDestinationMonotonicTx(ctx, tx, volumeID, destination, originNodeID, originRunID); err != nil {
			return err
		}
		return fmt.Errorf("upsert destination_run_ids: conditional update applied no row for (%d, %q, %d)", volumeID, destination, originNodeID)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO destination_run_ids_history
			(volume_id, destination, origin_node_id, origin_run_id, at_ns)
		VALUES (?, ?, ?, ?, ?)
	`, volumeID, destination, originNodeID, originRunID, atNs); err != nil {
		return fmt.Errorf("append destination_run_ids_history: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit upsert destination_run_ids: %w", err)
	}
	return nil
}

// guardDestinationMonotonicTx reads the current vector component inside
// tx and returns a *DestinationRewindError when attempted is strictly
// below it. No row imposes no floor. Called after the conditional
// upsert applied nothing, to turn the refusal into a precise error.
func guardDestinationMonotonicTx(ctx context.Context, tx *sql.Tx, volumeID int64, destination string, originNodeID, attempted int64) error {
	var current int64
	err := tx.QueryRowContext(ctx,
		`SELECT origin_run_id FROM destination_run_ids
		 WHERE volume_id = ? AND destination = ? AND origin_node_id = ?`,
		volumeID, destination, originNodeID).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("read current destination run id: %w", err)
	}
	if attempted < current {
		return &DestinationRewindError{
			VolumeID:     volumeID,
			Destination:  destination,
			OriginNodeID: originNodeID,
			Current:      current,
			Attempted:    attempted,
		}
	}
	return nil
}

// ListDestinationRunIDHistory returns every advance recorded for the
// (volume, destination) pair across all origin nodes, oldest first
// (ascending id, which is insertion order). An empty slice means no
// recorded advances.
func (s *Store) ListDestinationRunIDHistory(ctx context.Context, volumeID int64, destination string) ([]DestinationRunIDHistory, error) {
	return queryRows(ctx, s.db, `
		SELECT id, volume_id, destination, origin_node_id, origin_run_id, at_ns
		FROM destination_run_ids_history
		WHERE volume_id = ? AND destination = ?
		ORDER BY id
	`, scanDestinationRunIDHistory, volumeID, destination)
}

func scanDestinationRunIDHistory(s rowScanner) (DestinationRunIDHistory, error) {
	var h DestinationRunIDHistory
	err := s.Scan(&h.ID, &h.VolumeID, &h.Destination, &h.OriginNodeID, &h.OriginRunID, &h.AtNs)
	return h, err
}

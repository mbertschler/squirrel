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

// ListVolumeDestinationRunIDs returns every recorded vector component
// for the volume across all destinations, ordered by destination then
// origin node id. The peer durability endpoint serves this listing so
// a peer can hold offline evidence about destinations only this node
// can see.
func (s *Store) ListVolumeDestinationRunIDs(ctx context.Context, volumeID int64) ([]DestinationRunID, error) {
	return queryRows(ctx, s.db,
		`SELECT volume_id, destination, origin_node_id, origin_run_id, updated_at_ns
		 FROM destination_run_ids
		 WHERE volume_id = ?
		 ORDER BY destination, origin_node_id`,
		scanDestinationRunID, volumeID)
}

func scanDestinationRunID(s rowScanner) (DestinationRunID, error) {
	var d DestinationRunID
	err := s.Scan(&d.VolumeID, &d.Destination, &d.OriginNodeID, &d.OriginRunID, &d.UpdatedAtNs)
	return d, err
}

// AdvanceDestinationVector advances the destination's durability vector
// to cover the volume's current present set: one component per origin
// node, valued at the highest origin-space run among that node's
// present content. Locally-introduced content (contents.origin_* NULL)
// counts under this node's self row at its introduction run — the
// content's earliest first_seen_run_id in the volume, the same
// coordinate the peer-sync sender materialises on the wire — so a
// duplicate path observed later never advances the component past the
// coordinates actually in circulation. Rows under the reserved sync
// subtrees are excluded — they never travel to a destination, so they
// must not advance its evidence.
//
// Callers invoke it only once the destination has verifiably landed the
// volume's full present set (a successful whole-volume sync); each
// component routes through UpsertDestinationRunID so the advance is
// monotonic and history-logged. A component already recorded above the
// computed value is left in place: destinations are append-only, so the
// higher recorded floor still holds (componentwise max, like a version-
// vector join). This is the single advancement path for the vector —
// destination handlers and the peer-sync initiator both call it rather
// than writing components directly.
func (s *Store) AdvanceDestinationVector(ctx context.Context, volumeID int64, destination string) error {
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		return fmt.Errorf("advance destination vector: self node: %w", err)
	}
	components, err := s.presentOriginMaxima(ctx, volumeID, self.ID)
	if err != nil {
		return err
	}
	for _, c := range components {
		err := s.UpsertDestinationRunID(ctx, volumeID, destination, c.OriginNodeID, c.OriginRunID, false)
		if errors.Is(err, ErrWatermarkRewind) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// originComponent is one (origin node, max origin run) pair computed
// over a volume's present rows by presentOriginMaxima.
type originComponent struct {
	OriginNodeID int64
	OriginRunID  int64
}

// presentOriginMaxima computes the per-origin-node maximum origin run
// over the volume's present files, deduplicated to one coordinate per
// content. Content whose origin is NULL (or partially NULL — degraded
// the same way the conflict pre-stage treats partial provenance) maps
// to selfNodeID at its introduction run, mirroring
// ContentIntroductionRunID (the introduction MIN spans every
// observation of the content, any status). The reserved sync subtrees
// are excluded from the present set for the reason documented on
// AdvanceDestinationVector.
func (s *Store) presentOriginMaxima(ctx context.Context, volumeID, selfNodeID int64) ([]originComponent, error) {
	return queryRows(ctx, s.db, `
		WITH present_contents AS (
			SELECT DISTINCT f.content_id, c.origin_node_id, c.origin_run_id
			FROM files f
			JOIN folders fo ON fo.id = f.folder_id
			JOIN contents c ON c.id = f.content_id
			WHERE fo.volume_id = ? AND f.status = 'present'
			  AND `+reservedSubtreeFilter+`
		)
		SELECT
			CASE WHEN pc.origin_node_id IS NULL OR pc.origin_run_id IS NULL
			     THEN ? ELSE pc.origin_node_id END AS origin_node,
			MAX(CASE WHEN pc.origin_node_id IS NULL OR pc.origin_run_id IS NULL
			     THEN (SELECT MIN(f2.first_seen_run_id)
			           FROM files f2
			           JOIN folders fo2 ON fo2.id = f2.folder_id
			           WHERE fo2.volume_id = ? AND f2.content_id = pc.content_id)
			     ELSE pc.origin_run_id END) AS origin_run
		FROM present_contents pc
		GROUP BY origin_node
		ORDER BY origin_node
	`, scanOriginComponent, volumeID, selfNodeID, volumeID)
}

func scanOriginComponent(s rowScanner) (originComponent, error) {
	var c originComponent
	err := s.Scan(&c.OriginNodeID, &c.OriginRunID)
	return c, err
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

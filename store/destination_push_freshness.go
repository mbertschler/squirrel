package store

import (
	"context"
	"fmt"
)

// DestinationPushFreshness is one origin-space freshness coordinate: the
// highest origin run of OriginNodeID's content that was present on the
// pushing node when it last completed a successful whole-volume push of
// VolumeID to Destination. Coordinates live in the origin node's run
// space, like contents.origin_run_id and DestinationRunID.OriginRunID, so
// a node that never pushes to Destination directly can still compare a
// gated content's origin run against it.
//
// Unlike the monotonic durability vector (DestinationRunID), this maximum
// is overwritten per push: it tracks the most recent push's coverage, not
// the all-time maximum, so it answers "is this content's origin run
// covered by a *fresh* push" rather than "was it ever durable".
type DestinationPushFreshness struct {
	VolumeID     int64
	Destination  string
	OriginNodeID int64
	OriginRunID  int64
	UpdatedAtNs  int64
}

// UpsertDestinationPushFreshness sets the freshness coordinate for
// (volume, destination, origin node) to originRunID, overwriting any
// prior value. Callers invoke it once per successful whole-volume push
// with that push's present-set origin maxima, so the row always reflects
// the latest push rather than a monotonic floor.
func (s *Store) UpsertDestinationPushFreshness(ctx context.Context, volumeID int64, destination string, originNodeID, originRunID int64) error {
	if destination == "" {
		return fmt.Errorf("UpsertDestinationPushFreshness: destination must be non-empty")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO destination_push_freshness (volume_id, destination, origin_node_id, origin_run_id, updated_at_ns)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(volume_id, destination, origin_node_id) DO UPDATE SET
			origin_run_id = excluded.origin_run_id,
			updated_at_ns = excluded.updated_at_ns
	`, volumeID, destination, originNodeID, originRunID, NowNs())
	if err != nil {
		return fmt.Errorf("upsert destination_push_freshness: %w", err)
	}
	return nil
}

// MergeDestinationPushFreshness raises the freshness coordinate for
// (volume, destination, origin node) to originRunID only when it exceeds
// the recorded value, leaving a higher recorded value in place. The
// durability pull uses it to accumulate freshness evidence about a
// relayed target across pulls: a target is append-only, so once a push
// covered origin run R every coordinate up to R was pushed and persists,
// making the highest coordinate ever observed the soundest cached fact.
// A stale pull reporting a lower value never rewinds the puller — the
// monotonic accumulation mirrors the durability vector's pull merge.
func (s *Store) MergeDestinationPushFreshness(ctx context.Context, volumeID int64, destination string, originNodeID, originRunID int64) error {
	if destination == "" {
		return fmt.Errorf("MergeDestinationPushFreshness: destination must be non-empty")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO destination_push_freshness (volume_id, destination, origin_node_id, origin_run_id, updated_at_ns)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(volume_id, destination, origin_node_id) DO UPDATE SET
			origin_run_id = excluded.origin_run_id,
			updated_at_ns = excluded.updated_at_ns
		WHERE excluded.origin_run_id > destination_push_freshness.origin_run_id
	`, volumeID, destination, originNodeID, originRunID, NowNs())
	if err != nil {
		return fmt.Errorf("merge destination_push_freshness: %w", err)
	}
	return nil
}

// ListDestinationPushFreshness returns every freshness coordinate for one
// (volume, destination), ordered by origin node id. An empty slice means
// the destination has no recorded whole-volume push for the volume yet —
// which the offload gate reads as "no freshness evidence" and refuses a
// relayed target on.
func (s *Store) ListDestinationPushFreshness(ctx context.Context, volumeID int64, destination string) ([]DestinationPushFreshness, error) {
	return queryRows(ctx, s.db,
		`SELECT volume_id, destination, origin_node_id, origin_run_id, updated_at_ns
		 FROM destination_push_freshness
		 WHERE volume_id = ? AND destination = ?
		 ORDER BY origin_node_id`,
		scanDestinationPushFreshness, volumeID, destination)
}

// ListVolumeDestinationPushFreshness returns every freshness coordinate
// for the volume across all destinations, ordered by destination then
// origin node id. The peer durability endpoint serves it so a relayed
// target's freshness evidence travels to a node that never pushes there.
func (s *Store) ListVolumeDestinationPushFreshness(ctx context.Context, volumeID int64) ([]DestinationPushFreshness, error) {
	return queryRows(ctx, s.db,
		`SELECT volume_id, destination, origin_node_id, origin_run_id, updated_at_ns
		 FROM destination_push_freshness
		 WHERE volume_id = ?
		 ORDER BY destination, origin_node_id`,
		scanDestinationPushFreshness, volumeID)
}

func scanDestinationPushFreshness(s rowScanner) (DestinationPushFreshness, error) {
	var f DestinationPushFreshness
	err := s.Scan(&f.VolumeID, &f.Destination, &f.OriginNodeID, &f.OriginRunID, &f.UpdatedAtNs)
	return f, err
}

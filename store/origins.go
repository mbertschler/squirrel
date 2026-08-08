package store

import "context"

// Origin-coordinate queries for the fleet view (#187). A destination's
// durability vector records, per origin node, the highest origin run known
// durable there; this node's own index records the coordinate every content
// carries. Comparing the two answers "how much of what I hold has not
// reached that place" and "does that place hold something I have never
// seen" without any new exchange — the same watermarks the offload gate
// already reads, counted rather than tested.
//
// Both queries mirror PresentOriginMaxima's NULL-origin handling (content
// introduced here carries no origin coordinate, so it counts under
// selfNodeID at its introduction run) and its exclusion of the reserved
// sync subtrees, which never travel to a destination.

// OriginFileCount is one origin coordinate with how many of a volume's
// present files carry it. Files counts path observations, not distinct
// contents: "12 files missing there" is a count of files the operator can
// see in their tree, so two paths sharing one content count twice.
type OriginFileCount struct {
	OriginNodeID int64
	OriginRunID  int64
	Files        int64
}

// PresentFilesByOrigin returns the volume's present files bucketed by
// origin coordinate. A place's durability vector covers a bucket iff its
// component for that origin node is at or above the bucket's run, so
// summing the uncovered buckets yields the file count that has not reached
// that place. The result has one row per distinct (origin node, origin
// run) pair — bounded by the number of runs that introduced content, not
// by the file count.
func (s *Store) PresentFilesByOrigin(ctx context.Context, volumeID, selfNodeID int64) ([]OriginFileCount, error) {
	return queryRows(ctx, s.db, `
		WITH present_files AS (
			SELECT f.content_id, c.origin_node_id, c.origin_run_id
			FROM files f
			JOIN folders fo ON fo.id = f.folder_id
			JOIN contents c ON c.id = f.content_id
			WHERE fo.volume_id = ? AND f.status = 'present'
			  AND `+reservedSubtreeFilter+`
		)
		SELECT
			CASE WHEN pf.origin_node_id IS NULL OR pf.origin_run_id IS NULL
			     THEN ? ELSE pf.origin_node_id END AS origin_node,
			CASE WHEN pf.origin_node_id IS NULL OR pf.origin_run_id IS NULL
			     THEN (SELECT MIN(f2.first_seen_run_id)
			           FROM files f2
			           JOIN folders fo2 ON fo2.id = f2.folder_id
			           WHERE fo2.volume_id = ? AND f2.content_id = pf.content_id)
			     ELSE pf.origin_run_id END AS origin_run,
			COUNT(*) AS files
		FROM present_files pf
		GROUP BY origin_node, origin_run
		ORDER BY origin_node, origin_run
	`, scanOriginFileCount, volumeID, selfNodeID, volumeID)
}

func scanOriginFileCount(s rowScanner) (OriginFileCount, error) {
	var c OriginFileCount
	err := s.Scan(&c.OriginNodeID, &c.OriginRunID, &c.Files)
	return c, err
}

// KnownOriginMaxima returns, per origin node, the highest origin run this
// volume has ever observed here — any file status, not just present. It is
// the floor the "ahead" inference compares a peer's asserted coverage
// against: a peer claiming durability for a run above this node's maximum
// for that origin is holding content this node has never seen.
//
// Deliberately wider than PresentOriginMaxima, which answers the durability
// question over the present set. Content this node received and later
// offloaded, or deleted, has still been *seen*; scoring it as never-seen
// would make every offloading edge machine look permanently behind its
// peers.
func (s *Store) KnownOriginMaxima(ctx context.Context, volumeID, selfNodeID int64) ([]OriginComponent, error) {
	return queryRows(ctx, s.db, `
		WITH known_contents AS (
			SELECT DISTINCT f.content_id, c.origin_node_id, c.origin_run_id
			FROM files f
			JOIN folders fo ON fo.id = f.folder_id
			JOIN contents c ON c.id = f.content_id
			WHERE fo.volume_id = ?
			  AND `+reservedSubtreeFilter+`
		)
		SELECT
			CASE WHEN kc.origin_node_id IS NULL OR kc.origin_run_id IS NULL
			     THEN ? ELSE kc.origin_node_id END AS origin_node,
			MAX(CASE WHEN kc.origin_node_id IS NULL OR kc.origin_run_id IS NULL
			     THEN (SELECT MIN(f2.first_seen_run_id)
			           FROM files f2
			           JOIN folders fo2 ON fo2.id = f2.folder_id
			           WHERE fo2.volume_id = ? AND f2.content_id = kc.content_id)
			     ELSE kc.origin_run_id END) AS origin_run
		FROM known_contents kc
		GROUP BY origin_node
		ORDER BY origin_node
	`, scanOriginComponent, volumeID, selfNodeID, volumeID)
}

package store

import (
	"context"
)

// PathDelta is one path-level state change, as exported into a
// content-addressed destination's manifest segment: the volume-relative
// path, its content coordinates, and the status the change left the row
// in. Status is one of the files statuses — 'present' (the path's
// current content), 'superseded' (an outgoing content observation),
// 'missing' or 'offloaded' (the path's current content with its local
// bytes gone — unexpectedly or intentionally).
type PathDelta struct {
	Path      string
	ContentID int64
	Blake3    []byte // raw 32-byte BLAKE3-256 digest
	SizeBytes int64
	MtimeNs   int64
	Status    string
}

// reservedSubtreeFilter excludes the squirrel-reserved sync subtrees
// from a files read (table alias fo). Content under them never travels
// to a destination, so destination-facing reads — the durability vector
// and the manifest delta — must not see it.
const reservedSubtreeFilter = `fo.path != '.squirrel-history'         AND fo.path NOT LIKE '.squirrel-history/%'
		  AND fo.path != '.squirrel-conflicts'       AND fo.path NOT LIKE '.squirrel-conflicts/%'
		  AND fo.path != '.squirrel-restore-history' AND fo.path NOT LIKE '.squirrel-restore-history/%'
		  AND fo.path != '.squirrel-index'           AND fo.path NOT LIKE '.squirrel-index/%'`

// ListPathDeltaSince returns every row in the volume whose status last
// changed after sinceRunID, ordered by (path, status) so the export is
// deterministic. sinceRunID = 0 reads the volume's full recorded state.
// The reserved sync subtrees are excluded — they never travel to a
// destination. status_changed_run_id is maintained by every status
// writer from v18 on; the COALESCE covers rows a pre-v18 binary wrote
// after the backfill ran (their insert run is the one recorded
// coordinate).
func (s *Store) ListPathDeltaSince(ctx context.Context, volumeID, sinceRunID int64) ([]PathDelta, error) {
	return queryRows(ctx, s.db, `
		SELECT `+pathFromFolderAndName+`, f.content_id, c.blake3, c.size_bytes, f.mtime_ns, f.status
		FROM `+fileFromJoin+`
		WHERE fo.volume_id = ?
		  AND COALESCE(f.status_changed_run_id, f.first_seen_run_id) > ?
		  AND `+reservedSubtreeFilter+`
		ORDER BY `+pathFromFolderAndName+`, f.status
	`, scanPathDelta, volumeID, sinceRunID)
}

// ListPresentContent returns the volume's live 'present' rows — the paths
// whose current content has local bytes and therefore has a copy at a
// destination — as (path, content) references ordered by path. The
// reserved sync subtrees are excluded (they never travel to a
// destination), matching ListPathDeltaSince. Restore of a content-addressed
// or packed destination uses this to resolve each requested path to the
// content hash it must fetch from objects/ or a pack.
func (s *Store) ListPresentContent(ctx context.Context, volumeID int64) ([]PathDelta, error) {
	return queryRows(ctx, s.db, `
		SELECT `+pathFromFolderAndName+`, f.content_id, c.blake3, c.size_bytes, f.mtime_ns, f.status
		FROM `+fileFromJoin+`
		WHERE fo.volume_id = ?
		  AND f.status = 'present'
		  AND `+reservedSubtreeFilter+`
		ORDER BY `+pathFromFolderAndName+`
	`, scanPathDelta, volumeID)
}

func scanPathDelta(s rowScanner) (PathDelta, error) {
	var d PathDelta
	err := s.Scan(&d.Path, &d.ContentID, &d.Blake3, &d.SizeBytes, &d.MtimeNs, &d.Status)
	return d, err
}

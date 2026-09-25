package store

import (
	"context"
	"fmt"
)

// StoredRemotePath is a live or displaced remote_paths row with the volume
// it belongs to: a version a mirror holds by squirrel's records, at
// <volume>/<path> or in that volume's history.
type StoredRemotePath struct {
	RemotePath
	VolumeID int64
	Volume   string
}

// ListStoredRemotePaths returns every live and displaced row on the
// destination, in every volume: what a verify pass checks. They come least
// recently verified first, never-verified ones before all others, so a pass
// that re-reads only a slice of them rotates through the whole set.
func (s *Store) ListStoredRemotePaths(ctx context.Context, destination string) ([]StoredRemotePath, error) {
	return queryRows(ctx, s.db, `
		SELECT rp.id, rp.content_id, rp.written_run_id,
		       rp.state, rp.displaced_run_id, rp.mtime_ns, rp.checksum,
		       CASE fo.path WHEN '' THEN rp.name ELSE fo.path || '/' || rp.name END,
		       c.size_bytes, c.blake3, v.id, v.name
		FROM remote_paths rp
		JOIN folders fo ON fo.id = rp.folder_id
		JOIN volumes v ON v.id = fo.volume_id
		JOIN contents c ON c.id = rp.content_id
		WHERE rp.destination = ? AND rp.state IN ('live', 'displaced')
		ORDER BY rp.verified_at_ns IS NOT NULL, rp.verified_at_ns, rp.id
	`, func(sc rowScanner) (StoredRemotePath, error) {
		var r StoredRemotePath
		err := sc.Scan(&r.ID, &r.ContentID, &r.WrittenRunID,
			&r.State, &r.DisplacedRunID, &r.MtimeNs, &r.Checksum,
			&r.Path, &r.SizeBytes, &r.Blake3, &r.VolumeID, &r.Volume)
		return r, err
	}, destination)
}

// RecordRemotePathVerified records that a read of a live or displaced
// row's stored bytes just hashed to checksum, its content's BLAKE3 in
// lowercase hex, at atNs, and reports whether it did. A row that already
// records a checksum must record this one — a read never replaces the
// fingerprint it re-confirms — and a row a push moved since is left alone.
func (s *Store) RecordRemotePathVerified(ctx context.Context, id int64, checksum string, atNs int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE remote_paths SET checksum_algo = ?, checksum = ?, verified_at_ns = ?
		WHERE id = ? AND state IN ('live', 'displaced') AND (checksum IS NULL OR checksum = ?)
	`, ChecksumAlgoBlake3, checksum, atNs, id, checksum)
	if err != nil {
		return false, fmt.Errorf("record verification of remote path %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record verification of remote path %d: %w", id, err)
	}
	return n == 1, nil
}

// LoseStoredRemotePath marks a row lost that a verify pass found missing or
// changed, provided it is still in state, the state the pass read it in.
// It reports whether the row moved: a push may have moved it since, and
// then the pass's finding describes a version that is no longer there.
func (s *Store) LoseStoredRemotePath(ctx context.Context, id int64, state string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE remote_paths SET state = 'lost' WHERE id = ? AND state = ? AND state IN ('live', 'displaced')
	`, id, state)
	if err != nil {
		return false, fmt.Errorf("mark remote path %d lost: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark remote path %d lost: %w", id, err)
	}
	return n == 1, nil
}

// ListRemotePathRepairs returns the volume's present paths the destination
// lost: a lost row holds the path's current content, and no committing or
// live row does. A push writes them again although the index did not change
// — the planner's repairs. Ordered by path, like the delta.
func (s *Store) ListRemotePathRepairs(ctx context.Context, destination string, volumeID int64) ([]PathDelta, error) {
	return queryRows(ctx, s.db, `
		SELECT DISTINCT `+pathFromFolderAndName+`, f.folder_id, f.content_id, c.blake3, c.size_bytes, f.mtime_ns, f.status
		FROM remote_paths rp
		JOIN files f ON f.folder_id = rp.folder_id AND f.name = rp.name AND f.content_id = rp.content_id
		JOIN folders fo ON fo.id = f.folder_id
		JOIN contents c ON c.id = f.content_id
		WHERE rp.destination = ? AND rp.state = 'lost'
		  AND fo.volume_id = ? AND f.status = 'present'
		  AND `+reservedSubtreeFilter+`
		  AND NOT EXISTS (
			SELECT 1 FROM remote_paths held
			WHERE held.destination = rp.destination AND held.folder_id = rp.folder_id
			  AND held.name = rp.name AND held.content_id = rp.content_id
			  AND held.state IN ('committing', 'live')
		  )
		ORDER BY 1
	`, scanPathDelta, destination, volumeID)
}

package store

import (
	"context"
	"database/sql"
	"fmt"
)

// The states of a remote_paths row. Each move on the destination is
// recorded before it happens, so a crash leaves the row naming the move in
// flight and the next push settles it from where the bytes actually are.
const (
	// RemotePathCommitting: the version is staged and about to be renamed
	// onto its path.
	RemotePathCommitting = "committing"
	// RemotePathLive: the version is at its path.
	RemotePathLive = "live"
	// RemotePathDisplacing: the version is about to move into history.
	RemotePathDisplacing = "displacing"
	// RemotePathDisplaced: the version sits at
	// .squirrel-history/run-<DisplacedRunID>/<path>.
	RemotePathDisplaced = "displaced"
	// RemotePathLost: the bytes are no longer where the row says. The row
	// keeps its history but stops vouching for its content.
	RemotePathLost = "lost"
)

// RemotePath is one version squirrel wrote at a mirror path on a
// destination, keyed on the files row it came from. Path, SizeBytes and
// Blake3 are read through that row's folder and content. MtimeNs is the
// mtime the destination reported for the written version. Checksum is
// the BLAKE3 a read of the stored bytes confirmed, NULL while none did.
type RemotePath struct {
	ID             int64
	ContentID      int64
	WrittenRunID   int64
	State          string
	DisplacedRunID sql.NullInt64
	MtimeNs        int64
	Checksum       sql.NullString

	Path      string
	SizeBytes int64
	Blake3    []byte
}

// RemotePathWrite is the intent to commit one version at a mirror path.
// Checksum is the lowercase hex BLAKE3 a read-back of the staged version
// confirmed at VerifiedAtNs, or empty when the destination offered none.
type RemotePathWrite struct {
	Destination  string
	FolderID     int64
	Name         string
	ContentID    int64
	RunID        int64
	MtimeNs      int64
	Checksum     string
	VerifiedAtNs int64
}

// ChecksumAlgoBlake3 is the checksum_algo of a fingerprint squirrel
// computed itself, by reading the stored bytes back through BLAKE3.
const ChecksumAlgoBlake3 = "blake3"

// BeginRemotePathCommit records the intent to rename a staged version onto
// its path, as a committing row. The path's previous live row must already
// be displacing, displaced or lost: at most one committing-or-live row per
// (destination, path) exists, and the insert fails on that unique index
// otherwise.
func (s *Store) BeginRemotePathCommit(ctx context.Context, w RemotePathWrite) (int64, error) {
	if w.Destination == "" {
		return 0, fmt.Errorf("BeginRemotePathCommit: destination must be non-empty")
	}
	var algo, checksum sql.NullString
	var verified sql.NullInt64
	if w.Checksum != "" {
		algo = sql.NullString{String: ChecksumAlgoBlake3, Valid: true}
		checksum = sql.NullString{String: w.Checksum, Valid: true}
		verified = sql.NullInt64{Int64: w.VerifiedAtNs, Valid: true}
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO remote_paths (destination, folder_id, name, content_id, written_run_id, state, mtime_ns,
		                          checksum_algo, checksum, verified_at_ns)
		VALUES (?, ?, ?, ?, ?, 'committing', ?, ?, ?, ?)
	`, w.Destination, w.FolderID, w.Name, w.ContentID, w.RunID, w.MtimeNs, algo, checksum, verified)
	if err != nil {
		return 0, fmt.Errorf("record commit of %q on %q: %w", w.Name, w.Destination, err)
	}
	return res.LastInsertId()
}

// ConfirmRemotePathsLive moves committing rows to live once their versions
// are at their paths.
func (s *Store) ConfirmRemotePathsLive(ctx context.Context, ids ...int64) error {
	return s.transitionRemotePaths(ctx, ids, RemotePathLive, `
		UPDATE remote_paths SET state = 'live' WHERE id = ? AND state = 'committing'`)
}

// BeginRemotePathsDisplace records that live rows are about to move into
// .squirrel-history/run-<runID>/. A directory displaced because a file
// replaced it moves every live row under it, all in one call.
func (s *Store) BeginRemotePathsDisplace(ctx context.Context, runID int64, ids ...int64) error {
	return s.transitionRemotePaths(ctx, ids, RemotePathDisplacing, `
		UPDATE remote_paths SET state = 'displacing', displaced_run_id = ?
		WHERE id = ? AND state = 'live'`, runID)
}

// ConfirmRemotePathsDisplaced moves displacing rows to displaced once their
// versions are in history.
func (s *Store) ConfirmRemotePathsDisplaced(ctx context.Context, ids ...int64) error {
	return s.transitionRemotePaths(ctx, ids, RemotePathDisplaced, `
		UPDATE remote_paths SET state = 'displaced' WHERE id = ? AND state = 'displacing'`)
}

// RevertRemotePathsLive withdraws a displacement that never happened: the
// version is still at its path, so the row goes back to live.
func (s *Store) RevertRemotePathsLive(ctx context.Context, ids ...int64) error {
	return s.transitionRemotePaths(ctx, ids, RemotePathLive, `
		UPDATE remote_paths SET state = 'live', displaced_run_id = NULL
		WHERE id = ? AND state = 'displacing'`)
}

// MarkRemotePathsLost records that the rows' bytes are no longer where the
// rows say: other bytes are there, or nothing. A withdrawn commit ends
// here too, because its version never reached its path.
func (s *Store) MarkRemotePathsLost(ctx context.Context, ids ...int64) error {
	return s.transitionRemotePaths(ctx, ids, RemotePathLost, `
		UPDATE remote_paths SET state = 'lost' WHERE id = ? AND state != 'lost'`)
}

// transitionRemotePaths applies one state transition to every id in one
// transaction. update takes the prefix args followed by the row id. Each
// row must be in the transition's source state or the whole batch fails,
// so a record moves only through the moves it witnessed.
func (s *Store) transitionRemotePaths(ctx context.Context, ids []int64, to, update string, prefix ...any) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin remote path transition to %s: %w", to, err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, update, append(append([]any{}, prefix...), id)...)
		if err != nil {
			return fmt.Errorf("move remote path %d to %s: %w", id, to, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("move remote path %d to %s: %w", id, to, err)
		}
		if n != 1 {
			return fmt.Errorf("move remote path %d to %s: the row is not in the state this move starts from", id, to)
		}
	}
	return tx.Commit()
}

// remotePathSelect reads remote_paths rows with their volume-relative path
// and content size and hash (table aliases rp, fo, c).
const remotePathSelect = `
	SELECT rp.id, rp.content_id, rp.written_run_id,
	       rp.state, rp.displaced_run_id, rp.mtime_ns, rp.checksum,
	       CASE fo.path WHEN '' THEN rp.name ELSE fo.path || '/' || rp.name END,
	       c.size_bytes, c.blake3
	FROM remote_paths rp
	JOIN folders fo ON fo.id = rp.folder_id
	JOIN contents c ON c.id = rp.content_id`

func scanRemotePath(s rowScanner) (RemotePath, error) {
	var r RemotePath
	err := s.Scan(&r.ID, &r.ContentID, &r.WrittenRunID,
		&r.State, &r.DisplacedRunID, &r.MtimeNs, &r.Checksum,
		&r.Path, &r.SizeBytes, &r.Blake3)
	return r, err
}

// ListLiveRemotePaths returns the volume's live rows on the destination,
// ordered by path: what the destination's mirror tree holds by squirrel's
// records.
func (s *Store) ListLiveRemotePaths(ctx context.Context, destination string, volumeID int64) ([]RemotePath, error) {
	return queryRows(ctx, s.db, remotePathSelect+`
		WHERE rp.destination = ? AND fo.volume_id = ? AND rp.state = 'live'
		ORDER BY 9
	`, scanRemotePath, destination, volumeID)
}

// ListUnsettledRemotePaths returns the volume's committing and displacing
// rows on the destination: the moves a crashed push left in flight.
func (s *Store) ListUnsettledRemotePaths(ctx context.Context, destination string, volumeID int64) ([]RemotePath, error) {
	return queryRows(ctx, s.db, remotePathSelect+`
		WHERE rp.destination = ? AND fo.volume_id = ? AND rp.state IN ('committing', 'displacing')
		ORDER BY rp.id
	`, scanRemotePath, destination, volumeID)
}

// ListRemotePaths returns every row of the volume on the destination, in
// insertion order.
func (s *Store) ListRemotePaths(ctx context.Context, destination string, volumeID int64) ([]RemotePath, error) {
	return queryRows(ctx, s.db, remotePathSelect+`
		WHERE rp.destination = ? AND fo.volume_id = ?
		ORDER BY rp.id
	`, scanRemotePath, destination, volumeID)
}

// CountInSyncRemotePaths counts the volume's live rows on the destination
// whose files row is still present: paths the destination holds with
// their current content by squirrel's records, the mirror's "already
// correct".
func (s *Store) CountInSyncRemotePaths(ctx context.Context, destination string, volumeID int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM remote_paths rp
		JOIN folders fo ON fo.id = rp.folder_id
		JOIN files f ON f.folder_id = rp.folder_id AND f.name = rp.name AND f.content_id = rp.content_id
		WHERE rp.destination = ? AND fo.volume_id = ? AND rp.state = 'live' AND f.status = 'present'
	`, destination, volumeID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count in-sync mirror paths on %q: %w", destination, err)
	}
	return n, nil
}

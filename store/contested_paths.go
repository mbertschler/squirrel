package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ContestedPath is one row of the contested_paths latch: a path frozen by
// a peer-sync conflict so divergent edits stop ping-ponging (#158, F27).
// While the row stands the receiver refuses to re-supersede the path (the
// `contested` disposition) and every node that knows about it surfaces a
// badge; both versions stay preserved — the winner live at Path, the loser
// under .squirrel-conflicts/ at PreservedAtPath. It is derived standing
// state, like DestinationAlarm: the permanent record of every conflict
// lives in runs/runs_audit and the preserved bytes on disk, so clearing
// the latch loses no history.
//
// LiveBlake3 is the frozen winner (nil when a node recorded the freeze
// without knowing the winning digest); PreservedBlake3 is the loser.
// PeerNodeID is the node that caused the freeze — the initiator peer on
// the receiver, or the destination peer on an initiator — and is 0 when
// unattributed. RaisedRunID references the run that first raised the
// latch; RaisedAtNs is stable across repeated conflicts so a surface can
// show "contested since".
type ContestedPath struct {
	VolumeID        int64
	Path            string
	LiveBlake3      []byte
	PreservedBlake3 []byte
	PreservedAtPath string
	PeerNodeID      int64
	RaisedRunID     int64
	RaisedAtNs      int64
}

// RaiseContested latches a contested freeze on (volume, path) when none is
// already active. The raise is idempotent: an existing latch keeps its
// original digests, timestamp, and run so a repeated conflict does not
// reset "contested since" or append a duplicate audit row — the freeze is
// recorded once, not every cadence tick (the F27 fix). A
// 'contested-raise' runs_audit entry is written against RaisedRunID in the
// same transaction as the insert, only when a fresh latch is created.
func (s *Store) RaiseContested(ctx context.Context, c ContestedPath) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin raise contested %q: %w", c.Path, err)
	}
	defer func() { _ = tx.Rollback() }()

	atNs := NowNs()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO contested_paths (
			volume_id, path, live_blake3, preserved_blake3, preserved_at_path,
			peer_node_id, raised_run_id, raised_at_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(volume_id, path) DO NOTHING
	`, c.VolumeID, c.Path, nullBlob(c.LiveBlake3), nullBlob(c.PreservedBlake3),
		nullString(c.PreservedAtPath), nullPeerID(c.PeerNodeID), c.RaisedRunID, atNs)
	if err != nil {
		return fmt.Errorf("raise contested %q: %w", c.Path, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("raise contested %q rows affected: %w", c.Path, err)
	}
	if inserted == 0 {
		// Already frozen: keep the original latch untouched.
		return tx.Commit()
	}
	if err := appendRunAuditTx(ctx, tx, RunAuditEntry{
		RunID: c.RaisedRunID, Transition: TransitionContestedRaise, Note: c.Path,
	}, atNs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit raise contested %q: %w", c.Path, err)
	}
	return nil
}

// ClearContested clears the active freeze on (volume, path), if any, and
// appends a 'contested-clear' runs_audit entry (tagged with operator)
// against the run that raised it. Returns whether a latch was active.
// Deleting the latch loses no history: the raise and this clear both
// survive in runs_audit, every conflict run survives in runs, and the
// preserved bytes stay on disk under .squirrel-conflicts/.
func (s *Store) ClearContested(ctx context.Context, volumeID int64, path, operator string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin clear contested %q: %w", path, err)
	}
	defer func() { _ = tx.Rollback() }()

	var raisedRunID int64
	err = tx.QueryRowContext(ctx,
		`SELECT raised_run_id FROM contested_paths WHERE volume_id = ? AND path = ?`,
		volumeID, path).Scan(&raisedRunID)
	if IsNotFound(err) {
		return false, tx.Commit()
	}
	if err != nil {
		return false, fmt.Errorf("read contested %q: %w", path, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM contested_paths WHERE volume_id = ? AND path = ?`,
		volumeID, path); err != nil {
		return false, fmt.Errorf("clear contested %q: %w", path, err)
	}
	if err := appendRunAuditTx(ctx, tx, RunAuditEntry{
		RunID: raisedRunID, Transition: TransitionContestedClear, Operator: operator, Note: path,
	}, NowNs()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit clear contested %q: %w", path, err)
	}
	return true, nil
}

// IsPathContested reports whether (volume, path) is frozen and, if so,
// returns the frozen winner's digest so the classifier can distinguish the
// winner re-asserting (digest matches — allowed) from a divergent
// re-assertion (digest differs — refused). liveBlake3 is nil when the
// freeze was recorded without a known winner digest.
func (s *Store) IsPathContested(ctx context.Context, volumeID int64, path string) (liveBlake3 []byte, contested bool, err error) {
	var digest []byte
	err = s.db.QueryRowContext(ctx,
		`SELECT live_blake3 FROM contested_paths WHERE volume_id = ? AND path = ?`,
		volumeID, path).Scan(&digest)
	if IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("check contested %q: %w", path, err)
	}
	return digest, true, nil
}

// GetContestedPath returns the active freeze on (volume, path), or
// sql.ErrNoRows (IsNotFound) when the path is not contested.
func (s *Store) GetContestedPath(ctx context.Context, volumeID int64, path string) (ContestedPath, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+contestedColumns+` FROM contested_paths WHERE volume_id = ? AND path = ?`,
		volumeID, path)
	return scanContestedPath(row.Scan)
}

// ListContestedPaths returns every active freeze, ordered by (volume, path)
// so the listing is deterministic. An empty slice means nothing is frozen.
func (s *Store) ListContestedPaths(ctx context.Context) ([]ContestedPath, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+contestedColumns+` FROM contested_paths ORDER BY volume_id, path`)
	if err != nil {
		return nil, fmt.Errorf("list contested paths: %w", err)
	}
	defer rows.Close()
	var out []ContestedPath
	for rows.Next() {
		c, err := scanContestedPath(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountContestedRaisedByRun returns a map from run id to the number of
// contested_paths latches that run raised, for the given run ids. Only
// runs with a non-zero count appear as keys. `squirrel runs` uses it to
// fill the CONFLICTS column for an initiator's own sync runs — where the
// conflict was frozen on a remote receiver, so the receiver-side
// `.squirrel-conflicts/run-N/` prefix count is zero on this node.
func (s *Store) CountContestedRaisedByRun(ctx context.Context, runIDs []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(runIDs))
	if len(runIDs) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(runIDs)-1) + "?"
	args := make([]any, 0, len(runIDs))
	for _, id := range runIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT raised_run_id, COUNT(*) FROM contested_paths
		 WHERE raised_run_id IN (`+placeholders+`)
		 GROUP BY raised_run_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("count contested by run: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("scan contested count row: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

const contestedColumns = `volume_id, path, live_blake3, preserved_blake3, preserved_at_path, peer_node_id, raised_run_id, raised_at_ns`

func scanContestedPath(scan func(...any) error) (ContestedPath, error) {
	var (
		c      ContestedPath
		prsvd  sql.NullString
		peerID sql.NullInt64
	)
	if err := scan(&c.VolumeID, &c.Path, &c.LiveBlake3, &c.PreservedBlake3,
		&prsvd, &peerID, &c.RaisedRunID, &c.RaisedAtNs); err != nil {
		return ContestedPath{}, err
	}
	c.PreservedAtPath = prsvd.String
	c.PeerNodeID = peerID.Int64
	return c, nil
}

// nullBlob renders a digest for an INSERT bind: an empty slice becomes a
// NULL BLOB (the "digest unknown" convention), any other value binds as-is.
func nullBlob(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// nullPeerID maps a zero node id to a NULL peer_node_id (unattributed
// freeze) and any positive id to a valid one.
func nullPeerID(id int64) sql.NullInt64 {
	if id == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: id, Valid: true}
}

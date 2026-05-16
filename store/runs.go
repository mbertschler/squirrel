package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Run is one entry in the runs table. VolumeID uses sql.NullInt64 because the
// column is nullable (cross-volume sync runs in the future); EndedAtNs and
// Error are likewise nullable while a run is still in-flight or finished
// without an error. FileCount is int64 to match the SQLite INTEGER column.
type Run struct {
	ID          int64
	Kind        string
	VolumeID    sql.NullInt64
	StartedAtNs int64
	EndedAtNs   sql.NullInt64
	Status      string
	Error       sql.NullString
	FileCount   int64
}

// BeginRun records the start of an index or sync run and returns its id.
// Callers must pair it with FinishRun (typically via defer with an error
// pointer or an explicit terminal call). volumeID must reference an existing
// volume; the schema column is nullable in anticipation of cross-volume sync
// but no current caller relies on that.
func (s *Store) BeginRun(ctx context.Context, kind string, volumeID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (kind, volume_id, started_at_ns, status, file_count)
		VALUES (?, ?, ?, 'running', 0)
	`, kind, volumeID, NowNs())
	if err != nil {
		return 0, fmt.Errorf("insert run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("run last insert id: %w", err)
	}
	return id, nil
}

// FinishRun records the terminal state of a run. errMsg is stored as NULL
// when empty. fileCount should be the total number of files the run touched
// (added + modified + unchanged) so the runs table doubles as a coarse audit
// log without needing to scan files. Returns an error if the runID does not
// match any row, which would otherwise leave a run stuck in status='running'.
func (s *Store) FinishRun(ctx context.Context, runID int64, status string, errMsg string, fileCount int64) error {
	var errVal sql.NullString
	if errMsg != "" {
		errVal = sql.NullString{String: errMsg, Valid: true}
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET ended_at_ns = ?, status = ?, error = ?, file_count = ?
		WHERE id = ?
	`, NowNs(), status, errVal, fileCount, runID)
	if err != nil {
		return fmt.Errorf("finish run %d: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish run %d rows affected: %w", runID, err)
	}
	if n == 0 {
		return fmt.Errorf("finish run %d: no such run", runID)
	}
	return nil
}

// ListRunsOpts filters and shapes the result of ListRuns. The zero value
// returns every run, oldest first, with no cap.
type ListRunsOpts struct {
	// VolumeID, when non-nil, restricts results to runs against that volume.
	// A nil VolumeID returns runs across every volume (including any future
	// cross-volume sync runs, which have a NULL volume_id).
	VolumeID *int64
	// Limit caps the result count. Zero (or negative) means no cap.
	Limit int
	// Descending sorts by id descending (most recent first). Defaults to
	// ascending so callers that walk history get start-order chronological
	// output without thinking about it.
	Descending bool
}

// ListRuns returns runs matching opts. See ListRunsOpts for filter and
// ordering semantics.
func (s *Store) ListRuns(ctx context.Context, opts ListRunsOpts) ([]Run, error) {
	query := `SELECT id, kind, volume_id, started_at_ns, ended_at_ns, status, error, file_count FROM runs`
	var args []any
	if opts.VolumeID != nil {
		query += ` WHERE volume_id = ?`
		args = append(args, *opts.VolumeID)
	}
	if opts.Descending {
		query += ` ORDER BY id DESC`
	} else {
		query += ` ORDER BY id`
	}
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.Kind, &r.VolumeID, &r.StartedAtNs, &r.EndedAtNs, &r.Status, &r.Error, &r.FileCount); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

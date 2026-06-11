package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RemoteObject is the per-(content, destination) upload record for
// destinations whose stored bytes can't be cheaply re-read. The row is
// written once at upload time; ChecksumAlgo and Checksum carry the
// provider's own checksum for the uploaded object, compared verbatim
// ("value then" vs "value now") on later verification passes. The pair
// is NULL together while the fingerprint is still pending — the upload
// happened, the scan-back pass hasn't filled the provider checksum in
// yet (a CHECK in the schema keeps the two columns paired).
// UploadedRunID references the local run that performed the upload;
// VerifiedAtNs is NULL until the first re-verification confirms the
// object unchanged.
type RemoteObject struct {
	ContentID     int64
	Destination   string
	UploadedRunID int64
	ChecksumAlgo  sql.NullString
	Checksum      sql.NullString
	VerifiedAtNs  sql.NullInt64
}

// InsertRemoteObject records one freshly uploaded object, with its
// fingerprint when the caller already holds one and with the checksum
// pair NULL when the fingerprint comes later. Content is uploaded at
// most once per destination (the offsite layout is content-addressed
// and append-only), so a second insert for the same (content,
// destination) fails on the primary key rather than silently replacing
// the record future verifications compare against.
func (s *Store) InsertRemoteObject(ctx context.Context, o RemoteObject) error {
	if o.Destination == "" {
		return fmt.Errorf("InsertRemoteObject: destination must be non-empty")
	}
	if o.ChecksumAlgo.Valid != o.Checksum.Valid {
		return fmt.Errorf("InsertRemoteObject: checksum_algo and checksum must be set together")
	}
	if o.ChecksumAlgo.Valid && (o.ChecksumAlgo.String == "" || o.Checksum.String == "") {
		return fmt.Errorf("InsertRemoteObject: checksum_algo and checksum must be non-empty when set")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO remote_objects (content_id, destination, uploaded_run_id, checksum_algo, checksum, verified_at_ns)
		VALUES (?, ?, ?, ?, ?, ?)
	`, o.ContentID, o.Destination, o.UploadedRunID, o.ChecksumAlgo, o.Checksum, o.VerifiedAtNs)
	if err != nil {
		return fmt.Errorf("insert remote object: %w", err)
	}
	return nil
}

// GetRemoteObject returns the upload record for one (content,
// destination), or sql.ErrNoRows when the content was never recorded as
// uploaded there.
func (s *Store) GetRemoteObject(ctx context.Context, contentID int64, destination string) (RemoteObject, error) {
	var o RemoteObject
	err := s.db.QueryRowContext(ctx, `
		SELECT content_id, destination, uploaded_run_id, checksum_algo, checksum, verified_at_ns
		FROM remote_objects
		WHERE content_id = ? AND destination = ?
	`, contentID, destination).
		Scan(&o.ContentID, &o.Destination, &o.UploadedRunID, &o.ChecksumAlgo, &o.Checksum, &o.VerifiedAtNs)
	return o, err
}

// HasRemoteObject reports whether an upload record exists for the
// (content, destination) pair. The content-addressed push uses it as
// its upload-once gate: a recorded content hash is skipped, fingerprint
// present or pending.
func (s *Store) HasRemoteObject(ctx context.Context, contentID int64, destination string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM remote_objects WHERE content_id = ? AND destination = ?
	`, contentID, destination).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lookup remote object: %w", err)
	}
	return true, nil
}

// MarkRemoteObjectVerified stamps verified_at_ns after a verification
// pass re-read the provider checksum and found it equal to the recorded
// one. Returns an error when no record exists for the pair — verifying
// an unrecorded upload is a caller bug.
func (s *Store) MarkRemoteObjectVerified(ctx context.Context, contentID int64, destination string, atNs int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE remote_objects SET verified_at_ns = ?
		WHERE content_id = ? AND destination = ?
	`, atNs, contentID, destination)
	if err != nil {
		return fmt.Errorf("mark remote object verified: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark remote object verified rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("mark remote object verified: no remote object for content %d on %q", contentID, destination)
	}
	return nil
}

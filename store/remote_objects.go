package store

import (
	"context"
	"database/sql"
	"fmt"
)

// RemoteObject is the per-(content, destination) upload fingerprint for
// destinations whose stored bytes can't be cheaply re-read: the
// provider's own checksum for the uploaded object, recorded once at
// upload time and compared verbatim ("value then" vs "value now") on
// later verification passes. ChecksumAlgo names the provider checksum
// scheme so a reader knows what the opaque Checksum string is.
// UploadedRunID references the local run that performed the upload;
// VerifiedAtNs is NULL until the first re-verification confirms the
// object unchanged.
type RemoteObject struct {
	ContentID     int64
	Destination   string
	UploadedRunID int64
	ChecksumAlgo  string
	Checksum      string
	VerifiedAtNs  sql.NullInt64
}

// InsertRemoteObject records the fingerprint for one freshly uploaded
// object. Content is uploaded at most once per destination (the offsite
// layout is content-addressed and append-only), so a second insert for
// the same (content, destination) fails on the primary key rather than
// silently replacing the fingerprint future verifications compare
// against.
func (s *Store) InsertRemoteObject(ctx context.Context, o RemoteObject) error {
	if o.Destination == "" {
		return fmt.Errorf("InsertRemoteObject: destination must be non-empty")
	}
	if o.ChecksumAlgo == "" || o.Checksum == "" {
		return fmt.Errorf("InsertRemoteObject: checksum_algo and checksum must be non-empty")
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

// GetRemoteObject returns the fingerprint for one (content,
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

// MarkRemoteObjectVerified stamps verified_at_ns after a verification
// pass re-read the provider checksum and found it equal to the recorded
// one. Returns an error when no fingerprint exists for the pair — a
// verification against an unrecorded upload is a caller bug, not a
// silent no-op.
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
		return err
	}
	if n == 0 {
		return fmt.Errorf("mark remote object verified: no remote object for content %d on %q", contentID, destination)
	}
	return nil
}

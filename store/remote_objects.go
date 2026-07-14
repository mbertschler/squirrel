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
// UploadedRunID references the local run that performed the upload.
// VerifiedAtNs is stamped when the scan-back read fills the fingerprint —
// that read is itself the object's first verification (there is no
// independent local value to compare against, so the recorded checksum is
// the provider's own) — and is re-stamped by each later verify pass. It is
// NULL only while the fingerprint pair is still pending.
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

// SetRemoteObjectFingerprint fills the pending checksum pair on an upload
// record and stamps verified_at_ns in the same write: the scan-back
// fingerprint read from the provider after the upload was confirmed. That
// read is itself the first verification — for the ciphertext-ETag model
// there is no independent local value to compare against, so the recorded
// value is the provider's own, and reading it establishes the baseline
// every later verification re-confirms against. Only a NULL pair is filled,
// so a second write for the same (content, destination) fails instead of
// replacing it, and a missing record errors as a caller bug. Mirrors
// SetRemotePackFingerprint; the periodic verify (MarkRemoteObjectVerified)
// re-stamps verified_at_ns on each re-confirmation.
func (s *Store) SetRemoteObjectFingerprint(ctx context.Context, contentID int64, destination, algo, checksum string, atNs int64) error {
	if algo == "" || checksum == "" {
		return fmt.Errorf("SetRemoteObjectFingerprint: algo and checksum must be non-empty")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE remote_objects SET checksum_algo = ?, checksum = ?, verified_at_ns = ?
		WHERE content_id = ? AND destination = ?
		  AND checksum_algo IS NULL AND checksum IS NULL
	`, algo, checksum, atNs, contentID, destination)
	if err != nil {
		return fmt.Errorf("set remote object fingerprint: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set remote object fingerprint rows: %w", err)
	}
	if n == 0 {
		if _, getErr := s.GetRemoteObject(ctx, contentID, destination); errors.Is(getErr, sql.ErrNoRows) {
			return fmt.Errorf("set remote object fingerprint: no remote object for content %d on %q", contentID, destination)
		} else if getErr != nil {
			return fmt.Errorf("set remote object fingerprint: %w", getErr)
		}
		return fmt.Errorf("set remote object fingerprint: content %d on %q already has a recorded fingerprint", contentID, destination)
	}
	return nil
}

// RemoteObjectRecord pairs an upload record with its content hash; the
// destination-side object key is the lowercase hex of Blake3.
type RemoteObjectRecord struct {
	RemoteObject
	Blake3 []byte
}

// ListRemoteObjects returns every upload record for the destination with
// the content hash joined in, ordered by hash so verification output is
// deterministic.
func (s *Store) ListRemoteObjects(ctx context.Context, destination string) ([]RemoteObjectRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.content_id, r.destination, r.uploaded_run_id,
		       r.checksum_algo, r.checksum, r.verified_at_ns, c.blake3
		FROM remote_objects r
		JOIN contents c ON c.id = r.content_id
		WHERE r.destination = ?
		ORDER BY c.blake3
	`, destination)
	if err != nil {
		return nil, fmt.Errorf("list remote objects for %q: %w", destination, err)
	}
	defer rows.Close()
	var out []RemoteObjectRecord
	for rows.Next() {
		var r RemoteObjectRecord
		if err := rows.Scan(&r.ContentID, &r.Destination, &r.UploadedRunID,
			&r.ChecksumAlgo, &r.Checksum, &r.VerifiedAtNs, &r.Blake3); err != nil {
			return nil, fmt.Errorf("scan remote object row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
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

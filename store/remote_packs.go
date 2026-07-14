package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RemotePack is the per-(pack, destination) upload record for the packed
// layout — the pack-level analog of RemoteObject. The row is written once
// at upload time; ChecksumAlgo and Checksum carry the provider's own
// checksum for the uploaded pack (an S3 multipart ETag, or the backend
// hash for non-s3 remotes), compared verbatim on later verification
// passes. The pair is NULL together while the fingerprint is still pending
// — the upload happened but the scan-back read hasn't filled the provider
// checksum in yet (a CHECK in the schema keeps the two columns paired).
// One re-confirmed fingerprint per pack vouches for every content the pack
// holds, so a verified remote_packs row certifies all its members offsite.
// UploadedRunID references the local run that performed the upload;
// VerifiedAtNs is NULL until a fingerprint read confirms the pack.
type RemotePack struct {
	PackID        int64
	Destination   string
	UploadedRunID int64
	ChecksumAlgo  sql.NullString
	Checksum      sql.NullString
	VerifiedAtNs  sql.NullInt64
}

// InsertRemotePack records one freshly uploaded pack, with the checksum
// pair NULL when the scan-back fingerprint comes later. A pack's bytes are
// content-global and uploaded at most once per destination, so a second
// insert for the same (pack, destination) fails on the primary key rather
// than silently replacing the record future verifications compare against.
func (s *Store) InsertRemotePack(ctx context.Context, p RemotePack) error {
	if p.Destination == "" {
		return fmt.Errorf("InsertRemotePack: destination must be non-empty")
	}
	if p.ChecksumAlgo.Valid != p.Checksum.Valid {
		return fmt.Errorf("InsertRemotePack: checksum_algo and checksum must be set together")
	}
	if p.ChecksumAlgo.Valid && (p.ChecksumAlgo.String == "" || p.Checksum.String == "") {
		return fmt.Errorf("InsertRemotePack: checksum_algo and checksum must be non-empty when set")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO remote_packs (pack_id, destination, uploaded_run_id, checksum_algo, checksum, verified_at_ns)
		VALUES (?, ?, ?, ?, ?, ?)
	`, p.PackID, p.Destination, p.UploadedRunID, p.ChecksumAlgo, p.Checksum, p.VerifiedAtNs)
	if err != nil {
		return fmt.Errorf("insert remote pack: %w", err)
	}
	return nil
}

// SetRemotePackFingerprint fills the pending checksum pair on an upload
// record and stamps verified_at_ns in one write: the scan-back read of the
// provider checksum after the upload was confirmed both records the
// fingerprint and is its first verification — a genuine read of the stored
// bytes' provider checksum. Only a NULL pair is filled — the recorded
// fingerprint is the baseline every later verification compares against, so
// a second write for the same (pack, destination) fails instead of
// replacing it, and a missing record errors as a caller bug.
func (s *Store) SetRemotePackFingerprint(ctx context.Context, packID int64, destination, algo, checksum string, atNs int64) error {
	if algo == "" || checksum == "" {
		return fmt.Errorf("SetRemotePackFingerprint: algo and checksum must be non-empty")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE remote_packs SET checksum_algo = ?, checksum = ?, verified_at_ns = ?
		WHERE pack_id = ? AND destination = ?
		  AND checksum_algo IS NULL AND checksum IS NULL
	`, algo, checksum, atNs, packID, destination)
	if err != nil {
		return fmt.Errorf("set remote pack fingerprint: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set remote pack fingerprint rows: %w", err)
	}
	if n == 0 {
		if _, getErr := s.GetRemotePack(ctx, packID, destination); errors.Is(getErr, sql.ErrNoRows) {
			return fmt.Errorf("set remote pack fingerprint: no remote pack for pack %d on %q", packID, destination)
		} else if getErr != nil {
			return fmt.Errorf("set remote pack fingerprint: %w", getErr)
		}
		return fmt.Errorf("set remote pack fingerprint: pack %d on %q already has a recorded fingerprint", packID, destination)
	}
	return nil
}

// MarkRemotePackVerified stamps verified_at_ns after a verification pass
// re-read the provider checksum and found it equal to the recorded one.
// Returns an error when no record exists for the pair — verifying an
// unrecorded upload is a caller bug.
func (s *Store) MarkRemotePackVerified(ctx context.Context, packID int64, destination string, atNs int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE remote_packs SET verified_at_ns = ?
		WHERE pack_id = ? AND destination = ?
	`, atNs, packID, destination)
	if err != nil {
		return fmt.Errorf("mark remote pack verified: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark remote pack verified rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("mark remote pack verified: no remote pack for pack %d on %q", packID, destination)
	}
	return nil
}

// GetRemotePack returns the upload record for one (pack, destination), or
// sql.ErrNoRows when the pack was never recorded as uploaded there.
func (s *Store) GetRemotePack(ctx context.Context, packID int64, destination string) (RemotePack, error) {
	var p RemotePack
	err := s.db.QueryRowContext(ctx, `
		SELECT pack_id, destination, uploaded_run_id, checksum_algo, checksum, verified_at_ns
		FROM remote_packs
		WHERE pack_id = ? AND destination = ?
	`, packID, destination).
		Scan(&p.PackID, &p.Destination, &p.UploadedRunID, &p.ChecksumAlgo, &p.Checksum, &p.VerifiedAtNs)
	return p, err
}

// RemotePackRecord pairs an upload record with its pack key; the
// destination-side pack file is named by the lowercase hex of PackKey.
type RemotePackRecord struct {
	RemotePack
	PackKey []byte
}

// ListRemotePacks returns every upload record for the destination with the
// pack key joined in, ordered by key so verification output is
// deterministic.
func (s *Store) ListRemotePacks(ctx context.Context, destination string) ([]RemotePackRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.pack_id, r.destination, r.uploaded_run_id,
		       r.checksum_algo, r.checksum, r.verified_at_ns, p.pack_key
		FROM remote_packs r
		JOIN packs p ON p.id = r.pack_id
		WHERE r.destination = ?
		ORDER BY p.pack_key
	`, destination)
	if err != nil {
		return nil, fmt.Errorf("list remote packs for %q: %w", destination, err)
	}
	defer rows.Close()
	var out []RemotePackRecord
	for rows.Next() {
		var r RemotePackRecord
		if err := rows.Scan(&r.PackID, &r.Destination, &r.UploadedRunID,
			&r.ChecksumAlgo, &r.Checksum, &r.VerifiedAtNs, &r.PackKey); err != nil {
			return nil, fmt.Errorf("scan remote pack row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

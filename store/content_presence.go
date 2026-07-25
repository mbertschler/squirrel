package store

import (
	"context"
	"fmt"
)

// ContentPresentOnDestination reports whether the content is recorded as
// uploaded to the destination by either offsite layout: a remote_objects
// row (the content-addressed per-hash object) or a remote_packs row for a
// pack this content belongs to (the packed layout bundles it). Fingerprint
// state is ignored — this is the upload-once dedup gate, the two-source
// generalisation of HasRemoteObject, so a push never re-uploads bytes
// already offsite in either form. Both branches are per-destination: a pack
// this content sits in counts only when that pack was uploaded to *this*
// destination, so a pack landed elsewhere never suppresses a needed upload.
func (s *Store) ContentPresentOnDestination(ctx context.Context, contentID int64, destination string) (bool, error) {
	var present int
	err := s.db.QueryRowContext(ctx, `
		SELECT
			EXISTS (SELECT 1 FROM remote_objects WHERE content_id = ? AND destination = ?)
			OR EXISTS (
				SELECT 1 FROM pack_members pm
				JOIN remote_packs rp ON rp.pack_id = pm.pack_id
				WHERE pm.content_id = ? AND rp.destination = ?
			)
	`, contentID, destination, contentID, destination).Scan(&present)
	if err != nil {
		return false, fmt.Errorf("lookup content presence on %q: %w", destination, err)
	}
	return present != 0, nil
}

// CountVolumeContentsPendingFingerprint counts the distinct present
// contents of the volume that do NOT yet carry a verified provider
// fingerprint on the destination — the whole-state "pending artifact"
// tally the fingerprint-verified upgrade gates on. A content counts as
// pending unless it has either a remote_objects row or a member pack's
// remote_packs row on this destination with a non-NULL checksum and
// verified_at_ns, so content not yet uploaded, uploaded but not yet
// fingerprinted, or fingerprinted but not yet re-confirmed all keep the
// tally above zero. Zero means every present content is fingerprint-
// verified on the destination, so the vector may advance to
// VerifyMethodFingerprint over the whole present set. The reserved sync
// subtrees are excluded, matching PresentOriginMaxima — they never travel
// to a destination.
func (s *Store) CountVolumeContentsPendingFingerprint(ctx context.Context, volumeID int64, destination string) (int, error) {
	var pending int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT DISTINCT f.content_id
			FROM files f
			JOIN folders fo ON fo.id = f.folder_id
			WHERE fo.volume_id = ? AND f.status = 'present'
			  AND `+reservedSubtreeFilter+`
			  AND NOT (
				EXISTS (
					SELECT 1 FROM remote_objects ro
					WHERE ro.content_id = f.content_id AND ro.destination = ?
					  AND ro.checksum IS NOT NULL AND ro.verified_at_ns IS NOT NULL
				)
				OR EXISTS (
					SELECT 1 FROM pack_members pm
					JOIN remote_packs rp ON rp.pack_id = pm.pack_id
					WHERE pm.content_id = f.content_id AND rp.destination = ?
					  AND rp.checksum IS NOT NULL AND rp.verified_at_ns IS NOT NULL
				)
			  )
		)
	`, volumeID, destination, destination).Scan(&pending)
	if err != nil {
		return 0, fmt.Errorf("count pending fingerprints for volume %d on %q: %w", volumeID, destination, err)
	}
	return pending, nil
}

// ContentFingerprintVerified reports whether a verified provider
// fingerprint backs the content on the destination via either layout: a
// remote_objects row carrying a checksum and a verified_at_ns, or a
// remote_packs row (for a pack this content belongs to) likewise
// re-confirmed. The offload gate uses it to certify a presence+size
// component — a coarse vector that only claims bytes-present — so packed
// content gates exactly when its pack has a re-confirmed fingerprint, the
// packed analogue of the per-object scan-back the content-addressed layout
// requires. One verified pack vouches for every member.
func (s *Store) ContentFingerprintVerified(ctx context.Context, contentID int64, destination string) (bool, error) {
	var verified int
	err := s.db.QueryRowContext(ctx, `
		SELECT
			EXISTS (
				SELECT 1 FROM remote_objects
				WHERE content_id = ? AND destination = ?
				  AND checksum IS NOT NULL AND verified_at_ns IS NOT NULL
			)
			OR EXISTS (
				SELECT 1 FROM pack_members pm
				JOIN remote_packs rp ON rp.pack_id = pm.pack_id
				WHERE pm.content_id = ? AND rp.destination = ?
				  AND rp.checksum IS NOT NULL AND rp.verified_at_ns IS NOT NULL
			)
	`, contentID, destination, contentID, destination).Scan(&verified)
	if err != nil {
		return false, fmt.Errorf("lookup content fingerprint on %q: %w", destination, err)
	}
	return verified != 0, nil
}

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

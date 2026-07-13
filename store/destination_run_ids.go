package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Verification methods recorded on a durability component's
// VerifyMethod. They name the comparison that last advanced the
// component, so the offload gate can require a genuinely content-checked
// method before it deletes the only local copy. sync re-exports these as
// its VerifyMethod* identifiers, keeping one source of truth.
const (
	// VerifyMethodBlake3 is rclone's end-to-end content check
	// (--checksum --hash blake3).
	VerifyMethodBlake3 = "blake3"
	// VerifyMethodSizeMtime is rclone's default size+mtime comparison,
	// used for --shallow runs and forced by crypt destinations. Not a
	// content check.
	VerifyMethodSizeMtime = "size+mtime"
	// VerifyMethodPeer is the node-sync handshake's receiver-side BLAKE3
	// re-hash of every delivered path.
	VerifyMethodPeer = "peer-blake3"
	// VerifyMethodKopia is kopia's own repository verification
	// (`kopia snapshot verify`).
	VerifyMethodKopia = "kopia-verify"
	// VerifyMethodPresenceSize is the content-addressed push's check:
	// presence plus the expected ciphertext size, no content hash. Not a
	// content check on its own — a verified scan-back fingerprint must
	// back the object before such a component gates offload.
	VerifyMethodPresenceSize = "presence+size"
)

// ContentVerifiedMethod reports whether a durability component advanced
// by method carries genuine content verification — the precondition the
// offload gate applies before deleting a local copy. A presence-only or
// size+mtime method is not content-verified; an empty method (a pre-v19
// component, or one whose provenance is unknown) is treated as
// unverified so the gate refuses rather than over-claims.
func ContentVerifiedMethod(method string) bool {
	switch method {
	case VerifyMethodBlake3, VerifyMethodPeer, VerifyMethodKopia:
		return true
	default:
		return false
	}
}

// KnownVerifyMethod reports whether method is one of the defined
// verification-method identifiers above. It exists for the wire
// boundary: a pulled durability component carries its origin's
// VerifyMethod verbatim, and the offload gate later switches on it
// (ContentVerifiedMethod), so an unrecognised non-empty method should be
// refused on receipt rather than stored and silently ignored. The empty
// method (a pre-v19 row, or one whose provenance is unknown) is a
// legitimate "unverified" state and is deliberately excluded here;
// callers that accept it test for "" explicitly.
func KnownVerifyMethod(method string) bool {
	switch method {
	case VerifyMethodBlake3, VerifyMethodSizeMtime, VerifyMethodPeer, VerifyMethodKopia, VerifyMethodPresenceSize:
		return true
	default:
		return false
	}
}

// DestinationRunID is one component of a destination's durability
// version vector: the highest origin-space run id of OriginNodeID's
// content known durable on Destination for VolumeID. Destination is the
// unified target name — a bucket destination or a peer node name, the
// same namespace runs.destination uses. OriginRunID is in the origin
// node's run space, so like contents.origin_run_id it is not a local
// runs FK. Content with origin (N, r) is durable on a destination iff
// the vector's component for N is ≥ r. VerifyMethod names the comparison
// that last advanced the component (empty for a pre-v19 row).
//
// SourceNodeID is the provenance class: NULL when the component was
// advanced by a transfer this node observed itself (the locally-verified
// class, via AdvanceDestinationVectorTo), and the asserting peer's node
// id when a durability pull last advanced it. The offload gate weighs a
// peer-asserted component as a distinct, revocable class.
//
// Provenance is single-hop: the durability responder serves its whole
// vector, including components it itself pulled from a third node, so on a
// multi-hop relay SourceNodeID names the peer this node pulled from — the
// last relay — not the original asserter. Trust is therefore hop-by-hop:
// each node vouches for what it relays, and revoking a peer drops
// everything that peer asserted regardless of where it first originated.
type DestinationRunID struct {
	VolumeID     int64
	Destination  string
	OriginNodeID int64
	OriginRunID  int64
	UpdatedAtNs  int64
	VerifyMethod string
	SourceNodeID sql.NullInt64
}

// DestinationRunIDHistory is one row of the insert-only
// destination_run_ids_history log: a single vector-component advance.
// AtNs is the insertion timestamp; rows written in the same tick still
// order by id. VerifyMethod records the method behind this advance.
// SourceNodeID records the advance's provenance (NULL locally-verified,
// else the asserting peer) so the audit log traces revocation, not just
// the live vector.
type DestinationRunIDHistory struct {
	ID           int64
	VolumeID     int64
	Destination  string
	OriginNodeID int64
	OriginRunID  int64
	AtNs         int64
	VerifyMethod string
	SourceNodeID sql.NullInt64
}

// DestinationRewindError carries the rejected and current vector
// components when UpsertDestinationRunID refuses a backwards move. It
// wraps ErrWatermarkRewind so errors.Is matches the shared sentinel.
type DestinationRewindError struct {
	VolumeID     int64
	Destination  string
	OriginNodeID int64
	Current      int64
	Attempted    int64
}

func (e *DestinationRewindError) Error() string {
	return fmt.Sprintf(
		"destination %q watermark for volume %d origin node %d would move backwards from %d to %d; pass allowRewind to override",
		e.Destination, e.VolumeID, e.OriginNodeID, e.Current, e.Attempted)
}

func (e *DestinationRewindError) Unwrap() error { return ErrWatermarkRewind }

// GetDestinationRunID returns one vector component, or sql.ErrNoRows
// when the destination has never recorded durability for content
// originating at originNodeID. "No row" imposes no floor: any origin
// run id advances from it.
func (s *Store) GetDestinationRunID(ctx context.Context, volumeID int64, destination string, originNodeID int64) (DestinationRunID, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT volume_id, destination, origin_node_id, origin_run_id, updated_at_ns, verify_method, source_node_id
		 FROM destination_run_ids
		 WHERE volume_id = ? AND destination = ? AND origin_node_id = ?`,
		volumeID, destination, originNodeID)
	return scanDestinationRunID(row)
}

// ListDestinationRunIDs returns the full durability vector for one
// (volume, destination), ordered by origin node id. An empty slice
// means the destination has no recorded durability yet.
func (s *Store) ListDestinationRunIDs(ctx context.Context, volumeID int64, destination string) ([]DestinationRunID, error) {
	return queryRows(ctx, s.db,
		`SELECT volume_id, destination, origin_node_id, origin_run_id, updated_at_ns, verify_method, source_node_id
		 FROM destination_run_ids
		 WHERE volume_id = ? AND destination = ?
		 ORDER BY origin_node_id`,
		scanDestinationRunID, volumeID, destination)
}

// ListVolumeDestinationRunIDs returns every recorded vector component
// for the volume across all destinations, ordered by destination then
// origin node id. The peer durability endpoint serves this listing so
// a peer can hold offline evidence about destinations only this node
// can see.
func (s *Store) ListVolumeDestinationRunIDs(ctx context.Context, volumeID int64) ([]DestinationRunID, error) {
	return queryRows(ctx, s.db,
		`SELECT volume_id, destination, origin_node_id, origin_run_id, updated_at_ns, verify_method, source_node_id
		 FROM destination_run_ids
		 WHERE volume_id = ?
		 ORDER BY destination, origin_node_id`,
		scanDestinationRunID, volumeID)
}

func scanDestinationRunID(s rowScanner) (DestinationRunID, error) {
	var d DestinationRunID
	var method sql.NullString
	err := s.Scan(&d.VolumeID, &d.Destination, &d.OriginNodeID, &d.OriginRunID, &d.UpdatedAtNs, &method, &d.SourceNodeID)
	d.VerifyMethod = method.String
	return d, err
}

// AdvanceDestinationVectorTo advances the destination's durability
// vector to exactly the supplied components, tagging each with
// verifyMethod. Callers compute the components once from the push's own
// enumeration snapshot (a content-addressed delta, a peer plan, a
// pre-transfer listing) so the advance reflects only what was actually
// transferred — never a wider live set re-read after the transfer, which
// would claim durability for rows committed mid-push. Each component
// routes through the monotonic upsert; an attempted rewind is skipped
// (the recorded floor already covers it). This is the single
// advancement path the destination handlers and the peer-sync initiator
// use rather than writing components directly.
func (s *Store) AdvanceDestinationVectorTo(ctx context.Context, volumeID int64, destination, verifyMethod string, components []OriginComponent) error {
	for _, c := range components {
		err := s.upsertDestinationRunID(ctx, volumeID, destination, c.OriginNodeID, c.OriginRunID, verifyMethod, sql.NullInt64{}, false)
		if errors.Is(err, ErrWatermarkRewind) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return s.recordPushFreshness(ctx, volumeID, destination, components)
}

// recordPushFreshness overwrites the destination's push-freshness maxima
// to exactly the supplied snapshot — the per-origin-node maxima of the
// present set this push enumerated. Distinct from the monotonic vector
// advance above: freshness reflects only the latest push, so a push that
// dropped content from the present set lowers the maxima. The offload
// gate reads it as origin-space freshness for a relayed target.
//
// A node absent from the snapshot keeps its prior freshness row: the push
// enumerated no present content for that origin, which says nothing about
// whether that node's earlier content stopped being fresh, so leaving the
// row is the conservative choice (the monotonic vector still governs
// durability).
func (s *Store) recordPushFreshness(ctx context.Context, volumeID int64, destination string, components []OriginComponent) error {
	for _, c := range components {
		if err := s.UpsertDestinationPushFreshness(ctx, volumeID, destination, c.OriginNodeID, c.OriginRunID); err != nil {
			return err
		}
	}
	return nil
}

// OriginComponent is one (origin node, max origin run) pair computed
// over a volume's present rows by PresentOriginMaxima.
type OriginComponent struct {
	OriginNodeID int64
	OriginRunID  int64
}

// PresentOriginMaxima computes the per-origin-node maximum origin run
// over the volume's present files, deduplicated to one coordinate per
// content. Content whose origin is NULL (or partially NULL — degraded
// the same way the conflict pre-stage treats partial provenance) maps
// to selfNodeID at its introduction run, mirroring
// ContentIntroductionRunID (the introduction MIN spans every
// observation of the content, any status). The reserved sync subtrees
// are excluded from the present set — they never travel to a
// destination, so they must not advance its evidence.
//
// Handlers capture this snapshot before the transfer and feed it to
// AdvanceDestinationVectorTo, so a row committed between the snapshot and
// the advance is never claimed durable.
func (s *Store) PresentOriginMaxima(ctx context.Context, volumeID, selfNodeID int64) ([]OriginComponent, error) {
	return queryRows(ctx, s.db, `
		WITH present_contents AS (
			SELECT DISTINCT f.content_id, c.origin_node_id, c.origin_run_id
			FROM files f
			JOIN folders fo ON fo.id = f.folder_id
			JOIN contents c ON c.id = f.content_id
			WHERE fo.volume_id = ? AND f.status = 'present'
			  AND `+reservedSubtreeFilter+`
		)
		SELECT
			CASE WHEN pc.origin_node_id IS NULL OR pc.origin_run_id IS NULL
			     THEN ? ELSE pc.origin_node_id END AS origin_node,
			MAX(CASE WHEN pc.origin_node_id IS NULL OR pc.origin_run_id IS NULL
			     THEN (SELECT MIN(f2.first_seen_run_id)
			           FROM files f2
			           JOIN folders fo2 ON fo2.id = f2.folder_id
			           WHERE fo2.volume_id = ? AND f2.content_id = pc.content_id)
			     ELSE pc.origin_run_id END) AS origin_run
		FROM present_contents pc
		GROUP BY origin_node
		ORDER BY origin_node
	`, scanOriginComponent, volumeID, selfNodeID, volumeID)
}

func scanOriginComponent(s rowScanner) (OriginComponent, error) {
	var c OriginComponent
	err := s.Scan(&c.OriginNodeID, &c.OriginRunID)
	return c, err
}

// UpsertDestinationRunID advances one component of a destination's
// durability vector to originRunID, recording no verification method
// (the component reads as unverified to the offload gate until a typed
// advance re-stamps it). Callers invoke it only once the destination has
// verifiably landed every piece of content up to that origin run — a
// failed or partial push leaves the prior value in place.
//
// The component is meant to advance monotonically: the upsert statement
// itself only applies when originRunID is at or above the recorded value
// (or allowRewind is set — the opt-in for genuine recovery, mirroring
// UpsertPeerSyncState), so a racing writer cannot regress the vector. A
// refused rewind surfaces as a *DestinationRewindError (wrapping
// ErrWatermarkRewind).
//
// The upsert and an insert-only destination_run_ids_history row are
// written in one transaction so the append-only advance log can never
// diverge from the live vector.
//
// verify_method follows the component: a non-empty method always wins
// (an advance or a re-confirmation that upgrades the recorded method); an
// empty method clears it when the run strictly advances (a new,
// unverified coordinate) but preserves the existing method when the run
// is unchanged, so a methodless re-confirmation (e.g. a pull from a
// pre-v19 peer) never degrades a content-verified component to unknown.
func (s *Store) UpsertDestinationRunID(ctx context.Context, volumeID int64, destination string, originNodeID, originRunID int64, allowRewind bool) error {
	return s.upsertDestinationRunID(ctx, volumeID, destination, originNodeID, originRunID, "", sql.NullInt64{}, allowRewind)
}

// UpsertDestinationRunIDVerified advances a component this node verified
// itself, recording verifyMethod and leaving source_node_id NULL (the
// locally-verified provenance class). The durability pull does not use
// it — peer-asserted advances go through UpsertDestinationRunIDPulled so
// the two classes stay distinguishable and a peer's assertions revocable.
func (s *Store) UpsertDestinationRunIDVerified(ctx context.Context, volumeID int64, destination string, originNodeID, originRunID int64, verifyMethod string, allowRewind bool) error {
	return s.upsertDestinationRunID(ctx, volumeID, destination, originNodeID, originRunID, verifyMethod, sql.NullInt64{}, allowRewind)
}

// UpsertDestinationRunIDPulled records a peer-pulled advance: it carries
// the origin's reported verify method verbatim (so the puller's offload
// gate weighs the component exactly as the responder did) and tags the
// component with sourceNodeID, the asserting peer. This is the only
// entry point that stamps non-local provenance; every locally-verified
// path leaves source_node_id NULL, so revocation can drop a peer's
// assertions without touching evidence this node observed itself.
func (s *Store) UpsertDestinationRunIDPulled(ctx context.Context, volumeID int64, destination string, originNodeID, originRunID int64, verifyMethod string, sourceNodeID int64, allowRewind bool) error {
	return s.upsertDestinationRunID(ctx, volumeID, destination, originNodeID, originRunID, verifyMethod, sql.NullInt64{Int64: sourceNodeID, Valid: true}, allowRewind)
}

// upsertDestinationRunID is the shared advance. sourceNodeID carries
// provenance: an invalid (NULL) value is the locally-verified class, a
// valid value the asserting peer. source_node_id tracks the writer of the
// verify_method the row ends up recording, so the (method, provenance)
// pair always describes a single write — a peer's verification can never
// come to rest under local provenance, nor a local one under a peer:
//
//   - a strict run advance adopts the incoming provenance: new coverage
//     belongs to whoever proved it;
//   - an equal-run write that changes the recorded method adopts the
//     incoming provenance with it, so a peer upgrading the method (e.g.
//     presence+size → blake3) is tagged, and revocable, as that peer;
//   - a local (NULL-source) write carrying a method reclaims the component
//     to local, so a verified push takes back evidence a peer had asserted;
//   - otherwise provenance is preserved: a peer merely re-confirming the
//     method this node already holds never steals ownership of
//     locally-verified evidence (which would make it revocable), and a
//     methodless touch changes nothing.
func (s *Store) upsertDestinationRunID(ctx context.Context, volumeID int64, destination string, originNodeID, originRunID int64, verifyMethod string, sourceNodeID sql.NullInt64, allowRewind bool) error {
	if destination == "" {
		return fmt.Errorf("UpsertDestinationRunID: destination must be non-empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin upsert destination_run_ids: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	atNs := NowNs()
	method := nullableString(verifyMethod)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO destination_run_ids (volume_id, destination, origin_node_id, origin_run_id, updated_at_ns, verify_method, source_node_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(volume_id, destination, origin_node_id) DO UPDATE SET
			origin_run_id = excluded.origin_run_id,
			updated_at_ns = excluded.updated_at_ns,
			verify_method = CASE
				WHEN excluded.verify_method IS NOT NULL THEN excluded.verify_method
				WHEN excluded.origin_run_id > destination_run_ids.origin_run_id THEN NULL
				ELSE destination_run_ids.verify_method
			END,
			source_node_id = CASE
				WHEN excluded.origin_run_id > destination_run_ids.origin_run_id THEN excluded.source_node_id
				WHEN excluded.verify_method IS NOT NULL
				     AND excluded.verify_method IS NOT destination_run_ids.verify_method THEN excluded.source_node_id
				WHEN excluded.source_node_id IS NULL AND excluded.verify_method IS NOT NULL THEN NULL
				ELSE destination_run_ids.source_node_id
			END
		WHERE excluded.origin_run_id >= destination_run_ids.origin_run_id OR ?
	`, volumeID, destination, originNodeID, originRunID, atNs, method, sourceNodeID, allowRewind)
	if err != nil {
		return fmt.Errorf("upsert destination_run_ids: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("upsert destination_run_ids rows: %w", err)
	}
	if n == 0 {
		if err := guardDestinationMonotonicTx(ctx, tx, volumeID, destination, originNodeID, originRunID); err != nil {
			return err
		}
		return fmt.Errorf("upsert destination_run_ids: conditional update applied no row for (%d, %q, %d)", volumeID, destination, originNodeID)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO destination_run_ids_history
			(volume_id, destination, origin_node_id, origin_run_id, at_ns, verify_method, source_node_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, volumeID, destination, originNodeID, originRunID, atNs, method, sourceNodeID); err != nil {
		return fmt.Errorf("append destination_run_ids_history: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit upsert destination_run_ids: %w", err)
	}
	return nil
}

// nullableString maps "" to a SQL NULL so an unset verify method stays
// NULL rather than an empty string the gate would have to special-case.
func nullableString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// guardDestinationMonotonicTx reads the current vector component inside
// tx and returns a *DestinationRewindError when attempted is strictly
// below it. No row imposes no floor. Called after the conditional
// upsert applied nothing, to turn the refusal into a precise error.
func guardDestinationMonotonicTx(ctx context.Context, tx *sql.Tx, volumeID int64, destination string, originNodeID, attempted int64) error {
	var current int64
	err := tx.QueryRowContext(ctx,
		`SELECT origin_run_id FROM destination_run_ids
		 WHERE volume_id = ? AND destination = ? AND origin_node_id = ?`,
		volumeID, destination, originNodeID).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("read current destination run id: %w", err)
	}
	if attempted < current {
		return &DestinationRewindError{
			VolumeID:     volumeID,
			Destination:  destination,
			OriginNodeID: originNodeID,
			Current:      current,
			Attempted:    attempted,
		}
	}
	return nil
}

// ListDestinationRunIDHistory returns every advance recorded for the
// (volume, destination) pair across all origin nodes, oldest first
// (ascending id, which is insertion order). An empty slice means no
// recorded advances.
func (s *Store) ListDestinationRunIDHistory(ctx context.Context, volumeID int64, destination string) ([]DestinationRunIDHistory, error) {
	return queryRows(ctx, s.db, `
		SELECT id, volume_id, destination, origin_node_id, origin_run_id, at_ns, verify_method, source_node_id
		FROM destination_run_ids_history
		WHERE volume_id = ? AND destination = ?
		ORDER BY id
	`, scanDestinationRunIDHistory, volumeID, destination)
}

func scanDestinationRunIDHistory(s rowScanner) (DestinationRunIDHistory, error) {
	var h DestinationRunIDHistory
	var method sql.NullString
	err := s.Scan(&h.ID, &h.VolumeID, &h.Destination, &h.OriginNodeID, &h.OriginRunID, &h.AtNs, &method, &h.SourceNodeID)
	h.VerifyMethod = method.String
	return h, err
}

// RevokeDestinationRunIDsFromSource removes the live durability-vector
// components a single peer asserted, returning how many it deleted. It is
// the operator's recovery path for a compromised or mistaken peer: pulled
// evidence (source_node_id = sourceNodeID) is dropped while
// locally-verified components (source_node_id NULL) and other peers'
// assertions stay, so the live verified vector is never rewound. The
// append-only destination_run_ids_history is untouched — the assertions
// remain in the audit trail, so revocation is a forward act, not a
// rewrite of history. A revoked component reverts to "no row" (no floor);
// a later legitimate pull or verified push re-advances it.
func (s *Store) RevokeDestinationRunIDsFromSource(ctx context.Context, sourceNodeID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM destination_run_ids WHERE source_node_id = ?`, sourceNodeID)
	if err != nil {
		return 0, fmt.Errorf("revoke destination_run_ids from source %d: %w", sourceNodeID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("revoke destination_run_ids rows: %w", err)
	}
	return n, nil
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Pack is one immutable tar.zst pack the packed layout assembled and
// uploaded: a run's bundle of small content, addressed on the destination
// by the hex of PackKey (the BLAKE3 of the compressed plaintext). SizeBytes
// is the uploaded (compressed) size, MemberCount the number of content
// entries in the pack's uncompressed tar, and CreatedRunID the sync run
// that produced it. A pack row is written once and never rewritten.
type Pack struct {
	ID           int64
	PackKey      []byte // raw 32-byte BLAKE3 of the compressed pack bytes
	SizeBytes    int64
	MemberCount  int64
	CreatedRunID int64
}

// PackMember locates one content inside a pack's uncompressed tar:
// ByteOffset is where the member's data begins and ByteLength its size, so
// a reader that decompresses the pack can slice the content back out
// without stock tar. The PRIMARY KEY on content_id enforces that a given
// content is packed exactly once — its offset/length can never become
// ambiguous — so the packed writer relies on this to never re-pack.
type PackMember struct {
	ContentID  int64
	PackID     int64
	ByteOffset int64
	ByteLength int64
}

// PackWrite bundles a pack row with its members for one atomic insert.
// The members' PackID is filled from the freshly inserted pack id.
type PackWrite struct {
	Pack    Pack
	Members []PackMember
}

// InsertPacks records the run's assembled packs and their members in a
// single transaction: either every pack row and member row lands or none
// does, so a mid-insert failure never leaves a pack whose members are only
// half-recorded (which would make some content look packed and some not).
// The packed writer calls this only after every pack, the placement map,
// and the manifest segment have confirmed landing at the destination, so a
// recorded pack_members row always has its bytes offsite. Content is packed
// at most once (the pack_members primary key on content_id), so a duplicate
// content fails the insert rather than silently repointing it.
func (s *Store) InsertPacks(ctx context.Context, writes []PackWrite) error {
	if len(writes) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pack insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, w := range writes {
		if len(w.Pack.PackKey) != 32 {
			return fmt.Errorf("insert pack: pack_key must be 32 bytes, got %d", len(w.Pack.PackKey))
		}
		// Derive member_count from the members actually inserted rather than
		// trusting the caller's field, so the stored count can never drift
		// from the pack_members rows that reference this pack.
		res, err := tx.ExecContext(ctx, `
			INSERT INTO packs (pack_key, size_bytes, member_count, created_run_id)
			VALUES (?, ?, ?, ?)
		`, w.Pack.PackKey, w.Pack.SizeBytes, int64(len(w.Members)), w.Pack.CreatedRunID)
		if err != nil {
			return fmt.Errorf("insert pack: %w", err)
		}
		packID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("insert pack id: %w", err)
		}
		for _, m := range w.Members {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO pack_members (content_id, pack_id, byte_offset, byte_length)
				VALUES (?, ?, ?, ?)
			`, m.ContentID, packID, m.ByteOffset, m.ByteLength); err != nil {
				return fmt.Errorf("insert pack member content %d: %w", m.ContentID, err)
			}
		}
	}
	return tx.Commit()
}

// HasPackMember reports whether the content is already packed on some pack.
// The packed writer uses it as its pack-once gate: packing is
// destination-independent (a pack's bytes are content-global — see
// remote_packs for the per-destination upload record), so content recorded
// as packed is never re-packed on any later run or for any other packed
// destination.
func (s *Store) HasPackMember(ctx context.Context, contentID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM pack_members WHERE content_id = ?
	`, contentID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lookup pack member: %w", err)
	}
	return true, nil
}

// GetPackMember returns the placement of one content, or sql.ErrNoRows when
// the content is not packed.
func (s *Store) GetPackMember(ctx context.Context, contentID int64) (PackMember, error) {
	var m PackMember
	err := s.db.QueryRowContext(ctx, `
		SELECT content_id, pack_id, byte_offset, byte_length
		FROM pack_members WHERE content_id = ?
	`, contentID).Scan(&m.ContentID, &m.PackID, &m.ByteOffset, &m.ByteLength)
	return m, err
}

// GetPackByKey returns the pack row for a pack key, or sql.ErrNoRows when
// no pack with that key was recorded.
func (s *Store) GetPackByKey(ctx context.Context, packKey []byte) (Pack, error) {
	var p Pack
	err := s.db.QueryRowContext(ctx, `
		SELECT id, pack_key, size_bytes, member_count, created_run_id
		FROM packs WHERE pack_key = ?
	`, packKey).Scan(&p.ID, &p.PackKey, &p.SizeBytes, &p.MemberCount, &p.CreatedRunID)
	return p, err
}

// GetPack returns the pack row for a pack id, or sql.ErrNoRows when no
// pack with that id was recorded. Restore uses it to resolve a
// pack_members row's pack_id back to the pack_key that names the pack at
// the destination.
func (s *Store) GetPack(ctx context.Context, id int64) (Pack, error) {
	var p Pack
	err := s.db.QueryRowContext(ctx, `
		SELECT id, pack_key, size_bytes, member_count, created_run_id
		FROM packs WHERE id = ?
	`, id).Scan(&p.ID, &p.PackKey, &p.SizeBytes, &p.MemberCount, &p.CreatedRunID)
	return p, err
}

// ListPackMembers returns every member of one pack, ordered by byte offset
// so the enumeration follows the pack's uncompressed tar layout.
func (s *Store) ListPackMembers(ctx context.Context, packID int64) ([]PackMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT content_id, pack_id, byte_offset, byte_length
		FROM pack_members WHERE pack_id = ?
		ORDER BY byte_offset
	`, packID)
	if err != nil {
		return nil, fmt.Errorf("list pack members for %d: %w", packID, err)
	}
	defer rows.Close()
	var out []PackMember
	for rows.Next() {
		var m PackMember
		if err := rows.Scan(&m.ContentID, &m.PackID, &m.ByteOffset, &m.ByteLength); err != nil {
			return nil, fmt.Errorf("scan pack member row: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

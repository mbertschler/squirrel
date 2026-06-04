package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
)

// FolderHashDivergence records one folder whose stored Merkle hash does
// not match the value re-derived from its current file and child rows.
// Which is set tells the caller whether the shallow digest, the deep
// digest, or both diverged; the Stored/Derived byte slices carry the
// 32-byte digests for display. A non-empty slice from CheckFolderHashes
// is the volume's self-check failure set.
type FolderHashDivergence struct {
	Path            string
	ShallowDiverged bool
	DeepDiverged    bool
	StoredShallow   []byte
	DerivedShallow  []byte
	StoredDeep      []byte
	DerivedDeep     []byte
}

// CheckFolderHashes re-derives every folder's shallow and deep BLAKE3 for
// the given volume from the current file/child rows and compares each to
// the stored shallow_blake3/deep_blake3, returning one
// FolderHashDivergence per folder that disagrees (empty slice when the
// volume is internally consistent).
//
// The derivation reuses the same computeShallow/computeDeep helpers the
// steady-state recompute walk uses (so hashing is not reimplemented); it
// runs read-only inside one transaction and writes nothing. The deep
// recomputation folds each child's *stored* deep digest — so a single
// corrupted folder surfaces both at that folder (its own deep no longer
// matches its files+children) and at its parent (whose stored deep was
// folded from the pre-corruption child), which is the desired "report
// every divergence" behaviour for an integrity audit.
func (s *Store) CheckFolderHashes(ctx context.Context, volumeID int64) ([]FolderHashDivergence, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin folder hash check: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	folders, err := listVolumeFolderHashesTx(ctx, tx, volumeID)
	if err != nil {
		return nil, err
	}
	var diverged []FolderHashDivergence
	for _, f := range folders {
		d, err := checkOneFolderHash(ctx, tx, f)
		if err != nil {
			return nil, err
		}
		if d != nil {
			diverged = append(diverged, *d)
		}
	}
	return diverged, nil
}

// folderHashRow is the stored (id, path, shallow, deep) of one folder,
// the input to the per-folder comparison.
type folderHashRow struct {
	id      int64
	path    string
	shallow []byte
	deep    []byte
}

func listVolumeFolderHashesTx(ctx context.Context, tx *sql.Tx, volumeID int64) ([]folderHashRow, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, path, shallow_blake3, deep_blake3 FROM folders WHERE volume_id = ?`,
		volumeID)
	if err != nil {
		return nil, fmt.Errorf("list folders for volume %d: %w", volumeID, err)
	}
	defer rows.Close()
	var out []folderHashRow
	for rows.Next() {
		var f folderHashRow
		if err := rows.Scan(&f.id, &f.path, &f.shallow, &f.deep); err != nil {
			return nil, fmt.Errorf("scan folder hash row: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// checkOneFolderHash re-derives one folder's shallow and deep digests and
// returns a *FolderHashDivergence when either differs from stored, or nil
// when both match. Derivation reuses the steady-state recompute helpers.
func checkOneFolderHash(ctx context.Context, tx *sql.Tx, f folderHashRow) (*FolderHashDivergence, error) {
	shallow, _, _, err := computeShallowAndDirectAggregatesTx(ctx, tx, f.id)
	if err != nil {
		return nil, err
	}
	deep, _, _, err := computeDeepAndChildAggregatesTx(ctx, tx, f.id, shallow)
	if err != nil {
		return nil, err
	}
	shallowDiverged := !bytes.Equal(shallow, f.shallow)
	deepDiverged := !bytes.Equal(deep, f.deep)
	if !shallowDiverged && !deepDiverged {
		return nil, nil
	}
	return &FolderHashDivergence{
		Path:            f.path,
		ShallowDiverged: shallowDiverged,
		DeepDiverged:    deepDiverged,
		StoredShallow:   f.shallow,
		DerivedShallow:  shallow,
		StoredDeep:      f.deep,
		DerivedDeep:     deep,
	}, nil
}

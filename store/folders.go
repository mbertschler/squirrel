package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/zeebo/blake3"
)

// folderHashContext domain-separates folder Merkle hashes from file content
// hashes (which are unkeyed BLAKE3). Bumping the version string lets us
// evolve the canonical input format without ambiguity — every existing
// folder digest is implicitly invalidated and a recompute pass is required.
const folderHashContext = "squirrel-folder-v1"

// folderHashKey is the 32-byte key seeded once at package init. blake3 keyed
// hashing with this key produces digests that cannot collide with raw
// file-content BLAKE3 digests stored on files.blake3, even if the
// observable bytes coincide.
var folderHashKey [32]byte

func init() {
	blake3.DeriveKey(folderHashContext, nil, folderHashKey[:])
}

// Folder is a directory row. Path is the volume-relative folder path with no
// trailing slash; the volume root is the empty string. ParentID is NULL only
// for the root. ShallowBlake3 and DeepBlake3 are 32-byte keyed BLAKE3 digests
// over canonical inputs (see ComputeShallowHash / ComputeDeepHash). They are
// nullable in storage so the migration can seed rows in two passes, but in
// steady state on a fully-seeded volume they are always non-NULL — the empty
// folder hash is well-defined (keyed hash of the empty input).
type Folder struct {
	ID               int64
	VolumeID         int64
	ParentID         sql.NullInt64
	Path             string
	ShallowBlake3    []byte
	DeepBlake3       []byte
	LastChangedRunID sql.NullInt64
}

// FolderName returns the last path segment of the folder, i.e. its name
// relative to its parent. The volume root returns "".
func (f Folder) Name() string {
	return folderName(f.Path)
}

func folderName(path string) string {
	if path == "" {
		return ""
	}
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// parentFolderPath returns the path of the parent of the folder at p. The
// root's parent path is "" too — callers must check for root before
// recursing.
func parentFolderPath(p string) string {
	if p == "" {
		return ""
	}
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// splitFilePath splits a volume-relative file path into (folderPath, name).
// Root-level files yield folderPath="". Trailing or leading slashes are not
// expected (the indexer normalises paths via filepath.ToSlash on relative
// outputs); a leading slash would slip through but produces a folderPath
// containing that slash, which is fine — the row keys off folder_id not
// path content.
func splitFilePath(p string) (folderPath, name string) {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}

// pathFromFolderAndName is the SQL CASE expression that reconstructs a
// file's full volume-relative path from a folders↔files JOIN. Used in every
// SELECT that returns FileRow so the public Path field stays populated even
// though the underlying storage is (folder_id, name).
const pathFromFolderAndName = `CASE fo.path WHEN '' THEN f.name ELSE fo.path || '/' || f.name END`

// GetOrCreateFolder returns the folder id at (volumeID, path), creating any
// missing ancestor folders up to the volume root. Inputs are expected to be
// the volume-relative directory path with no trailing slash; the root is "".
//
// The call runs in its own transaction. Callers that need to atomically
// pair the lookup with a write (Upsert does) should use the tx-scoped form
// getOrCreateFolderTx instead.
func (s *Store) GetOrCreateFolder(ctx context.Context, volumeID int64, path string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin folder upsert: %w", err)
	}
	defer tx.Rollback()
	id, err := getOrCreateFolderTx(ctx, tx, volumeID, path)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit folder upsert: %w", err)
	}
	return id, nil
}

// getOrCreateFolderTx is the tx-scoped form. Recursively materialises every
// ancestor up to the root so the folders table always carries a chain from
// any leaf back to the volume root. Each newly created folder gets its
// shallow_blake3 seeded to the empty-input keyed hash; deep_blake3 is left
// NULL and gets populated lazily by recomputeFolderAndAncestors after the
// first file lands.
func getOrCreateFolderTx(ctx context.Context, tx *sql.Tx, volumeID int64, path string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM folders WHERE volume_id = ? AND path = ?`,
		volumeID, path).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !IsNotFound(err) {
		return 0, fmt.Errorf("lookup folder: %w", err)
	}

	var parentID sql.NullInt64
	if path != "" {
		pid, err := getOrCreateFolderTx(ctx, tx, volumeID, parentFolderPath(path))
		if err != nil {
			return 0, err
		}
		parentID = sql.NullInt64{Int64: pid, Valid: true}
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO folders (volume_id, parent_id, path, shallow_blake3, deep_blake3)
		 VALUES (?, ?, ?, NULL, NULL)`,
		volumeID, parentID, path)
	if err != nil {
		return 0, fmt.Errorf("insert folder: %w", err)
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	return id, nil
}

// GetFolderByPath returns the folder row at (volumeID, path). Returns
// sql.ErrNoRows when no such folder exists.
func (s *Store) GetFolderByPath(ctx context.Context, volumeID int64, path string) (Folder, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, volume_id, parent_id, path, shallow_blake3, deep_blake3, last_changed_run_id
		 FROM folders WHERE volume_id = ? AND path = ?`,
		volumeID, path)
	return scanFolder(row)
}

// ListChildFolders returns the direct subfolders of parentID, ordered
// by full path ascending — which, because the WHERE clause restricts to
// one parent, is the same byte-wise order the Merkle walk's deep-hash
// reconstruction expects (children of a single parent share the same
// prefix, so sorting by path is identical to sorting by name). Returns
// the empty slice when parentID has no children.
func (s *Store) ListChildFolders(ctx context.Context, parentID int64) ([]Folder, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, volume_id, parent_id, path, shallow_blake3, deep_blake3, last_changed_run_id
		 FROM folders WHERE parent_id = ?
		 ORDER BY path`,
		parentID)
	if err != nil {
		return nil, fmt.Errorf("list child folders: %w", err)
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.VolumeID, &f.ParentID, &f.Path, &f.ShallowBlake3, &f.DeepBlake3, &f.LastChangedRunID); err != nil {
			return nil, fmt.Errorf("scan child folder: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFolderByID returns the folder row by primary key.
func (s *Store) GetFolderByID(ctx context.Context, id int64) (Folder, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, volume_id, parent_id, path, shallow_blake3, deep_blake3, last_changed_run_id
		 FROM folders WHERE id = ?`,
		id)
	return scanFolder(row)
}

func scanFolder(row *sql.Row) (Folder, error) {
	var f Folder
	err := row.Scan(&f.ID, &f.VolumeID, &f.ParentID, &f.Path, &f.ShallowBlake3, &f.DeepBlake3, &f.LastChangedRunID)
	return f, err
}

// ComputeShallowHash is the canonical shallow_blake3 over the live direct
// file children of one folder, exported so tests can verify the stored
// digest against an independent recomputation. The (name, blake3) pairs
// must be sorted ascending by name (byte-wise) before being fed in;
// callers that don't already have a sorted slice should sort first.
func ComputeShallowHash(entries []ShallowEntry) []byte {
	h, _ := blake3.NewKeyed(folderHashKey[:])
	var lenBuf [4]byte
	// blake3.Hasher.Write never returns an error (it accumulates into a
	// fixed-size sponge), but errcheck flags the discarded result anyway.
	// Bind to blank to make the discard explicit.
	for _, e := range entries {
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(e.Name)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write([]byte(e.Name))
		_, _ = h.Write(e.Blake3)
	}
	return h.Sum(nil)
}

// ShallowEntry is one (name, content-blake3) pair contributing to a
// folder's shallow hash.
type ShallowEntry struct {
	Name   string
	Blake3 []byte
}

// ComputeDeepHash folds the shallow hash and the deep hashes of direct
// child folders into the deep_blake3 digest. Children must be sorted
// ascending by name before being fed in.
func ComputeDeepHash(shallow []byte, children []ChildFolder) []byte {
	h, _ := blake3.NewKeyed(folderHashKey[:])
	_, _ = h.Write(shallow)
	var lenBuf [4]byte
	for _, c := range children {
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(c.Name)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write([]byte(c.Name))
		_, _ = h.Write(c.DeepBlake3)
	}
	return h.Sum(nil)
}

// ChildFolder is one direct subfolder's contribution to its parent's deep
// hash.
type ChildFolder struct {
	Name       string
	DeepBlake3 []byte
}

// recomputeFolderAndAncestors recomputes shallow and deep hashes for the
// given folder, then re-folds the deep hash of every ancestor up to the
// volume root. Stamps last_changed_run_id on every touched folder so a
// later drift detector (#17) can locate "folders whose hashes changed in
// run N" without a separate audit column.
//
// runID is recorded as NULL when zero (e.g. during migration backfill,
// where no run id is meaningful). The caller is expected to pass a real
// run id during normal Upsert flow.
func recomputeFolderAndAncestors(ctx context.Context, tx *sql.Tx, folderID int64, runID int64) error {
	for folderID != 0 {
		var parentID sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT parent_id FROM folders WHERE id = ?`, folderID).Scan(&parentID); err != nil {
			return fmt.Errorf("lookup folder %d: %w", folderID, err)
		}
		shallow, err := computeShallowForFolderTx(ctx, tx, folderID)
		if err != nil {
			return err
		}
		deep, err := computeDeepForFolderTx(ctx, tx, folderID, shallow)
		if err != nil {
			return err
		}
		if err := writeFolderHashesTx(ctx, tx, folderID, shallow, deep, runID); err != nil {
			return err
		}
		if !parentID.Valid {
			return nil
		}
		folderID = parentID.Int64
	}
	return nil
}

// computeShallowForFolderTx reads the live direct file children of one
// folder inside the open transaction and returns the canonical shallow
// digest. An empty folder yields the keyed hash of the empty input — a
// well-defined value, never NULL.
func computeShallowForFolderTx(ctx context.Context, tx *sql.Tx, folderID int64) ([]byte, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT name, blake3 FROM files
		 WHERE folder_id = ? AND status = 'present'
		 ORDER BY name`,
		folderID)
	if err != nil {
		return nil, fmt.Errorf("read folder %d files: %w", folderID, err)
	}
	defer rows.Close()
	var entries []ShallowEntry
	for rows.Next() {
		var e ShallowEntry
		if err := rows.Scan(&e.Name, &e.Blake3); err != nil {
			return nil, fmt.Errorf("scan folder %d files: %w", folderID, err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ComputeShallowHash(entries), nil
}

// computeDeepForFolderTx folds shallow with the deep hashes of direct
// child folders inside the open transaction. Children that still carry a
// NULL deep_blake3 (uninitialised on this volume — e.g. a fresh ancestor
// row whose subtree has no files yet) are folded with the empty-input
// keyed hash so the input is always 32 bytes and the parent hash stays
// stable.
func computeDeepForFolderTx(ctx context.Context, tx *sql.Tx, folderID int64, shallow []byte) ([]byte, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT path, deep_blake3 FROM folders
		 WHERE parent_id = ?
		 ORDER BY path`,
		folderID)
	if err != nil {
		return nil, fmt.Errorf("read child folders of %d: %w", folderID, err)
	}
	defer rows.Close()
	var children []ChildFolder
	emptyDeep := ComputeShallowHash(nil)
	for rows.Next() {
		var path string
		var deep []byte
		if err := rows.Scan(&path, &deep); err != nil {
			return nil, fmt.Errorf("scan child folders of %d: %w", folderID, err)
		}
		if deep == nil {
			deep = emptyDeep
		}
		children = append(children, ChildFolder{Name: folderName(path), DeepBlake3: deep})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ComputeDeepHash(shallow, children), nil
}

func writeFolderHashesTx(ctx context.Context, tx *sql.Tx, folderID int64, shallow, deep []byte, runID int64) error {
	var runIDArg any
	if runID > 0 {
		runIDArg = runID
	} else {
		runIDArg = nil
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE folders SET shallow_blake3 = ?, deep_blake3 = ?, last_changed_run_id = ?
		 WHERE id = ?`,
		shallow, deep, runIDArg, folderID)
	if err != nil {
		return fmt.Errorf("update folder %d hashes: %w", folderID, err)
	}
	return nil
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
)

// Volume is a named indexing root. The path is an absolute filesystem path;
// it is intentionally not UNIQUE so two volumes can share the same path with
// different (future) filters or modes. The name is UNIQUE and case-sensitive.
type Volume struct {
	ID   int64
	Name string
	Path string
}

// GetOrCreateVolume returns the volume whose path equals absPath, or creates
// a new one with a basename-derived name. On UNIQUE name collision, a numeric
// suffix (-2, -3, ...) is appended until a free name is found.
func (s *Store) GetOrCreateVolume(ctx context.Context, absPath string) (Volume, error) {
	var v Volume
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, path FROM volumes WHERE path = ? ORDER BY id LIMIT 1`, absPath).
		Scan(&v.ID, &v.Name, &v.Path)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Volume{}, fmt.Errorf("lookup volume by path: %w", err)
	}

	base := filepath.Base(absPath)
	const maxAttempts = 1000
	for i := range maxAttempts {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i+1)
		}
		var existingID int64
		err := s.db.QueryRowContext(ctx,
			`SELECT id FROM volumes WHERE name = ?`, name).Scan(&existingID)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Volume{}, fmt.Errorf("lookup volume by name: %w", err)
		}
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO volumes (name, path) VALUES (?, ?)`, name, absPath)
		if err != nil {
			return Volume{}, fmt.Errorf("insert volume: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return Volume{}, fmt.Errorf("volume last insert id: %w", err)
		}
		return Volume{ID: id, Name: name, Path: absPath}, nil
	}
	return Volume{}, fmt.Errorf("could not allocate unique volume name for %q after %d attempts", absPath, maxAttempts)
}

// CreateVolume inserts a new volume row with the given name and absolute
// path. Returns the inserted row. Fails when the name already exists
// (UNIQUE) — callers should look up first via GetVolumeByName and decide
// how to handle that case. Used by the indexer when a config-declared
// volume is indexed for the first time and we want the DB name to match
// the config name exactly (not the basename of the path).
func (s *Store) CreateVolume(ctx context.Context, name, absPath string) (Volume, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO volumes (name, path) VALUES (?, ?)`, name, absPath)
	if err != nil {
		return Volume{}, fmt.Errorf("insert volume %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Volume{}, fmt.Errorf("volume last insert id: %w", err)
	}
	return Volume{ID: id, Name: name, Path: absPath}, nil
}

// GetVolumeByID returns the volume with the given id, or sql.ErrNoRows.
func (s *Store) GetVolumeByID(ctx context.Context, id int64) (Volume, error) {
	var v Volume
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, path FROM volumes WHERE id = ?`, id).
		Scan(&v.ID, &v.Name, &v.Path)
	return v, err
}

// GetVolumeByName returns the volume with the given name, or sql.ErrNoRows.
// Names are UNIQUE per the schema so this lookup is unambiguous.
func (s *Store) GetVolumeByName(ctx context.Context, name string) (Volume, error) {
	var v Volume
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, path FROM volumes WHERE name = ?`, name).
		Scan(&v.ID, &v.Name, &v.Path)
	return v, err
}

// GetVolumeByPath returns the volume whose path equals absPath, or
// sql.ErrNoRows. When multiple volumes share the same path (allowed by the
// schema), the lowest id wins.
func (s *Store) GetVolumeByPath(ctx context.Context, absPath string) (Volume, error) {
	var v Volume
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, path FROM volumes WHERE path = ? ORDER BY id LIMIT 1`, absPath).
		Scan(&v.ID, &v.Name, &v.Path)
	return v, err
}

// ListVolumes returns all volumes ordered by id.
func (s *Store) ListVolumes(ctx context.Context) ([]Volume, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, path FROM volumes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Volume
	for rows.Next() {
		var v Volume
		if err := rows.Scan(&v.ID, &v.Name, &v.Path); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

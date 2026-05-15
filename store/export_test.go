package store

import "context"

// SetSchemaVersion forces a schema version. Test-only helper exposed via the
// _test.go file so it does not appear in the package's public API.
func (s *Store) SetSchemaVersion(ctx context.Context, v int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, v)
	return err
}

package config

import "fmt"

// Backups is the resolved `[backups]` configuration governing the
// snapshot-on-sync feature (#75): after a successful sync, squirrel takes
// a VACUUM INTO snapshot of the index to a local tier and — for
// destination syncs — rides a copy along to the destination bucket so the
// catalog inherits the same redundancy as the data it describes.
//
// Defense-in-depth is the default: an absent `[backups]` table means
// "enabled with the defaults below", and individual keys override only
// what they name. Setting Enabled=false disables both halves; Cloud=false
// keeps the local snapshot but skips the ride-along upload.
type Backups struct {
	// Enabled gates the whole feature — the local snapshot-on-sync and,
	// transitively, the cloud ride-along.
	Enabled bool
	// Dir is the local snapshot directory. Empty means the consumer
	// resolves it to "<dirname(db)>/backups" (the same sibling directory
	// the pre-migration and `db backup` snapshots use); the dependency on
	// the resolved DB path is why the default is applied at the call site
	// rather than here.
	Dir string
	// Keep bounds the local snapshot directory: after writing, the oldest
	// index-* snapshots are rotated away until at most Keep remain. Zero
	// means no rotation. Pre-migration snapshots in the same directory are
	// exempt from this sync-time rotation — only an explicit `db backup
	// --keep` retention removes them.
	Keep int
	// Cloud gates the destination ride-along. Ignored when Enabled is
	// false (no snapshot is taken to upload).
	Cloud bool
	// CloudKeep bounds each destination's per-volume .squirrel-index/
	// directory. Zero means no rotation.
	CloudKeep int
}

// DefaultBackups returns the zero-config defaults: both halves on, seven
// snapshots kept on each tier, local directory resolved by the consumer.
func DefaultBackups() Backups {
	return Backups{Enabled: true, Dir: "", Keep: 7, Cloud: true, CloudKeep: 7}
}

// rawBackups is the on-disk shape of the `[backups]` table. Every field is
// a pointer (or, for Dir, distinguished by emptiness) so resolveBackups
// can tell "key omitted" from "key set to the zero value" — without that,
// `enabled = false` would be indistinguishable from a missing key.
type rawBackups struct {
	Enabled   *bool  `toml:"enabled"`
	Dir       string `toml:"dir"`
	Keep      *int   `toml:"keep"`
	Cloud     *bool  `toml:"cloud"`
	CloudKeep *int   `toml:"cloud_keep"`
}

// resolveBackups folds an optional `[backups]` table over the defaults. A
// nil raw (no table) yields DefaultBackups unchanged. Present keys
// override; Keep and CloudKeep must be non-negative.
func resolveBackups(raw *rawBackups) (Backups, error) {
	b := DefaultBackups()
	if raw == nil {
		return b, nil
	}
	if raw.Enabled != nil {
		b.Enabled = *raw.Enabled
	}
	if raw.Dir != "" {
		dir, err := expandPath(raw.Dir)
		if err != nil {
			return Backups{}, fmt.Errorf("dir: %w", err)
		}
		b.Dir = dir
	}
	if raw.Keep != nil {
		if *raw.Keep < 0 {
			return Backups{}, fmt.Errorf("keep must be non-negative, got %d", *raw.Keep)
		}
		b.Keep = *raw.Keep
	}
	if raw.Cloud != nil {
		b.Cloud = *raw.Cloud
	}
	if raw.CloudKeep != nil {
		if *raw.CloudKeep < 0 {
			return Backups{}, fmt.Errorf("cloud_keep must be non-negative, got %d", *raw.CloudKeep)
		}
		b.CloudKeep = *raw.CloudKeep
	}
	return b, nil
}

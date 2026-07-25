package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// This file holds the v26 → v27 bulk conversion of every pre-convention
// table to SQLite STRICT (issue #148). It lives apart from migrations.go
// because it is a mechanical rewrite of the whole schema rather than a
// feature step: the table bodies below are the authoritative shape of the
// eighteen tables that predate the STRICT convention, transcribed verbatim
// from v26 with `STRICT` appended and nothing else changed. The registry
// entry that runs it stays in migrations.go with every other step.

// strictRebuildSuffix names the scratch table each rebuild copies into
// before it is renamed over the original. Naming it after the target
// version follows the convention of the earlier rebuilds (files_v3,
// runs_v25) and keeps the name recognisable if one ever shows up in a
// debugger or a `db schema` dump.
const strictRebuildSuffix = "_v27"

// strictRebuild is one table's conversion. body is the parenthesised
// column/constraint list of the CREATE TABLE, and objects holds the indexes
// and triggers to recreate after the rename — SQLite drops those along with
// the old table, and they are not carried by the copy.
//
// body deliberately omits the table name and the STRICT keyword:
// rebuildTableAsStrict spells the scratch name itself, so no spec can name
// the wrong table or forget the keyword this migration exists to add.
type strictRebuild struct {
	table   string
	body    string
	objects []string
}

// migrateV26ToV27 converts every remaining non-STRICT table to STRICT,
// closing the second half of issue #148 (the first half — STRICT for new
// tables — has been convention since v25's destination_alarms).
//
// STRICT makes SQLite reject any value whose storage class can't be
// losslessly converted to the column's declared type, instead of quietly
// coercing it through type affinity. Every column in this schema already
// declares INTEGER, TEXT, or BLOB and every writer already binds matching
// Go types, so no row in a healthy database changes; what changes is the
// failure mode of a future mishap (a stray string bound into size_bytes, a
// concatenated query, a reflection accident): a hard error at the write
// instead of a wrong-storage-class row that reads back as garbage. That is
// the same integrity-first stance as the CHECK / NOT NULL / append-only
// trigger discipline the schema already carries.
//
// STRICT cannot be switched on with ALTER, so each table is rebuilt with
// the standard recipe: create a STRICT copy, INSERT…SELECT every row, drop
// the original, rename the copy into place, recreate its indexes and
// triggers. The whole sweep runs in one transaction with foreign keys off —
// off because the drops and renames would otherwise trip the dense FK graph
// mid-rebuild, and because a rename with FKs enabled would rewrite other
// tables' REFERENCES clauses to point at the scratch name. PRAGMA
// foreign_key_check verifies the result before commit, exactly as the v4→v5
// and v24→v25 rebuilds do.
//
// Cost: this rewrites the entire database, so it is O(index size) in time
// and needs room for a second copy of the largest table. It is a one-time
// cost at upgrade, and Open's pre-migration snapshot is the rollback
// surface if anything goes wrong. Row counts are compared per table so a
// silently short copy fails the migration rather than losing observations.
func migrateV26ToV27(ctx context.Context, db *sql.DB) error {
	conn, restore, err := disableForeignKeys(ctx, db)
	if err != nil {
		return err
	}
	defer restore()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, r := range strictRebuildsV27() {
		// A table can legitimately be absent: this sweep converts the
		// tables the chain has materialised, and it is not the place to
		// diagnose a database whose history skipped one (a partial
		// fixture, a hand-repaired index). There is nothing to convert
		// and nothing to lose, so move on.
		present, err := tableExists(ctx, tx, r.table)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		if err := rebuildTableAsStrict(ctx, tx, r); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (27)`); err != nil {
		return fmt.Errorf("record schema v27: %w", err)
	}
	if err := verifyForeignKeysClean(ctx, tx, "v26→v27"); err != nil {
		return err
	}
	return tx.Commit()
}

// rebuildTableAsStrict performs one table's create-copy-drop-rename cycle
// and recreates the objects SQLite dropped with the original table. Each
// cycle leaves the schema whole before the next begins, so the rename —
// which makes SQLite reparse every trigger in the schema — never sees a
// half-converted database.
func rebuildTableAsStrict(ctx context.Context, tx *sql.Tx, r strictRebuild) error {
	tmp := r.table + strictRebuildSuffix
	before, err := countTableRows(ctx, tx, r.table)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("CREATE TABLE %s %s STRICT", tmp, r.body)); err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	cols, err := tableColumnNames(ctx, tx, tmp)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s", tmp, cols, cols, r.table)); err != nil {
		return fmt.Errorf("copy %s into its STRICT replacement (a legacy value whose storage class doesn't match its column type surfaces here; nothing is committed, and the pre-migration snapshot under backups/ is the rollback surface): %w", r.table, err)
	}
	after, err := countTableRows(ctx, tx, tmp)
	if err != nil {
		return err
	}
	if before != after {
		return fmt.Errorf("copy %s: %d of %d rows carried over; refusing to drop the original", r.table, after, before)
	}
	stmts := append([]string{
		fmt.Sprintf("DROP TABLE %s", r.table),
		fmt.Sprintf("ALTER TABLE %s RENAME TO %s", tmp, r.table),
	}, r.objects...)
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("rebuild %s: exec %q: %w", r.table, q, err)
		}
	}
	return nil
}

// tableExists reports whether the named table is present in the main
// schema. Indexes and triggers are excluded by the type filter.
func tableExists(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
		return false, fmt.Errorf("look up table %s: %w", table, err)
	}
	return n > 0, nil
}

// countTableRows counts a table inside the migration transaction. table comes
// from the spec list in this file, never from user input.
func countTableRows(ctx context.Context, tx *sql.Tx, table string) (int64, error) {
	var n int64
	if err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s: %w", table, err)
	}
	return n, nil
}

// tableColumnNames returns the table's columns in declaration order, joined for
// use in an INSERT … SELECT. Reading them back from the freshly created
// STRICT table means the copy always names exactly the columns that exist,
// so a spec edit can't leave a column list behind to drift.
func tableColumnNames(ctx context.Context, tx *sql.Tx, table string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return "", fmt.Errorf("read columns of %s: %w", table, err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", fmt.Errorf("scan column of %s: %w", table, err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate columns of %s: %w", table, err)
	}
	if len(cols) == 0 {
		return "", fmt.Errorf("table %s has no columns", table)
	}
	return strings.Join(cols, ", "), nil
}

// strictRebuildsV27 is the full conversion list, split into one function per
// subsystem — each is a DDL table rather than logic, so the split is for
// readability, not phases. The order between groups is free: every rebuild
// is self-contained, and the generated schema.sql snapshot is sorted by
// table name regardless.
//
// contested_paths and destination_alarms are absent because they were born
// STRICT (v26 and v25).
func strictRebuildsV27() []strictRebuild {
	var rs []strictRebuild
	rs = append(rs, strictRebuildsBookkeeping()...)
	rs = append(rs, strictRebuildsVolumeTree()...)
	rs = append(rs, strictRebuildsContentModel()...)
	rs = append(rs, strictRebuildsRunHistory()...)
	rs = append(rs, strictRebuildsDestinationState()...)
	rs = append(rs, strictRebuildsUploadState()...)
	rs = append(rs, strictRebuildsPeerState()...)
	return rs
}

// strictRebuildsBookkeeping covers the two tables that aren't part of any
// subsystem: the version marker and the node registry. schema_version is
// rebuilt like any other table — the INSERT recording v27 lands in the
// rebuilt copy, inside the same transaction.
func strictRebuildsBookkeeping() []strictRebuild {
	return []strictRebuild{
		{
			table: "schema_version",
			body: `(
	version INTEGER NOT NULL PRIMARY KEY
)`,
		},
		{
			table: "nodes",
			body: `(
	id                     INTEGER PRIMARY KEY,
	name                   TEXT NOT NULL UNIQUE,
	endpoint               TEXT,
	public_key_fingerprint TEXT
)`,
		},
	}
}

// strictRebuildsVolumeTree covers the addressing side of the index: the
// volumes and the folder tree inside them, including folders' self
// reference through parent_id (harmless here — the rebuild runs with
// foreign keys off).
func strictRebuildsVolumeTree() []strictRebuild {
	return []strictRebuild{
		{
			table: "volumes",
			body: `(
	id   INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	path TEXT NOT NULL
)`,
		},
		{
			table: "folders",
			body: `(
	id                  INTEGER PRIMARY KEY,
	volume_id           INTEGER NOT NULL REFERENCES volumes(id),
	parent_id           INTEGER REFERENCES folders(id),
	path                TEXT    NOT NULL,
	shallow_blake3      BLOB    CHECK (shallow_blake3 IS NULL OR length(shallow_blake3) = 32),
	deep_blake3         BLOB    CHECK (deep_blake3    IS NULL OR length(deep_blake3)    = 32),
	last_changed_run_id INTEGER REFERENCES runs(id),
	file_count          INTEGER NOT NULL DEFAULT 0,
	cumulative_size     INTEGER NOT NULL DEFAULT 0,
	UNIQUE (volume_id, path)
)`,
			objects: []string{
				`CREATE INDEX idx_folders_parent ON folders(parent_id)`,
			},
		},
	}
}

// strictRebuildsContentModel covers the core principle's two tables: the
// append-only contents entity and the files observations that bind a path
// to a content. contents keeps both append-only triggers and files keeps
// the partial unique index enforcing at most one live row per path — the
// schema-level guarantees AGENTS.md calls out, recreated verbatim after the
// rename because SQLite drops them with the old table.
func strictRebuildsContentModel() []strictRebuild {
	return []strictRebuild{
		{
			table: "contents",
			body: `(
	id             INTEGER PRIMARY KEY,
	blake3         BLOB NOT NULL UNIQUE CHECK (length(blake3) = 32),
	size_bytes     INTEGER NOT NULL,
	origin_node_id INTEGER REFERENCES nodes(id),
	origin_run_id  INTEGER
)`,
			objects: []string{
				`CREATE INDEX idx_contents_origin_node ON contents(origin_node_id)
 WHERE origin_node_id IS NOT NULL`,
				`CREATE TRIGGER contents_no_delete BEFORE DELETE ON contents
 BEGIN
     SELECT RAISE(ABORT, 'contents is append-only; a content row is never deleted');
 END`,
				`CREATE TRIGGER contents_no_update BEFORE UPDATE ON contents
 BEGIN
     SELECT RAISE(ABORT, 'contents is append-only; supersede the files row and insert new content instead of updating');
 END`,
			},
		},
		{
			table: "files",
			body: `(
	folder_id             INTEGER NOT NULL REFERENCES folders(id),
	name                  TEXT    NOT NULL,
	content_id            INTEGER NOT NULL REFERENCES contents(id),
	mtime_ns              INTEGER NOT NULL,
	status                TEXT    NOT NULL CHECK (status IN ('present','missing','superseded','offloaded')),
	first_seen_run_id     INTEGER NOT NULL REFERENCES runs(id),
	last_seen_run_id      INTEGER NOT NULL REFERENCES runs(id),
	indexed_at_ns         INTEGER NOT NULL,
	status_changed_run_id INTEGER REFERENCES runs(id),
	PRIMARY KEY (folder_id, name, content_id)
)`,
			objects: []string{
				`CREATE INDEX idx_files_content ON files(content_id)`,
				`CREATE INDEX idx_files_missing ON files(folder_id, name) WHERE status = 'missing'`,
				`CREATE UNIQUE INDEX uniq_files_live_per_path ON files(folder_id, name) WHERE status != 'superseded'`,
			},
		},
	}
}

// strictRebuildsRunHistory covers the audit trail: runs, the transition
// log hanging off it, and the hook invocations it triggers. runs never
// auto-prunes, so this is the largest history in the schema after files —
// its copy carries every id forward because six other tables reference it.
func strictRebuildsRunHistory() []strictRebuild {
	return []strictRebuild{
		{
			table: "runs",
			body: `(
	id                INTEGER PRIMARY KEY,
	kind              TEXT    NOT NULL CHECK (kind IN ('index','sync','restore','audit','offload')),
	volume_id         INTEGER REFERENCES volumes(id),
	destination       TEXT,
	started_at_ns     INTEGER NOT NULL,
	ended_at_ns       INTEGER,
	status            TEXT    NOT NULL CHECK (status IN ('running','success','failed','partial','refused','aborted')),
	error             TEXT,
	file_count        INTEGER NOT NULL DEFAULT 0,
	peer_node_id      INTEGER REFERENCES nodes(id),
	correlated_run_id INTEGER,
	shallow           INTEGER CHECK (shallow IS NULL OR shallow IN (0, 1)),
	CHECK (
		(kind IN ('index','audit','offload') AND destination IS NULL) OR
		(kind IN ('sync','restore') AND destination IS NOT NULL AND destination != '')
	)
)`,
			objects: []string{
				`CREATE INDEX idx_runs_volume_started ON runs(volume_id, started_at_ns)`,
				`CREATE INDEX idx_runs_destination ON runs(destination) WHERE destination IS NOT NULL`,
			},
		},
		{
			table: "runs_audit",
			body: `(
	id         INTEGER PRIMARY KEY,
	run_id     INTEGER NOT NULL REFERENCES runs(id),
	transition TEXT    NOT NULL,
	operator   TEXT,
	at_ns      INTEGER NOT NULL,
	note       TEXT
)`,
			objects: []string{
				`CREATE INDEX idx_runs_audit_run ON runs_audit(run_id)`,
			},
		},
		{
			table: "hook_runs",
			body: `(
	id                INTEGER PRIMARY KEY,
	volume_id         INTEGER NOT NULL REFERENCES volumes(id),
	trigger           TEXT    NOT NULL CHECK (trigger IN ('change','interval')),
	triggering_run_id INTEGER REFERENCES runs(id),
	changed           INTEGER NOT NULL CHECK (changed IN (0, 1)),
	started_at_ns     INTEGER NOT NULL,
	ended_at_ns       INTEGER,
	status            TEXT    NOT NULL CHECK (status IN ('running','success','failed')),
	exit_code         INTEGER,
	error             TEXT,
	CHECK (
		(trigger = 'change'   AND triggering_run_id IS NOT NULL) OR
		(trigger = 'interval' AND triggering_run_id IS NULL)
	)
)`,
			objects: []string{
				`CREATE INDEX idx_hook_runs_volume_trigger ON hook_runs(volume_id, trigger, started_at_ns)`,
			},
		},
	}
}

// strictRebuildsDestinationState covers the per-destination watermarks: the
// run-id each origin has landed at a destination, that watermark's history,
// and the push-freshness view. The columns bolted on by earlier ALTER TABLE
// steps (verify_method, source_node_id, verified_at_ns) keep their trailing
// positions, so the copy stays a positional no-op.
func strictRebuildsDestinationState() []strictRebuild {
	return []strictRebuild{
		{
			table: "destination_run_ids",
			body: `(
	volume_id      INTEGER NOT NULL REFERENCES volumes(id),
	destination    TEXT    NOT NULL,
	origin_node_id INTEGER NOT NULL REFERENCES nodes(id),
	origin_run_id  INTEGER NOT NULL,
	updated_at_ns  INTEGER NOT NULL,
	verify_method  TEXT,
	source_node_id INTEGER REFERENCES nodes(id),
	verified_at_ns INTEGER,
	PRIMARY KEY (volume_id, destination, origin_node_id)
)`,
		},
		{
			table: "destination_run_ids_history",
			body: `(
	id             INTEGER PRIMARY KEY,
	volume_id      INTEGER NOT NULL,
	destination    TEXT    NOT NULL,
	origin_node_id INTEGER NOT NULL,
	origin_run_id  INTEGER NOT NULL,
	at_ns          INTEGER NOT NULL,
	verify_method  TEXT,
	source_node_id INTEGER
)`,
			objects: []string{
				`CREATE INDEX idx_destination_run_ids_history
 ON destination_run_ids_history(volume_id, destination)`,
			},
		},
		{
			table: "destination_push_freshness",
			body: `(
	volume_id      INTEGER NOT NULL REFERENCES volumes(id),
	destination    TEXT    NOT NULL,
	origin_node_id INTEGER NOT NULL REFERENCES nodes(id),
	origin_run_id  INTEGER NOT NULL,
	updated_at_ns  INTEGER NOT NULL,
	PRIMARY KEY (volume_id, destination, origin_node_id)
)`,
		},
	}
}

// strictRebuildsUploadState covers what has been uploaded where: the
// per-content and per-pack upload fingerprints, and the pack entity with
// its members. remote_objects and remote_packs both carry the
// checksum_algo/checksum pairing CHECK, which the rebuild reproduces
// verbatim — STRICT constrains storage classes, not nullability pairings.
func strictRebuildsUploadState() []strictRebuild {
	return []strictRebuild{
		{
			table: "remote_objects",
			body: `(
	content_id      INTEGER NOT NULL REFERENCES contents(id),
	destination     TEXT    NOT NULL,
	uploaded_run_id INTEGER NOT NULL REFERENCES runs(id),
	checksum_algo   TEXT,
	checksum        TEXT,
	verified_at_ns  INTEGER,
	PRIMARY KEY (content_id, destination),
	CHECK ((checksum_algo IS NULL) = (checksum IS NULL))
)`,
		},
		{
			table: "packs",
			body: `(
	id             INTEGER PRIMARY KEY,
	pack_key       BLOB NOT NULL UNIQUE CHECK (length(pack_key) = 32),
	size_bytes     INTEGER NOT NULL,
	member_count   INTEGER NOT NULL,
	created_run_id INTEGER NOT NULL REFERENCES runs(id)
)`,
		},
		{
			table: "pack_members",
			body: `(
	content_id  INTEGER NOT NULL REFERENCES contents(id),
	pack_id     INTEGER NOT NULL REFERENCES packs(id),
	byte_offset INTEGER NOT NULL,
	byte_length INTEGER NOT NULL,
	PRIMARY KEY (content_id)
)`,
			objects: []string{
				`CREATE INDEX idx_pack_members_pack ON pack_members(pack_id)`,
			},
		},
		{
			table: "remote_packs",
			body: `(
	pack_id         INTEGER NOT NULL REFERENCES packs(id),
	destination     TEXT    NOT NULL,
	uploaded_run_id INTEGER NOT NULL REFERENCES runs(id),
	checksum_algo   TEXT,
	checksum        TEXT,
	verified_at_ns  INTEGER,
	PRIMARY KEY (pack_id, destination),
	CHECK ((checksum_algo IS NULL) = (checksum IS NULL))
)`,
		},
	}
}

// strictRebuildsPeerState covers the peer-sync watermarks and their
// history. last_shared_run_id stays free of a FK on purpose: it carries the
// *initiator's* run id, which is meaningless in this node's runs table.
func strictRebuildsPeerState() []strictRebuild {
	return []strictRebuild{
		{
			table: "peer_sync_state",
			body: `(
	volume_id          INTEGER NOT NULL REFERENCES volumes(id),
	peer_node_id       INTEGER NOT NULL REFERENCES nodes(id),
	last_shared_run_id INTEGER,
	last_synced_at     INTEGER NOT NULL,
	PRIMARY KEY (volume_id, peer_node_id)
)`,
		},
		{
			table: "peer_sync_state_history",
			body: `(
	id                 INTEGER PRIMARY KEY,
	volume_id          INTEGER NOT NULL REFERENCES volumes(id),
	peer_node_id       INTEGER NOT NULL REFERENCES nodes(id),
	last_shared_run_id INTEGER,
	last_synced_at     INTEGER NOT NULL,
	at_ns              INTEGER NOT NULL
)`,
			objects: []string{
				`CREATE INDEX idx_peer_sync_history_pair
 ON peer_sync_state_history(volume_id, peer_node_id)`,
			},
		},
	}
}

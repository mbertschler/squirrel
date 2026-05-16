# squirrel

Backup tool for your own NAS + cloud offsite storage.

This first milestone is a local content-addressed file indexer: walk a directory tree, hash each file with BLAKE3, store the result in SQLite, and answer queries about duplicates and missing files.

## Principle

Squirrel indexes **content**, not paths. A BLAKE3 hash that has ever been observed stays retrievable — paths are observations of content, not the other way around. When content at a path changes, the prior row is flipped to `superseded` and a new row is inserted; the old hash is never rewritten in place. `squirrel query <hash>` will still find a hash whose path now holds different content.

## Install

```
go install github.com/mbertschler/squirrel/cmd/squirrel@latest
```

Or from a checkout:

```
go build -o squirrel ./cmd/squirrel
```

## Quickstart

Index a directory:

```
squirrel index ~/Pictures
```

By default the database is written to `$HOME/.squirrel/index.db` (the parent directory is created if missing). Pick a different location with `--db`:

```
squirrel index --db ~/.squirrel/pictures.db ~/Pictures
```

Re-running `squirrel index` updates the index incrementally — new files are added, modified files re-hashed, and files no longer on disk are flagged as missing (rows are not deleted). Pass `--shallow` to skip re-hashing files whose `(size, mtime)` already match the stored row, or `--dry-run` to see what would change without writing to the database.

Every non-dry-run `index` invocation records a row in the `runs` table (kind, volume, started/ended timestamps, terminal status, error, file count) and each `files` row carries `first_seen_run_id` / `last_seen_run_id` pointing into that history. Dry runs intentionally leave the runs table untouched.

Look up a file by its BLAKE3 hex hash:

```
squirrel query 26e70f0a438787ee143979a9b519a4a330ea21e0a23d31fcb47051e70b8fe5ad
```

Look up the row for a path:

```
squirrel query ~/Pictures/foo.jpg
```

List hashes that appear at more than one path:

```
squirrel query --duplicates
```

List paths previously indexed but no longer on disk:

```
squirrel query --missing
```

Inspect the content history of a single path — the live row plus every superseded predecessor:

```
squirrel query --history ~/Pictures/foo.jpg
```

List recent index runs (most recent first):

```
squirrel runs
squirrel runs --volume pictures --limit 5
```

## CLI reference

```
squirrel index <path> [--db PATH] [--shallow] [--dry-run] [--workers N]
squirrel query <hash-or-path> [--db PATH] [--history]
squirrel query --duplicates [--db PATH]
squirrel query --missing [--db PATH]
squirrel runs [--db PATH] [--volume NAME] [--limit N]
squirrel volumes [--db PATH]
```

| Flag         | Default                      | Meaning                                              |
| ------------ | ---------------------------- | ---------------------------------------------------- |
| `--db`       | `$HOME/.squirrel/index.db`   | SQLite database path                                 |
| `--shallow`  | off                          | Skip re-hashing when `(size, mtime)` already matches |
| `--dry-run`  | off                          | Report changes without writing to the database       |
| `--workers`  | `NumCPU()`                   | Number of hashing workers                            |

## Notes

- Hash: BLAKE3-256 via `github.com/zeebo/blake3`. Stored as a 32-byte `BLOB` in the `blake3` column (encoding the algorithm in the column name leaves room for a future `sha256` column without ambiguity). The CLI accepts and prints hex.
- Storage: SQLite via the pure-Go `modernc.org/sqlite`. WAL mode is enabled at open.
- Symlinks are skipped.
- Paths are stored **relative** to the indexed root. Each row has a `(root, path)` composite key. Today the `root` column holds the absolute path passed to `squirrel index`; a future config milestone will replace it with a logical root name.
- Re-indexing always compares the actual hash unless `--shallow` is set.

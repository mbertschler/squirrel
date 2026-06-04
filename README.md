# squirrel

Backup tool for your own NAS + cloud offsite storage.

Squirrel indexes a local file tree by BLAKE3 content hash and syncs it to one or more remote destinations (NAS, S3, B2, GCS, SFTP, …) via rclone. Every upload is BLAKE3-verified end-to-end. Destinations are append-only: an overwrite at the destination moves the prior bytes into `.squirrel-history/run-<id>/`, never deletes them.

## Principle

Squirrel indexes **content**, not paths. A BLAKE3 hash that has ever been observed stays retrievable — paths are observations of content, not the other way around. When content at a path changes, the prior row is flipped to `superseded` and a new row is inserted; the old hash is never rewritten in place. `squirrel query <hash>` will still find a hash whose path now holds different content.

The same principle extends to sync: overwrites at the destination are preserved under `<dest>/<volume>/.squirrel-history/run-<id>/`, and `squirrel sync` never deletes files at the destination even when the local copy is gone.

## Install

```
go install github.com/mbertschler/squirrel/cmd/squirrel@latest
```

You will also need [rclone](https://rclone.org) ≥ 1.66 on `PATH` for `sync` and `restore` to work (BLAKE3 hash support landed in rclone 1.66):

```
brew install rclone     # macOS
apt install rclone      # Debian / Ubuntu
```

## Configuration

Squirrel is configured via a TOML file at `~/.squirrel/config.toml` (override with `--config <path>` or `$SQUIRREL_CONFIG`). Every volume and destination squirrel touches must be declared there — there is no implicit "just point at a directory" mode.

```toml
db = "~/.squirrel/index.db"

[volumes.pictures]
path    = "~/Pictures"
sync_to = ["nas", "offsite"]

[volumes.docs]
path    = "~/Documents"
sync_to = ["nas"]

[destinations.nas]
type     = "sftp"
host     = "nas.local"
user     = "martin"
password = { env = "NAS_PASSWORD" }
root     = "/volume1/squirrel"

[destinations.offsite]
type              = "s3"
provider          = "AWS"
region            = "eu-central-1"
access_key_id     = { env = "AWS_ACCESS_KEY_ID" }
secret_access_key = { env = "AWS_SECRET_ACCESS_KEY" }
bucket            = "squirrel-backup"
root              = "/squirrel"
```

Supported destination types: `local`, `sftp`, `s3`, `b2`, `gcs`. Secrets accept either a literal string or an inline `{ env = "VAR_NAME" }` table that is resolved at load time. Unknown fields, missing required fields, and unset env vars are rejected immediately — squirrel will not invoke rclone with a misconfigured destination.

Squirrel writes its own `rclone.conf` next to the config (`~/.squirrel/rclone.conf`, mode 0600) on every sync invocation. You do not run `rclone config` and you should not edit `rclone.conf` by hand.

### Hooks

A volume can declare a per-volume **hook** — a command the agent runs to nudge an external tool when the volume's content changes. squirrel stays tool-agnostic: it never learns what the command does (a backup with kopia/restic, an `rclone copy`, a shell script — all the same to squirrel). It exec's the command **without a shell**, passes context through environment variables, and records only the generic outcome (exit code, timestamps).

```toml
[volumes.pictures.hook]
command  = ["kopia", "snapshot", "create", "."]
timeout  = "30m"   # optional, defaults to 1h
interval = "24h"   # optional — also fire on this cadence (see below)
```

A hook fires on two triggers, both reusing the same command:

- **on change** — after every successful index run on the volume (which the agent runs on the `index_every` / `sync_every` cadence). This answers *"is the latest content backed up?"*. It keys off content settling, not off a sync to a remote, so a volume needs no `sync_to` destination for the hook to be useful.
- **on interval** — every `interval`, *regardless of whether anything changed*. This answers *"is the existing backup still intact?"*. Verification is orthogonal to change — bitrot happens to static data — so re-checks have to run on a clock. Omit `interval` to fire on-change only.

The command tells the two apart via `SQUIRREL_TRIGGER` (so a single command can back up on change and verify on interval). It is **best-effort**: a hook failure or timeout never fails or blocks the run that triggered it, and overlapping invocations for the same volume are skipped rather than stacked. The command receives:

| Variable | Meaning |
|---|---|
| `SQUIRREL_VOLUME` | volume name |
| `SQUIRREL_PATH` | absolute volume path |
| `SQUIRREL_RUN_ID` | the index run that triggered the hook (empty on the interval trigger) |
| `SQUIRREL_CHANGED` | `true`/`false` — whether the run observed changes (so the command can cheaply no-op); always `false` on the interval trigger |
| `SQUIRREL_TRIGGER` | `change` or `interval` |

Because the command is exec'd without a shell, the volume path is never string-concatenated into a command line. If you want shell features, make the command `["sh", "-c", "…"]` yourself. Recorded outcomes are visible via `squirrel hooks` and the TUI's Hooks tab.

> **Don't double-schedule verification.** If your external tool already runs its own verify on a timer (e.g. a cron/systemd job), don't *also* set `interval` for a verify command — two heavy passes will step on each other. Pick one driver: let squirrel schedule it (so the result lands in `squirrel hooks` / the TUI) **or** let the tool schedule it (maximum independence — verification keeps happening even when the agent is down), not both.

### Index snapshots

The catalog should be as redundant as the data it describes. After every successful sync, squirrel takes one `VACUUM INTO` snapshot of the whole index (a self-contained, `db check`-able `.db` file) to a **local tier** and — for destination (bucket/sftp/…) syncs — rides a copy **along to the destination**, under each synced volume's `.squirrel-index/`. A restore-from-cloud then yields the data *and* the index that explains it.

This is **on by default, zero-config** — an absent `[backups]` table means it's enabled with the defaults below. Override or disable via:

```toml
[backups]
enabled    = true   # local snapshot-on-sync (default true)
dir        = ""     # local snapshot directory (default: <dir of db>/backups)
keep       = 7      # local snapshots kept (rotation; 0 = keep all)
cloud      = true   # ride a copy along to destination buckets (default true)
cloud_keep = 7      # snapshots kept per <dest>/<volume>/.squirrel-index/ (0 = keep all)
```

`enabled = false` disables both halves; `cloud = false` keeps the local snapshot but uploads nothing. Snapshots are named `index-<ISO8601>-run-<id>.db` — lexically sortable and traceable to the run that produced them. A single snapshot is taken per `squirrel sync` invocation and fanned out to every target; a snapshot or upload failure is surfaced as a warning but never fails the sync.

> **Privacy.** The ride-along payload is the *full global* `index.db` — paths and BLAKE3 hashes for **all** volumes (never file contents). It lands in the same bucket as your data (same trust boundary). Use a private bucket and server-side encryption.

## Quickstart

Index a configured volume:

```
squirrel index pictures
```

Re-running `squirrel index` updates the index incrementally — new files are added, modified files re-hashed, and files no longer on disk are flagged as missing (rows are not deleted). Pass `--shallow` to skip re-hashing files whose `(size, mtime)` already match the stored row, or `--dry-run` to see what would change without writing to the database.

Sync a volume to its configured destinations:

```
squirrel sync pictures              # all destinations declared on pictures
squirrel sync pictures --to nas     # just one
squirrel sync                       # every (volume, destination) pair in config
```

Sync verifies each uploaded file's BLAKE3 against the destination (using rclone's `--checksum --hash blake3`). Mismatches abort that file before the runs row is marked success. Use `--shallow` to fall back to rclone's default size+mtime comparison if you want speed over integrity for a big initial push.

Look up a file by its BLAKE3 hex hash:

```
squirrel query 26e70f0a438787ee143979a9b519a4a330ea21e0a23d31fcb47051e70b8fe5ad
```

Look up the row for a path:

```
squirrel query ~/Pictures/foo.jpg
```

List hashes that appear at more than one path, paths no longer on disk, or the full content history at a path:

```
squirrel query --duplicates
squirrel query --missing
squirrel query --history ~/Pictures/foo.jpg
```

List recent runs (most recent first):

```
squirrel runs
squirrel runs --volume pictures --limit 5
```

Open the interactive terminal UI to watch live runs, browse the index ncdu-style, and drill into individual run records:

```
squirrel tui
squirrel        # bare invocation opens the TUI when stdin/stdout are a terminal
```

## CLI reference

```
squirrel index   <volume>            [--shallow] [--dry-run] [--workers N]
squirrel sync    [<volume>]          [--to DEST] [--shallow] [--dry-run]
squirrel query   <hash-or-path>      [--history]
squirrel query   --duplicates
squirrel query   --missing
squirrel runs                        [--volume NAME] [--limit N]
squirrel volumes
squirrel tui
```

| Flag        | Default                       | Meaning                                                          |
| ----------- | ----------------------------- | ---------------------------------------------------------------- |
| `--config`  | `~/.squirrel/config.toml`     | TOML configuration file (env: `SQUIRREL_CONFIG`)                 |
| `--db`      | from config, else default     | SQLite database path; overrides `db` in config                   |
| `--shallow` | off                           | Skip BLAKE3 verification; use rclone's default size+mtime check  |
| `--dry-run` | off                           | Report what would change without writing                         |
| `--workers` | `NumCPU()`                    | Number of hashing workers (index only)                           |

## Destination layout

Each destination is a tree shaped like the local volumes:

```
<dest.root>/
  pictures/
    2024/cat.jpg
    .squirrel-history/run-7/2024/cat.jpg     # prior content of cat.jpg
    .squirrel-index/index-2026-06-04T12:00:00Z-run-12.db   # global index snapshot (ride-along)
  docs/
    invoice.pdf
    .squirrel-history/run-9/invoice.pdf
```

`.squirrel-history/run-<run-id>/` is rclone's `--backup-dir` target for that sync run. It is filtered out of all subsequent comparisons so it does not grow rclone's listing time or get uploaded back. A directory literally called `.squirrel-history` in your source volume is also filtered (with a warning), to keep the reserved name out of the destination tree by accident.

`.squirrel-index/` holds the index snapshots ridden along after each successful sync (see [Index snapshots](#index-snapshots)). Like `.squirrel-history`, it is filtered out of all sync and restore transfers and from peer-sync, so a snapshot is never mistaken for user content.

## Notes

- Hash: BLAKE3-256 via `github.com/zeebo/blake3`. Stored as a 32-byte `BLOB` in the `blake3` column. The CLI accepts and prints hex.
- Storage: SQLite via the pure-Go `modernc.org/sqlite`. WAL mode is enabled at open. Schema version 10; older databases auto-migrate forward on first open.
- Symlinks are skipped during indexing.
- Sync runs do not pass `--delete-*` to rclone. Files removed locally remain at the destination.
- The `runs` table is never auto-pruned; the run history is an audit trail and any retention is explicit and operator-driven only.

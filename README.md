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

Supported destination types: `local`, `sftp`, `s3`, `b2`, `gcs` (rclone-backed), and `kopia` (see [kopia destinations](#kopia-destinations)). Secrets accept either a literal string or an inline `{ env = "VAR_NAME" }` table that is resolved at load time. Unknown fields, missing required fields, and unset env vars are rejected immediately — squirrel will not invoke rclone with a misconfigured destination.

Squirrel writes its own `rclone.conf` next to the config (`~/.squirrel/rclone.conf`, mode 0600) on every sync invocation. You do not run `rclone config` and you should not edit `rclone.conf` by hand.

### Encrypted destinations

Any non-`local` destination can add a `crypt` block to encrypt file contents client-side before upload, via rclone's [crypt](https://rclone.org/crypt/) overlay:

```toml
[destinations.offsite.crypt]
password  = { env = "OFFSITE_CRYPT_PASSWORD" }
password2 = { env = "OFFSITE_CRYPT_SALT" }    # salt — optional but recommended
```

`password` and `password2` are **rclone-obscured** values, the same representation `rclone config` stores for its own crypt remotes — generate one with `rclone obscure <plaintext>`. Both accept a literal or `{ env = "VAR" }`. Squirrel renders two sections into its `rclone.conf` — the underlying remote plus a crypt remote wrapping it — and addresses all sync and restore transfers through the crypt remote. Keep the passwords safe: restoring from an encrypted destination requires them.

Two properties to be aware of:

- **Contents only.** File and directory names are stored in clear at the destination (`filename_encryption = off`, fixed by design) — the tree stays browsable and keeps the same layout as an unencrypted destination. If the names themselves are sensitive, this overlay does not hide them.
- **Verification falls back to size+mtime.** rclone crypt remotes cannot expose content hashes, so the end-to-end BLAKE3 check (`--checksum --hash blake3`) cannot pass through the overlay. Transfers to and from an encrypted destination compare by size+mtime instead — the same comparison `--shallow` uses — and say so in the run output; the runs row records the transfer as shallow. Content-addressed destinations regain deeper verification through provider-side ciphertext fingerprints — see [Offsite verification](#offsite-verification-squirrel-verify).

### Kopia destinations

A `kopia` destination pushes a volume into a local [kopia](https://kopia.io) repository instead of an rclone remote — useful as a second, independently-verifiable backup format on another disk:

```toml
[destinations.mirror]
type     = "kopia"
root     = "/mnt/backup/kopia-repo"      # repository path
password = { env = "KOPIA_REPO_PASSWORD" }

[volumes.pictures]
path    = "~/Pictures"
sync_to = ["nas", "mirror"]
```

Like rclone, the kopia binary is driven as an opaque child process with squirrel owning the command line: each sync connects to the repository at `root` (creating it on first use), runs `kopia snapshot create` on the volume path, then `kopia snapshot verify` on the new snapshot. The repository password is passed to kopia via its environment, never on the command line, and the per-destination kopia config file lives next to squirrel's own config — your personal kopia configuration is never touched.

Properties that differ from rclone destinations:

- **kopia verifies its own content hashes**, so the runs row is never recorded as shallow and `--shallow` has no effect on kopia pairs; whether a given run counts as verified comes from kopia itself (a clean snapshot plus a passing `snapshot verify`). `--dry-run` is refused — kopia has no equivalent.
- **A `crypt` block is rejected**: kopia encrypts its repository itself. Keep the repository password safe; the repository is unreadable without it.
- **Restore goes through the kopia CLI** (`kopia snapshot restore`), since the repository is kopia's own format. `squirrel restore` refuses kopia destinations and says so.

### Content-addressed destinations

By default a destination mirrors the volume's tree (see [Destination layout](#destination-layout)). Any rclone-remote destination — with or without a `crypt` block — can instead opt into an **append-only, content-addressed** layout, built for cold archive storage where objects should never be rewritten or moved:

```toml
[destinations.archive]
type   = "sftp"
host   = "archive.example"
user   = "u"
root   = "/data"
layout = "content-addressed"
```

Instead of a browsable tree, the destination holds two streams:

- **`objects/<hash>`** (at the destination root, shared by all volumes) — one object per BLAKE3 content hash (lowercase hex), the raw file bytes (encrypted client-side when the destination has a `crypt` block). Each hash is uploaded **exactly once** per destination and never moved, overwritten, or deleted. A local rename or reorg changes only the path mapping — no re-upload, no server-side copy — and content duplicated across volumes is stored once.
- **`<volume>/index/run-<id>`** — one immutable **manifest segment** per sync run, per volume: the path-level delta of that run (see the format below). Replaying a volume's segments in run order yields its full current path→content mapping, and any past state.

Durability is **transactional per run**: the run only counts as successful — and only then feeds the durability evidence squirrel records per destination — once *both* all its content objects *and* its manifest segment are confirmed on the remote (each transfer's success plus a follow-up presence/size listing). A failed run may leave objects without a segment; they are harmless (nothing maps them) and the next run skips re-uploading anything already recorded, pushing only what's missing.

Properties that differ from mirrored destinations:

- **Verification is presence+size**, recorded as such: per-object transfers can't carry the end-to-end BLAKE3 check (and `crypt` remotes expose no hashes at all), so the runs row is recorded shallow and the push never claims content verification. On top of that, each upload's provider-side ciphertext fingerprint is recorded in the index and re-checked by [`squirrel verify`](#offsite-verification-squirrel-verify).
- **Pick the layout when the destination is first used.** Switching an existing mirrored destination to `content-addressed` (or back) is not supported — point the new layout at a fresh destination or root. The push detects a mirrored history (a recorded successful sync without its manifest segment) and refuses.
- **`squirrel restore` refuses the layout** for now; recovery tooling ships separately. The format is deliberately simple enough to recover without squirrel — see below.
- `--dry-run` is not supported yet.

#### Offsite verification (`squirrel verify`)

Cold archive storage is exactly the copy you can't cheaply re-download and re-hash. Content-addressed destinations therefore get a metadata-only integrity check, the **scan-back fingerprint**: after each object upload is confirmed, squirrel reads the *provider's own checksum* of the stored bytes (the ciphertext, for `crypt` destinations) back from the remote via `rclone lsjson --hash` and records it in the index next to the upload. Verification then re-fetches the same metadata later and compares **provider value then vs provider value now** — squirrel never recomputes a provider checksum, so provider-specific composite forms are handled as opaque strings, and no object body is ever transferred.

What gets recorded depends on the backend type:

- **`s3`** — the object **ETag**, recorded as `etag-md5` (or `etag-md5-composite` for multipart-style values). Reading it is a listing/metadata operation, so it works on archive-tier objects without a restore.
- **`sftp`** — the checksum computed server-side by the remote's hash command. Content-addressed sftp destinations default to **SHA-256** (`hash_algo = "sha256"`, rendered as rclone's sftp `hashes` option so the selection is explicit rather than rclone's md5/sha1 preference); set `hash_algo` if your server only offers another type.
- **other backends** — whatever hash `rclone lsjson --hash` exposes, recorded under its rclone hash name (e.g. `sha1` on b2). A backend exposing no checksum leaves the fingerprint pending, with a warning in the sync output.

Re-verify a destination (or all content-addressed destinations) at any time:

```
squirrel verify archive
squirrel verify
```

The pass lists the destination's `objects/` directory once (batched, metadata-only), then per recorded object: a **match** stamps the object verified in the index; an object **without a fingerprint yet** (uploaded before this feature, or whose capture failed) gets one recorded and is counted separately; a **mismatch or missing object** prints one loud line per object and exits non-zero — that is potential offsite corruption or tampering, and squirrel deliberately leaves both the destination and the recorded fingerprint untouched for inspection. Each pass is recorded as an `audit` run, with the destination and counters in the run's audit trail.

Because crypt encrypts with a random per-file nonce, the fingerprint is a property of the *uploaded ciphertext*, not of the content — which is exactly right here: the layout is append-only and each object is uploaded once, so the fingerprint is stable for the life of the object.

Two related destination knobs (both optional):

```toml
[destinations.archive]
# ...
hash_algo = "sha256"  # sftp only: which server-side hash the fingerprint uses
checkers  = 4         # cap rclone's concurrent checkers (providers that limit connections)
```

`checkers` flows into `--checkers` on the rclone invocations squirrel runs against that destination — useful when a provider caps simultaneous connections (server-side hashing typically uses one connection per concurrent check).

#### Manifest segment format

Each `<volume>/index/run-<id>` segment is JSONL — one JSON object per line, lines sorted by `(path, status)`:

```json
{"path":"2024/cat.jpg","blake3":"26e7…e5ad","status":"present","size_bytes":123,"mtime_ns":1712345678901234567}
```

- `path` — volume-relative path
- `blake3` — 64-char lowercase hex BLAKE3-256 of the file content; the bytes live at `objects/<blake3>`
- `status` — `present`, `superseded`, `missing`, or `offloaded`
- `size_bytes`, `mtime_ns` — as indexed

To replay: process segments in ascending run id; each line with status `present`, `missing`, or `offloaded` sets that path's current `(content, status)` — last write wins per path. `superseded` lines are history only (the outgoing content of a path that changed) and update no mapping. A full recovery script is: replay every segment, then for each `present`/`offloaded` path download `objects/<blake3>` (decrypting with the `crypt` password if one was set). `missing` paths are known-but-lost at the origin — the object may still exist from an earlier upload.

### Offloading

`squirrel offload` deletes the **local** copy of files whose content is provably stored on every target the volume's offload policy requires — never a blind delete. It is the only squirrel command that deletes user data.

```toml
[volumes.pictures]
path             = "~/Pictures"
sync_to          = ["nas", "offsite"]
offload_requires = ["nas", "offsite"]
```

`offload_requires` is the explicit per-volume policy: every named target's recorded durability must cover a file's content before its bytes may go, and a volume without the key refuses to offload entirely. The names share the flat destination/node namespace that `sync_to` uses. They may also name targets only a *peer* pushes to: evidence about those arrives through the peer durability pull (`squirrel peer-sync pull-durability`), and a name with no recorded evidence simply keeps the gate closed.

```
squirrel offload pictures 2019/             # a subtree
squirrel offload pictures --older-than 90d  # by age (indexed mtime)
squirrel offload pictures . --dry-run       # print the gate decisions, touch nothing
```

Selectors are volume-relative paths/prefixes plus `--older-than` (combinable); selecting the whole volume takes an explicit `.`. The durability gate is evaluated per file, entirely offline, against the durability version vectors in the local index: content with origin `(node, run)` passes for a target iff the target's recorded vector component for that node is ≥ `run`, for **every** required target. Files failing the gate are skipped and reported per target (`missing component for origin X` / `stale: have 40 need 45`).

Immediately before each unlink, squirrel re-verifies the on-disk bytes against the indexed row — size, mtime, and BLAKE3, with symlink-refusing traversal — and skips loudly on any difference: the disk is newer than the index, and unindexed bytes are never deleted. Offloaded files flip `present → offloaded` in the index under one `kind='offload'` run. The indexer treats an offloaded row's on-disk absence as expected (it never becomes `missing`), and re-acquiring the bytes (restore or copy-back) flips the row back to `present`.

### Hooks

A volume can declare a per-volume **hook** — a command the agent runs to nudge an external tool when the volume's content changes. squirrel stays tool-agnostic: it never learns what the command does (a backup with kopia/restic, an `rclone copy`, a shell script — all the same to squirrel). It exec's the command **without a shell**, passes context through environment variables, and records only the generic outcome (exit code, timestamps). That generic outcome is the ceiling: only the built-in destination types report verification results; a hook's exit code never counts as one. (For kopia specifically, squirrel can own the snapshot end-to-end instead via a [kopia destination](#kopia-destinations).)

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

Sync verifies each uploaded file's BLAKE3 against the destination (using rclone's `--checksum --hash blake3`). Mismatches abort that file before the runs row is marked success. Use `--shallow` to fall back to rclone's default size+mtime comparison if you want speed over integrity for a big initial push. Encrypted (`crypt`) destinations always use the size+mtime comparison (see [Encrypted destinations](#encrypted-destinations)).

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
squirrel verify  [<destination>]
squirrel offload <volume> [path...]  [--older-than DUR] [--dry-run]
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

Each mirrored destination (`layout = "mirror"`, the default) is a tree shaped like the local volumes:

```
<dest.root>/
  pictures/
    2024/cat.jpg
    .squirrel-history/run-7/2024/cat.jpg     # prior content of cat.jpg
    .squirrel-index/index-20260604T120000.000Z-run-12.db   # global index snapshot (ride-along)
  docs/
    invoice.pdf
    .squirrel-history/run-9/invoice.pdf
```

`.squirrel-history/run-<run-id>/` is rclone's `--backup-dir` target for that sync run. It is filtered out of all subsequent comparisons so it does not grow rclone's listing time or get uploaded back. A directory literally called `.squirrel-history` in your source volume is also filtered (with a warning), to keep the reserved name out of the destination tree by accident.

`.squirrel-index/` holds the index snapshots ridden along after each successful sync (see [Index snapshots](#index-snapshots)). Like `.squirrel-history`, it is filtered out of all sync and restore transfers and from peer-sync, so a snapshot is never mistaken for user content.

A [content-addressed destination](#content-addressed-destinations) holds a shared `objects/` directory at its root and `index/` under each per-volume directory instead of a mirrored tree (plus the same `.squirrel-index/` ride-along).

## Notes

- Hash: BLAKE3-256 via `github.com/zeebo/blake3`. Stored as a 32-byte `BLOB` in the `blake3` column. The CLI accepts and prints hex.
- Storage: SQLite via the pure-Go `modernc.org/sqlite`. WAL mode is enabled at open. Schema version 10; older databases auto-migrate forward on first open.
- Symlinks are skipped during indexing.
- Sync runs do not pass `--delete-*` to rclone. Files removed locally remain at the destination.
- The `runs` table is never auto-pruned; the run history is an audit trail and any retention is explicit and operator-driven only.

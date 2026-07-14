---
title: CLI reference
description: Every squirrel command, subcommand, argument, and flag.
---

Every command inherits two persistent flags from the root command:

| Flag | Default | Meaning |
|---|---|---|
| `--config` | OS-resolved default (`~/.squirrel/config.toml`) | TOML config file path (env: `SQUIRREL_CONFIG`). |
| `--db` | from config, else `~/.squirrel/index.db` | SQLite database path; overrides `db` in config. |

Resolution precedence for the database path is `--db` > config `db` field >
`$HOME/.squirrel/index.db`.

## Command summary

```
squirrel index   <volume>            [--shallow] [--dry-run] [--workers N] [--progress]
squirrel sync    [<volume>]          [--to DEST] [--shallow] [--dry-run] [--init] [--progress]
squirrel verify  [<destination>]
squirrel offload <volume> [path...]  [--older-than DUR] [--dry-run]
squirrel query   [<hash-or-path>]    [--history] [--duplicates] [--missing] [--from NODE]
squirrel runs                        [--volume NAME] [--limit N]
squirrel runs fail <id>
squirrel hooks                       [--volume NAME] [--limit N]
squirrel volumes
squirrel restore <volume>            [--from NAME] [--to PATH] [--shallow] [--dry-run] [--in-place]
squirrel audit   [<volume>]          [--deep | --folders]
squirrel peer-sync history <volume> <peer>
squirrel peer-sync pull-durability <volume> <peer> [--allow-rewind]
squirrel db backup                   [--to PATH] [--keep N]
squirrel db check
squirrel db restore <snapshot>       [--force]
squirrel db schema
squirrel tui
squirrel agent
```

---

## squirrel

**Local content-addressed file indexer.**

A bare `squirrel` with no subcommand opens the [TUI](/squirrel/guides/tui/) when
both stdin and stdout are TTYs; otherwise it prints help.

---

## squirrel index

**Walk a config-declared volume, hash regular files, and update the index.**

```
squirrel index <volume>
```

Takes exactly one positional argument — a volume name declared in config.
Indexing by raw path is not supported. See [Indexing](/squirrel/guides/indexing/).

| Flag | Default | Meaning |
|---|---|---|
| `--shallow` | `false` | Skip rehash when `(size, mtime)` match the stored row. |
| `--dry-run` | `false` | Report what would change without writing to the database. |
| `--workers` | `0` (= `NumCPU()`) | Number of hashing workers. |
| `--progress`, `-P` | auto on a TTY | Show live progress; `--progress=false` forces off. |

---

## squirrel sync

**Push configured volumes to their rclone destinations.**

```
squirrel sync [<volume>]
```

Optional single positional. No arg = every `(volume, destination)` pair; one arg
= every destination for that volume; `--to` narrows to one pair. See
[Syncing & first use](/squirrel/guides/syncing/).

| Flag | Default | Meaning |
|---|---|---|
| `--to` | all | Limit to this destination name. |
| `--shallow` | `false` | Skip BLAKE3 verification; trust rclone's size+mtime comparison. |
| `--dry-run` | `false` | Preview rclone actions without transferring; no runs row is written. |
| `--init` | `false` | Authorise first-use destination bootstrap. |
| `--progress`, `-P` | auto on a TTY | Show live transfer progress (files, bytes, rate, ETA). |

---

## squirrel verify

**Re-check recorded offsite objects and packs against their upload fingerprints.**

```
squirrel verify [<destination>]
```

Optional single positional. If omitted, verifies every content-addressed or
packed destination in config (sorted). An explicit destination must have layout
`content-addressed` or `packed`, else it errors. No flags. See
[Offsite verification](/squirrel/guides/verification/).

---

## squirrel offload

**Delete local bytes whose content is durable on every required target.**

```
squirrel offload <volume> [path...]
```

First positional is the volume; remaining positionals are volume-relative file or
directory-prefix paths. At least one selector (a path or `--older-than`) is
required; a volume without an `offload_requires` policy is refused. See
[Offloading](/squirrel/guides/offloading/).

| Flag | Default | Meaning |
|---|---|---|
| `--older-than` | — | Only files whose indexed mtime is older than this duration (Go durations like `720h`, or whole days like `90d`). |
| `--dry-run` | `false` | Print the per-file durability gate decisions without deleting anything. |

---

## squirrel query

**Look up the index by hash, path, or list duplicates/missing.**

```
squirrel query [<hash-or-path>]
```

Optional single positional — a 64-char hex BLAKE3 digest or a path. (A 64-char
hex string that also exists on disk, or contains a path separator, is treated as
a path.) Requires one of: `<hash>`, `<path>`, `--duplicates`, `--missing`, or
`--from`.

| Flag | Default | Meaning |
|---|---|---|
| `--duplicates` | `false` | List hashes that appear at more than one path. Rejects a positional argument. |
| `--missing` | `false` | List previously-indexed paths no longer on disk. Rejects a positional argument. |
| `--history` | `false` | When querying a path, also print the full content history at that path. (Path queries only.) |
| `--from` | — | Restrict results to rows whose content originates at this node. |

---

## squirrel runs

**List index runs (most recent first).**

```
squirrel runs
```

Takes no arguments. See [Runs & the audit trail](/squirrel/concepts/runs/).

| Flag | Default | Meaning |
|---|---|---|
| `--volume` | all | Filter to runs against this volume name. |
| `--limit` | `20` | Maximum number of runs to show (`0` for no limit). |

### squirrel runs fail

**Mark a stuck running run as failed.**

```
squirrel runs fail <id>
```

Exactly one positional — a run id. Only runs currently in status `running` may be
failed; it writes a `manual-fail` audit row and preserves the running row's
file count. No flags.

---

## squirrel hooks

**List external-tool hook runs (most recent first).**

```
squirrel hooks
```

Takes no arguments. See [Hooks](/squirrel/guides/hooks/).

| Flag | Default | Meaning |
|---|---|---|
| `--volume` | all | Filter to hooks for this volume name. |
| `--limit` | `20` | Maximum number of hook runs to show (`0` for no limit). |

---

## squirrel volumes

**List known indexing volumes.**

```
squirrel volumes
```

Takes no arguments and has no flags. Output is `id<TAB>name<TAB>path` per line.

---

## squirrel restore

**Pull a volume back from one of its rclone destinations.**

```
squirrel restore <volume>
```

Exactly one positional — the volume name. See [Restoring](/squirrel/guides/restore/).

| Flag | Default | Meaning |
|---|---|---|
| `--from` | — | Destination name to pull from, or peer node name to filter by content origin. |
| `--to` | volume's declared path | Local target path. |
| `--shallow` | `false` | Skip BLAKE3 verification on the way down. |
| `--dry-run` | `false` | Preview rclone actions without transferring. |
| `--in-place` | `false` | Permit restore against a non-empty live path; overwritten files move to `.squirrel-restore-history/run-<id>/`. |

---

## squirrel audit

**Walk a volume looking for out-of-band drift since the last index.**

```
squirrel audit [<volume>]
```

Optional single positional; omitted = every declared volume (sorted). See
[Auditing for drift](/squirrel/guides/auditing/).

| Flag | Default | Meaning |
|---|---|---|
| `--deep` | `false` | Re-hash every file (bit-rot check) instead of the size+mtime shortcut. |
| `--folders` | `false` | Re-derive folder Merkle hashes from the index and report divergence (no disk walk). |

`--deep` and `--folders` are mutually exclusive.

---

## squirrel peer-sync

**Inspect node-sync state and exchange peer metadata.**

On its own it prints help. See [Peer sync](/squirrel/guides/peer-sync/).

### squirrel peer-sync history

**List the watermark transition log for a (volume, peer) pair.**

```
squirrel peer-sync history <volume> <peer>
```

Two positionals: volume then peer (a node name). Output oldest-first, columns
`AT` and `LAST_SHARED_RUN_ID`. No flags.

### squirrel peer-sync pull-durability

**Fetch a peer's destination durability vectors for a volume into the local index.**

```
squirrel peer-sync pull-durability <volume> <peer>
```

Two positionals: volume then peer (a node name from config).

| Flag | Default | Meaning |
|---|---|---|
| `--allow-rewind` | `false` | Accept peer components below the locally recorded value (recovery override). |

---

## squirrel db

**SQLite index hygiene: backup, integrity check, restore.**

Parent command; prints help with no subcommand. See
[Index snapshots](/squirrel/configuration/index-snapshots/).

### squirrel db backup

**Write a consistent online snapshot of the index database.**

```
squirrel db backup
```

| Flag | Default | Meaning |
|---|---|---|
| `--to` | `~/.squirrel/backups/index-<ISO8601>.db` | Snapshot destination path. |
| `--keep` | `0` | After writing, rotate the backups directory to keep at most N snapshots (`0` = no rotation). |

### squirrel db check

**Run `PRAGMA integrity_check` on the index database.**

```
squirrel db check
```

No flags. Prints `ok` when clean; otherwise prints each issue line and exits
non-zero.

### squirrel db restore

**Replace the live index database with a previously taken snapshot.**

```
squirrel db restore <snapshot>
```

Exactly one positional — the snapshot path. Refuses if the snapshot path equals
the live DB path, or if the snapshot schema version differs from the binary's
schema version.

| Flag | Default | Meaning |
|---|---|---|
| `--force` | `false` | Skip the running-agent check; required when another process legitimately holds the DB open. |

### squirrel db schema

**Print the index database's DDL (tables, indexes, triggers).**

```
squirrel db schema
```

No flags. Opens the DB (running the normal migration chain first), then prints
the flattened DDL.

---

## squirrel tui

**Open the interactive terminal UI.**

```
squirrel tui
```

No arguments, no flags. Always opens the TUI regardless of TTY state. See
[Terminal UI](/squirrel/guides/tui/).

---

## squirrel agent

**Run the squirrel agent (HTTP server + scheduled audits + cadence-driven index/sync).**

```
squirrel agent
```

No arguments, no flags. Requires an `[agent]` block in config. See
[The agent](/squirrel/guides/agent/).

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
squirrel status  [<volume>]
squirrel verify  [<destination>]
squirrel verify ack <destination>
squirrel offload <volume> [path...]  [--older-than DUR] [--dry-run] [--per-file]
squirrel query   [<hash-or-path>]    [--history] [--duplicates] [--missing] [--from NODE]
squirrel runs                        [--volume NAME] [--limit N] [--failed] [--changes]
squirrel runs fail <id>
squirrel hooks                       [--volume NAME] [--limit N]
squirrel volumes
squirrel conflicts
squirrel conflicts resolve <volume> <path>
squirrel restore <volume>            [--from NAME] [--to PATH] [--shallow] [--dry-run] [--in-place]
squirrel destination reset <dest>    [--yes] [--dry-run]
squirrel audit   [<volume>]          [--deep | --folders]
squirrel config check
squirrel node pair <peer>            [--local-endpoint URL] [--peer-endpoint URL]
                                     [--peer-fingerprint sha256:HEX] [--peer-path PATH]
squirrel peer-sync history <volume> <peer>
squirrel peer-sync pull-durability <volume> <peer> [--allow-rewind]
squirrel db backup                   [--to PATH] [--keep N]
squirrel db check
squirrel db restore <snapshot>       [--force]
squirrel db schema
squirrel tui
squirrel agent
squirrel agent cert                  [--force]
squirrel version
```

Commands split into two families ([UX principle 2](https://github.com/mbertschler/squirrel/blob/main/design/ux-principles.md)):
**introspection** (`status`, `runs`, `query`, `volumes`, `hooks`, `conflicts`,
`config check`, `peer-sync history`, `db schema`, `db check`, `tui`) is safe at
any time and mutates nothing; **change** (`sync --init`, `offload`, `restore`,
`destination reset`, `conflicts resolve`, `verify ack`, `db restore`,
`agent cert`, `node pair`) is deliberate and typed by hand on purpose.

[`verify`](#squirrel-verify) straddles the two on purpose, and is listed in
neither: it is read-only *against the remote*, but writes what it learns
locally — recording fingerprints, upgrading a durability vector, and latching
an alarm on a mismatch. It is a question whose answer squirrel keeps.

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

## squirrel status

**Show per-target sync coverage and durability, and the offload-ready total.**

```
squirrel status [<volume>]
```

Optional single positional; omitted = every declared volume. No flags. The
read-only "am I safe?" answer: per volume, when it was last indexed, then one
row per configured target with its role, last sync, state, durability coverage,
verify method, and evidence age — plus the volume's offload readiness.

```
docs  /home/you/Documents  [amber]
  index: 8s ago
  TARGET  ROLE  LAST SYNC  STATE         DURABLE  METHOD  EVIDENCE
  nas     sync  never      never-synced  —        —       —
  offload: no policy
overall: amber
```

When the [agent](/squirrel/guides/agent/) has noticed its config file change on
disk since it started, a line above the grid says so and holds the report at
amber until the agent is restarted:

```
config on disk has changed since this agent started; restart to apply (/home/you/.squirrel/config.toml, noticed 12m ago)
```

The **exit code** carries the worst level, so the same command scripts a health
check without parsing the grid:

| Exit | Level | Meaning |
|---|---|---|
| `0` | green / neutral | Caught up within cadence and durable where policy requires; nothing to report. |
| `1` | amber | Not caught up yet, needs a one-time bootstrap, evidence aged past policy, or the agent's config has drifted from the file on disk. Recoverable and expected. |
| `2` | red | A latched alarm, a failed or regressed sync, or a pair far past its cadence. |

Passing a volume name scopes both the grid and the exit code to that volume, so
a per-volume check is not reddened by an unrelated one. Config drift is the one
exception: it is node-wide — every volume's paths, targets, and cadences came
out of that file — so it is reported and counted on every scope. The TUI dashboard
renders the same facts from the same query layer — see
[Terminal UI](/squirrel/guides/tui/).

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

A clean pass does two things beyond reporting: it fills any pending fingerprints
and then **upgrades the destination's durability vector to a content-verified
method**, which is what lets [`offload`](#squirrel-offload) accept it. A mismatch
**latches a standing alarm** on the destination that survives the run and shows
on every surface until acknowledged.

### squirrel verify ack

**Acknowledge and clear a standing verify alarm on a destination.**

```
squirrel verify ack <destination>
```

Exactly one positional — the destination name. No flags. Clears the latch raised
by a verify mismatch so the destination stops showing red; the raise and the
clear both survive in the audit trail with the operator recorded. A subsequent
clean verify pass also clears the alarm on its own.

---

## squirrel offload

**Delete local bytes whose content is durable on every required target.**

```
squirrel offload <volume> [path...]
```

First positional is the volume; remaining positionals are volume-relative file or
directory-prefix paths. At least one selector (a path or `--older-than`) is
required; a volume without an `offload_requires` policy is refused. Gate refusals
are reported in aggregate — grouped by target and cause, with each required
target's satisfied count — see
[Reading a refusal](/squirrel/guides/offloading/#reading-a-refusal).

| Flag | Default | Meaning |
|---|---|---|
| `--older-than` | — | Only files whose indexed mtime is older than this duration (Go durations like `720h`, or whole days like `90d`). |
| `--dry-run` | `false` | Report the durability gate decisions without deleting anything. |
| `--per-file` | `false` | Also list every blocked file with its own gate reasons; refusals are aggregated by target and cause otherwise. |

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

**List runs of every kind (most recent first).**

```
squirrel runs
```

Takes no arguments. Lists every kind of run — `index`, `sync`, `restore`,
`audit`, `offload` — not just index runs. (`squirrel verify` is recorded under
`audit`, not as a kind of its own.) See
[Runs & the audit trail](/squirrel/concepts/runs/).

| Flag | Default | Meaning |
|---|---|---|
| `--volume` | all | Filter to runs against this volume name. |
| `--limit` | `20` | Maximum number of runs to show (`0` for no limit). |
| `--failed` | `false` | Show only runs that need attention: `failed`, `refused`, `aborted`, or `partial`. |
| `--changes` | `false` | Hide clean no-op runs; show only runs that moved content or need attention. |

Under household cadences most rows are no-ops (a pair checked, nothing to do).
`--changes` keys on the count of files a run actually changed, so it folds away
bucket pushes and index runs too, not just peer-sync no-ops. Runs recorded
before that count existed are shown rather than silently folded — their change
count is genuinely unknown.

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

## squirrel conflicts

**List unresolved contested paths (frozen peer-sync conflicts).**

```
squirrel conflicts
```

Takes no arguments and has no flags. When the same path is edited on two
machines between cadences, the receiver preserves the loser under
`.squirrel-conflicts/` and raises a **contested freeze** on the path. While the
freeze stands, a divergent re-assertion from any peer is refused instead of
minting another conflict copy — the ping-pong stops at the first conflict. Both
versions stay reachable: the winner is live, the loser is on disk.

```
no contested paths — nothing frozen
```

Every node mirrors the freeze into its own index, so the losing edge machine
sees it too, not just the hub. See [Peer sync](/squirrel/guides/peer-sync/).

### squirrel conflicts resolve

**Clear a contested freeze so syncs flow again (explicit human act).**

```
squirrel conflicts resolve <volume> <path>
```

Two positionals: volume then the volume-relative path. No flags. Clearing the
latch lets syncs resume for that path; the raise and the clear are both recorded
with the operator's name. Squirrel never resolves a conflict on its own.

Resolve does **not** choose a version. It unfreezes the path with the current
winner live. Adopting the preserved version instead is a deliberate
[`restore`](#squirrel-restore), never a side effect of resolving.

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

Restore from an [encrypted (`crypt`)](/squirrel/layouts/encrypted/) destination
is always a size+mtime comparison (recorded shallow) even without `--shallow`,
because rclone crypt remotes don't expose content hashes.

---

## squirrel destination

**Manage a destination's recorded state.**

On its own it prints help. See [Recovery & disaster runbooks](/squirrel/guides/recovery/).

### squirrel destination reset

**Forget a destination's recorded upload and durability state (audit-preserving).**

```
squirrel destination reset <destination>
```

One positional — the destination name. Clears the `remote_objects`/`remote_packs`
upload ledgers, the durability vector, and the push-freshness rows for the
destination, so the next sync treats it as fresh and re-uploads. The runs table
and the append-only durability advance log are preserved, and the reset is
recorded as an audit run; the remote bytes are untouched.

| Flag | Default | Meaning |
|---|---|---|
| `--yes` | `false` | Confirm the reset; required to actually clear state. |
| `--dry-run` | `false` | Print what would be cleared without changing anything. |

Use it to recover a wrecked or repointed destination — see
[Resetting a wrecked destination](/squirrel/guides/recovery/#resetting-a-wrecked-destination).

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

## squirrel config

**Inspect the squirrel configuration.**

On its own it prints help.

### squirrel config check

**Parse and resolve the config, stat its paths, and report what it declares.**

```
squirrel config check
```

Takes no arguments and has no flags. The first command to run after writing or
editing a config: it reads the *config file*, not the database, so a declared
but never-indexed volume still shows up. Each volume, destination, and node is
stated with its resolved path and an `ok` or a flagged problem, and the summary
line affirms what was found.

```
config: ~/.squirrel/config.toml

volumes (1)
  ok    docs  /home/you/Documents
destinations (1)
  ok    nas  local mirror
nodes (0)

1 volumes, 1 destinations, 0 nodes — all resolvable
```

It also stats each node's byte-path and flags one that is missing, catching the
out-of-band mount assumption before bytes silently fail to land.

---

## squirrel node

**Manage peer node relationships.**

On its own it prints help. See [Peer sync](/squirrel/guides/peer-sync/).

### squirrel node pair

**Emit matching config halves (tokens, endpoints, fingerprints) for a peer relationship.**

```
squirrel node pair <peer>
```

Exactly one positional — the peer's node name. A single peer relationship needs
four token bindings across two machines' config files, each side's
`[agent.auth.peers.X]` matching the other side's `[nodes.Y].auth.bearer`.
Writing those by hand is the most error-prone part of bootstrap, and nothing
catches a mismatch until a sync fails with `401`. This generates both halves so
they match by construction.

| Flag | Default | Meaning |
|---|---|---|
| `--local-endpoint` | — | This node's agent endpoint as the peer dials it (e.g. `https://nas.home:8443`). |
| `--peer-endpoint` | — | The peer's agent endpoint. |
| `--peer-fingerprint` | — | The peer's TLS cert fingerprint (`sha256:…`), as printed by [`agent cert`](#squirrel-agent-cert). |
| `--peer-path` | — | The byte-path where this node mounts the peer's data. |

It **emits** config; it never edits a file. Paste each half into the machine it
names.

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

A machine that only runs cadences and never receives from a peer does not need a
listener: omit `listen` and the agent runs listener-less, with no HTTP surface
and no auth token to configure.

### squirrel agent cert

**Generate the agent's self-signed TLS cert+key and print its `sha256:` pin.**

```
squirrel agent cert
```

No positionals. Writes the cert and key to the paths in `[agent.tls]` and prints
the fingerprint peers must pin — the same `sha256:<hex>` string
[`node pair`](#squirrel-node-pair) takes as `--peer-fingerprint`. Certificate
pinning is the documented trust anchor between agents; this removes the openssl
incantations that used to be the only way to produce its ingredients.

| Flag | Default | Meaning |
|---|---|---|
| `--force` | `false` | Overwrite an existing cert/key. This changes the fingerprint — **every peer must re-pin**. |

---

## squirrel version

**Print the squirrel version and build information.**

```
squirrel version
```

No arguments, no flags. Prints the release, commit, build time, Go version, and
platform, so a household can confirm which build a machine is running:

```
squirrel v1.2.3
  commit:   a1b2c3d
  built:    2026-07-24T10:00:00Z
  go:       go1.26.1
  platform: linux/amd64
```

A source build reports `0.0.0-dev` — the version is stamped only into released
binaries. The agent reports the same string on `GET /v1/health`.

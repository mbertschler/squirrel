---
title: Configuration reference
description: Every config key — top-level, volumes, hooks, destinations (all types), crypt, content-addressed/packed knobs, and backups — in one place.
---

Squirrel reads a single TOML file (`~/.squirrel/config.toml` by default; override
with `--config` or `$SQUIRREL_CONFIG`). Unknown fields, missing required fields,
and unset environment variables are rejected at load time.

Any secret field accepts either a literal string or an inline `{ env = "VAR" }`
table resolved at load time.

## Top level

| Key | Required | Meaning |
|---|---|---|
| `db` | no | Path to the SQLite index database. Default `~/.squirrel/index.db`. |

## `[volumes.<name>]`

| Key | Required | Meaning |
|---|---|---|
| `path` | yes | Absolute (or `~`-relative) root directory of the volume. |
| `sync_to` | no | List of destination names to push to. |
| `offload_requires` | no | List of targets whose durability must cover a file before its local bytes may be [offloaded](/squirrel/guides/offloading/). A volume without this key refuses to offload. |
| `offload_max_evidence_age` | no | Duration; a target whose durability evidence was last re-verified longer ago than this refuses the offload. Default disabled. |

### `[volumes.<name>.hook]`

| Key | Required | Meaning |
|---|---|---|
| `command` | yes | Argv list, exec'd without a shell (e.g. `["kopia", "snapshot", "create", "."]`). |
| `timeout` | no | Per-invocation timeout. Default `1h`. |
| `interval` | no | Also fire on this cadence, regardless of change. Omit for on-change only. |

See [Hooks](/squirrel/guides/hooks/).

## `[destinations.<name>]`

### Common keys

| Key | Applies to | Meaning |
|---|---|---|
| `type` | all | `local`, `sftp`, `s3`, `b2`, `gcs`, or `kopia`. |
| `root` | all | Base path (or bucket sub-path) at the destination. |
| `layout` | rclone remotes | `mirror` (default), `content-addressed`, or `packed`. |

### `local`

| Key | Meaning |
|---|---|
| `root` | Local directory. Requires a `.squirrel-volume` marker (see [first use](/squirrel/guides/syncing/)). |

### `sftp`

| Key | Meaning |
|---|---|
| `host` | Server hostname. |
| `user` | SSH user. |
| `password` | Secret (literal or `{ env }`). |
| `root` | Base path on the server. |
| `known_hosts_file` | Path to a known_hosts file; **validates the server host key** (recommended). Without it, rclone connects to whatever host answers. |
| `host_key_algorithms` | Space-separated list pinning accepted host-key algorithms (optional). |

### `s3`

| Key | Meaning |
|---|---|
| `provider` | rclone s3 provider (e.g. `AWS`). |
| `region` | Bucket region. |
| `bucket` | Bucket name. |
| `root` | Sub-path within the bucket. |
| `access_key_id` | Secret. |
| `secret_access_key` | Secret. |
| `storage_class` | Optional; maps to rclone's s3 `storage_class`. Use the exact value your provider documents. |

### `b2` / `gcs`

rclone-backed object stores; provide the credentials rclone's b2/gcs backends
expect (as literal or `{ env }` secrets) plus `root`.

### `kopia`

| Key | Meaning |
|---|---|
| `root` | Kopia repository path. |
| `password` | Repository password (passed to kopia via environment). |

A `kopia` destination rejects a `crypt` block, `--dry-run`, and `--shallow`. See
[Kopia](/squirrel/layouts/kopia/).

## `[destinations.<name>.crypt]`

Client-side content encryption via rclone's crypt overlay (any non-`local`
destination). See [Encrypted (crypt)](/squirrel/layouts/encrypted/).

| Key | Meaning |
|---|---|
| `password` | Plaintext encryption password; squirrel obscures it at render time. Secret. |
| `password2` | Plaintext salt; optional but recommended. Secret. |
| `obscured` | `true` if `password`/`password2` are already rclone-obscured (renders them verbatim; default `false`). |

Filenames are **not** encrypted (`filename_encryption = off`, fixed by design).

## Content-addressed & packed knobs

For `layout = "content-addressed"` or `layout = "packed"` destinations. See
[Content-addressed](/squirrel/layouts/content-addressed/) and
[Packed](/squirrel/layouts/packed/).

| Key | Applies to | Default | Meaning |
|---|---|---|---|
| `hash_algo` | sftp | `sha256` | Which server-side hash the [scan-back fingerprint](/squirrel/guides/verification/) uses. |
| `checkers` | rclone remotes | rclone default | Cap rclone's concurrent checkers (`--checkers`). |
| `force_path_style` | s3 | `false` | Path-style bucket addressing for squirrel's own ETag reader (not the rclone transport). |
| `pack_threshold` | packed | — | Files smaller than this are packed; at/above land as objects (e.g. `1MiB`). |
| `pack_size` | packed | — | Target size of one pack before it is closed (e.g. `512MiB`). |
| `zstd_level` | packed | — | zstd compression level, `1` fastest … `4` best. |
| `verify_every` | content-addressed / packed | off | Cadence for the agent to re-check this destination's recorded objects and packs against their upload fingerprints — the same pass as [`squirrel verify`](/squirrel/guides/verification/), recorded as an `audit` run. Falls back to `[agent] verify_every`. Read-only; the agent never writes to the destination. |

## `[backups]`

Automatic [index snapshots](/squirrel/configuration/index-snapshots/). Absent
table = enabled with these defaults.

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `true` | Local snapshot-on-sync. `false` disables both halves. |
| `dir` | `<dir of db>/backups` | Local snapshot directory. |
| `keep` | `7` | Local snapshots kept (rotation; `0` = keep all). |
| `cloud` | `true` | Ride a copy along to destination buckets. |
| `cloud_keep` | `7` | Snapshots kept per `<dest>/<volume>/.squirrel-index/` (`0` = keep all). |

## `[agent]`

Required to run [`squirrel agent`](/squirrel/guides/agent/). Configures the
unattended cadence (`index_every`, `sync_every`), scheduled audits, and an
optional agent-specific `db` path (which takes precedence over the top-level `db`
when the agent runs).

`listen` is optional. When set (e.g. `0.0.0.0:8443`), the agent binds an HTTP
server for peer syncs and the health endpoint, and a bearer token is required.
When omitted, the agent runs its schedulers only — the *listener-less* mode for
cadence-only machines that never receive peer syncs; `[agent.auth]` is then
optional. See [The agent](/squirrel/guides/agent/#listener-less-cadence-only-machines).

| Key | Required | Meaning |
|---|---|---|
| `listen` | no | Bind address, e.g. `0.0.0.0:8443`. Omit for listener-less, scheduler-only mode (above). |
| `db` | no | Agent-specific index path; wins over the top-level `db`. |
| `auth.token` | with `listen` | Shared bearer token. Required only when the agent binds an HTTP surface. |
| `scan_interval` | no | Drift-scan cadence over every hosted volume. Off when absent. |
| `scan_strategy` | no | `shallow` (default) or `deep` (re-hash everything — bit-rot detection). |
| `verify_every` | no | Fleet-wide default verify cadence applied to every content-addressed/packed destination that declares no `verify_every` of its own. Off when absent. |

## `[nodes.<name>]`

A peer that runs its own agent. See [Peer sync](/squirrel/guides/peer-sync/).

| Key | Required | Meaning |
|---|---|---|
| `endpoint` | yes | Peer's sync-API URL, e.g. `https://nas.home:8443`. |
| `path` | only if synced to | rclone target prefix bytes are copied into. Required when some volume's `sync_to` names this node; omit it for a peer this machine only pulls durability evidence from. See below. |
| `auth.bearer` | yes | Bearer token this node presents to the peer. |
| `tls.cert_fingerprint` | no | `sha256:<hex>` pin for the peer's self-signed cert. |
| `dedup_strategy` | no | `copy` (default) or `off`. |
| `pull_durability_every` | no | Cadence for the agent to pull this peer's durability vectors (the same merge as [`squirrel peer-sync pull-durability`](/squirrel/guides/peer-sync/)), giving evidence freshness its own clock. Lets a **receive-only** node (one this machine never syncs *to*) keep its offload-gate evidence fresh unattended. The agent never rewinds a watermark. Off when absent. |

### The byte-path is an out-of-band contract

`path` is where *this* machine writes bytes for the peer — an SMB/NFS mount
point, or an rclone remote spec like `nasremote:backups`. It is deliberately
separate from `endpoint`: the sync API and the filesystem the bytes travel
over are independent, and often not even the same protocol.

That independence is also its weakness, so it is worth being precise about
what squirrel can and cannot tell you:

- **It can tell you the local end is wrong.** A `path` that does not exist
  or is not a directory shows as an amber `byte-path` state against that
  target in [`squirrel status`](/squirrel/reference/cli/) and the TUI, and
  is flagged by `squirrel config check`. Amber rather than red: the usual
  cause is a mount that has not come up yet, which fixes itself.
- **It cannot tell you the path is the *right* one.** A directory that
  exists but is not actually the peer's storage looks identical to a
  correct one from here. Bytes will land somewhere and the peer will never
  see them. Nothing in squirrel closes that gap — verify it once when you
  set the peer up.

**Omit `path` entirely when no bytes travel.** A peer you only pull
durability evidence from — the receive-only relationship
`pull_durability_every` exists for — moves nothing through a byte-path, and
requiring one would just mean inventing a value to satisfy the validator.
Squirrel asks for a `path` exactly when some volume's `sync_to` names the
node, and rejects the config at load time if it is missing there:

```
nodes.nas: path is required because volumes.pictures syncs to it (rclone target prefix the initiator copies bytes into)
```

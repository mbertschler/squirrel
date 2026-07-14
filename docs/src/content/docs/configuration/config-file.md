---
title: Config file & volumes
description: Squirrel is configured via a TOML file declaring the index database, volumes, and their destinations. Nothing is implicit.
---

Squirrel is configured via a TOML file at `~/.squirrel/config.toml`. Override the
location with `--config <path>` or the `SQUIRREL_CONFIG` environment variable.

Every volume and destination squirrel touches must be declared there — there is
no implicit "just point at a directory" mode. Unknown fields, missing required
fields, and unset environment variables are rejected immediately.

## A minimal config

```toml
db = "~/.squirrel/index.db"

[volumes.pictures]
path    = "~/Pictures"
sync_to = ["nas", "offsite"]

[volumes.docs]
path    = "~/Documents"
sync_to = ["nas"]
```

## The database

```toml
db = "~/.squirrel/index.db"
```

`db` is the path to the SQLite index database. It is created on first use and
migrated forward on every open. Resolution precedence for the database path is:

1. the `--db` flag,
2. the `db` field in config,
3. the built-in default `$HOME/.squirrel/index.db`.

Storage is SQLite via the pure-Go `modernc.org/sqlite`; WAL mode is enabled at
open.

## Volumes

A **volume** is a named local directory that squirrel indexes and syncs. Each
volume declares a `path` and, optionally, the destinations it syncs to.

```toml
[volumes.pictures]
path    = "~/Pictures"
sync_to = ["nas", "offsite"]
```

| Key | Required | Meaning |
|---|---|---|
| `path` | yes | Absolute (or `~`-relative) path to the volume's root directory. |
| `sync_to` | no | List of destination names to push to. A volume with no `sync_to` can still be indexed and can drive a [hook](/squirrel/guides/hooks/). |
| `offload_requires` | no | Per-volume [offload](/squirrel/guides/offloading/) policy — targets whose durability must cover a file before its local bytes may be deleted. |
| `offload_max_evidence_age` | no | Optional staleness bound on offload durability evidence. |
| `[volumes.<name>.hook]` | no | A per-volume [hook](/squirrel/guides/hooks/) command. |

List the volumes squirrel knows about:

```sh
squirrel volumes
```

Symlinks are skipped during indexing.

## Precedence & environment

- `--config <path>` or `$SQUIRREL_CONFIG` overrides the default config location.
- `--db <path>` overrides the `db` field for a single invocation.

## Next steps

- [Destinations & secrets](/squirrel/configuration/destinations/) — declare where
  volumes sync to.
- [Index snapshots](/squirrel/configuration/index-snapshots/) — automatic
  redundancy for the catalog itself.
- [Configuration reference](/squirrel/reference/configuration/) — every config
  key in one place.

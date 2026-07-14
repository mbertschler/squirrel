---
title: Quickstart
description: Index a configured volume, sync it to its destinations, look up content by hash or path, and open the TUI.
---

This walkthrough assumes you have a config file declaring at least one volume and
destination — see [Config file & volumes](/squirrel/configuration/config-file/)
for the minimal setup.

## Index a volume

```sh
squirrel index pictures
```

Re-running `squirrel index` updates the index **incrementally** — new files are
added, modified files re-hashed, and files no longer on disk are flagged as
missing (rows are not deleted).

- `--shallow` skips re-hashing files whose `(size, mtime)` already match the
  stored row.
- `--dry-run` shows what would change without writing to the database.

See [Indexing](/squirrel/guides/indexing/) for details.

## Sync a volume to its destinations

```sh
squirrel sync pictures              # all destinations declared on pictures
squirrel sync pictures --to nas     # just one
squirrel sync                       # every (volume, destination) pair in config
```

Sync verifies each uploaded file's BLAKE3 against the destination (using
rclone's `--checksum --hash blake3`). Mismatches abort that file before the run
is marked success. Use `--shallow` to fall back to a size+mtime comparison for a
big initial push.

:::caution[First use needs `--init`]
Destinations that need first-use setup (a `local` marker, a new kopia
repository) must be bootstrapped once with `--init`. Without it squirrel refuses
to create anything — this keeps "create a new empty target" a human-driven act.
See [Syncing & first use](/squirrel/guides/syncing/).
:::

## Look up content

By BLAKE3 hex hash:

```sh
squirrel query 26e70f0a438787ee143979a9b519a4a330ea21e0a23d31fcb47051e70b8fe5ad
```

By path:

```sh
squirrel query ~/Pictures/foo.jpg
```

List duplicates, missing paths, or the full content history at a path:

```sh
squirrel query --duplicates
squirrel query --missing
squirrel query --history ~/Pictures/foo.jpg
```

## List recent runs

```sh
squirrel runs
squirrel runs --volume pictures --limit 5
```

## Open the terminal UI

Watch live runs, browse the index ncdu-style, and drill into run records:

```sh
squirrel tui
squirrel        # bare invocation opens the TUI when stdin/stdout are a terminal
```

## What's next

- [Destinations & secrets](/squirrel/configuration/destinations/) — configure
  NAS, S3, B2, GCS, SFTP.
- [Destination layouts](/squirrel/layouts/mirror/) — mirror, encrypted, kopia,
  content-addressed, packed.
- [Offloading](/squirrel/guides/offloading/) — reclaim local space once content
  is durable offsite.

---
title: Index snapshots
description: After every successful sync, squirrel snapshots the whole index to a local tier and rides a copy along to destination buckets. On by default, zero-config.
---

The catalog should be as redundant as the data it describes. After every
successful sync, squirrel takes one `VACUUM INTO` snapshot of the whole index (a
self-contained, `db check`-able `.db` file) to a **local tier** and — for
destination (bucket/sftp/…) syncs — rides a copy **along to the destination**,
under each synced volume's `.squirrel-index/`.

A restore-from-cloud then yields the data *and* the index that explains it.

## On by default

This is **on by default, zero-config** — an absent `[backups]` table means it is
enabled with the defaults below.

```toml
[backups]
enabled    = true   # local snapshot-on-sync (default true)
dir        = ""     # local snapshot directory (default: <dir of db>/backups)
keep       = 7      # local snapshots kept (rotation; 0 = keep all)
cloud      = true   # ride a copy along to destination buckets (default true)
cloud_keep = 7      # snapshots kept per <dest>/<volume>/.squirrel-index/ (0 = keep all)
```

- `enabled = false` disables **both** halves.
- `cloud = false` keeps the local snapshot but uploads nothing.

## Naming and behavior

Snapshots are named `index-<ISO8601>-run-<id>.db` — lexically sortable and
traceable to the run that produced them. A single snapshot is taken per
`squirrel sync` invocation and fanned out to every target; a snapshot or upload
failure is surfaced as a warning but **never fails the sync**.

The `.squirrel-index/` directory is filtered out of all sync and restore
transfers and from peer-sync, so a snapshot is never mistaken for user content.

:::caution[Privacy]
The ride-along payload is the *full global* `index.db` — paths and BLAKE3 hashes
for **all** volumes (never file contents). It lands in the same bucket as your
data (the same trust boundary). Use a private bucket and server-side encryption.
:::

## Manual snapshots

You can take, check, and restore index snapshots on demand with the
[`squirrel db`](/squirrel/reference/cli/#squirrel-db) subcommands:

```sh
squirrel db backup --to /path/snapshot.db --keep 7
squirrel db check
squirrel db restore /path/snapshot.db
```

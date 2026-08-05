---
title: Quickstart
description: Index a configured volume, sync it to its destinations, look up content by hash or path, and open the TUI.
---

This walkthrough assumes you have a config file declaring at least one volume and
destination — see [Config file & volumes](/squirrel/configuration/config-file/)
for the minimal setup.

## Check the config first

```sh
squirrel config check
```

The first thing to run after writing a config. It reads the *config file* rather
than the database, so a volume you have declared but never indexed still shows
up, and it tells you affirmatively what it found:

```
1 volumes, 1 destinations, 0 nodes — all resolvable
```

That matters because `squirrel volumes` reads the database — before your first
index it prints an empty list and exits `0`, which looks identical to "nothing
configured".

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

## Ask whether you are safe

```sh
squirrel status                 # every volume
squirrel status pictures        # just one
```

Per volume, one row per configured target: last sync, state, durability, verify
method, and evidence age — plus whether anything is offload-ready. The exit code
is the worst level found (`0` green, `1` amber, `2` red), so the same command
works as a scripted health check.

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
squirrel runs --failed              # only failed, refused, aborted, or partial
squirrel runs --changes             # hide clean no-ops
```

Once the agent is running its cadences, most rows are no-ops. `--changes` and
`--failed` are how you keep the audit trail readable.

## Open the terminal UI

The trust surface: standing alarms, contested paths, live runs, and the
per-target coverage grid — plus an ncdu-style index browser.

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

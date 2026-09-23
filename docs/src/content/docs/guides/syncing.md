---
title: Syncing & first use
description: Push configured volumes to their destinations with checksum verification, and bootstrap first-use destinations safely with --init.
---

`squirrel sync` pushes configured volumes to their destinations.

```sh
squirrel sync pictures              # all destinations declared on pictures
squirrel sync pictures --to nas     # just one
squirrel sync                       # every (volume, destination) pair in config
```

- No argument → sync every `(volume, destination)` pair in config.
- One argument → every destination declared on that volume.
- `--to <dest>` → narrow to a single pair.

## Verification

A mirror on a `local` disk is written by squirrel itself: it hashes every file
with BLAKE3 as it streams out, confirms each written path's size and mtime, and
records the run with the `presence+size` method (see
[Mirror](/squirrel/layouts/mirror/)).

Sync compares every file with its copy on a remote mirror destination by
checksum (rclone's `--checksum`), under the first hash both ends support — MD5
on S3, independent of the BLAKE3 in the index. A copy that fails the check after
transfer is an error, so the runs row is **not** marked success. The run is
recorded with the `checksum` method.

The offload gate accepts neither method, so a mirror cannot back an offload.
Peer syncs are different: both ends hash every byte with BLAKE3 (see
[Peer sync](/squirrel/guides/peer-sync/)).

Use `--shallow` to fall back to rclone's default size+mtime comparison on a
remote mirror if you want speed over integrity for a big initial push. A local
mirror refuses it: there is no comparison to switch off. Encrypted
([`crypt`](/squirrel/layouts/encrypted/)) destinations always use the size+mtime
comparison, and content-addressed/packed destinations use presence+size plus the
[scan-back fingerprint](/squirrel/guides/verification/).

Sync never deletes at a destination: files removed locally remain there.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--to <dest>` | all | Limit to this destination name. |
| `--shallow` | off | Skip the checksum comparison on a remote mirror; trust rclone's size+mtime comparison. Refused on a local mirror. |
| `--dry-run` | off | Preview what a push would transfer without transferring; no runs row is written. |
| `--init` | off | Authorise first-use destination bootstrap (see below). |
| `--progress`, `-P` | auto on a TTY | Show a live transfer progress line (files, bytes, rate, ETA). |

## First use and the `.squirrel-volume` marker

Destinations that need first-use setup must be bootstrapped **once** with
`--init`; without it squirrel refuses to create anything:

```sh
squirrel sync pictures --to mirror --init   # first time only
squirrel sync pictures --to mirror          # every time after
```

`--init` authorises the one-time first-use setup, by destination type:

- **`local` and remote rclone** (`sftp`, `s3`, `b2`, `gcs`) — write a
  `.squirrel-volume` marker under the destination's volume directory (on the
  filesystem for local, over rclone through the same overlay the transfer uses
  for remotes). Every later sync **requires** that marker and refuses if it is
  missing (a missing marker after the fact almost always means the root is wrong
  — an unmounted disk, a typo, or an unreachable remote). A marker that names a
  *different* volume is always refused, with or without `--init`. This holds
  across the mirror, content-addressed, and packed layouts: the marker sits at
  the volume root regardless of layout, and is filtered out of every
  transfer, comparison, and restore.
- **`kopia`** — permits `kopia repository create` when connecting finds no
  repository.

### Why a flag rather than auto-create

A missing marker (or a missing kopia repository) is ambiguous — it could mean
"genuinely new" or "the destination I expect is unreachable right now."
Auto-creating in the second case would mint a fresh **empty** target, record it
as durable, and — once [`offload`](/squirrel/guides/offloading/) trusts that
durability — let it delete the only local copy.

Requiring `--init` keeps that irreversible "create a new target" step a one-time,
human-driven act. In particular, **the agent/scheduler never passes `--init`**,
so an unattended sync can never silently create an empty target on a transient
outage.

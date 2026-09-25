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
with BLAKE3 as it streams out, reads each copy back through BLAKE3 before
committing it, and records the run as `fingerprint-verified` once every file
has such a copy (see [Mirror](/squirrel/layouts/mirror/#verification)). That
mirror can back an offload. A native mirror on `sftp` reads nothing back, so
its runs stay `presence+size` and cannot.

Sync compares every file with its copy on an rclone mirror destination by
checksum (rclone's `--checksum`), under the first hash both ends support — MD5
on S3, independent of the BLAKE3 in the index. A copy that fails the check after
transfer is an error, so the runs row is **not** marked success. The run is
recorded with the `checksum` method, which the offload gate refuses.

Peer syncs are different: both ends hash every byte with BLAKE3 (see
[Peer sync](/squirrel/guides/peer-sync/)).

Use `--shallow` to fall back to rclone's default size+mtime comparison on an
rclone mirror if you want speed over integrity for a big initial push. A
[native mirror](/squirrel/layouts/mirror/#how-a-mirror-is-written) (`local`, or
`sftp` without crypt) refuses it: there is no comparison to switch off. Encrypted
([`crypt`](/squirrel/layouts/encrypted/)) destinations always use the size+mtime
comparison, and content-addressed/packed destinations use presence+size plus the
[scan-back fingerprint](/squirrel/guides/verification/).

Sync never deletes at a destination: files removed locally remain there.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--to <dest>` | all | Limit to this destination name. |
| `--shallow` | off | Skip the checksum comparison on an rclone mirror; trust rclone's size+mtime comparison. Refused on a native mirror (`local`, or `sftp` without crypt). |
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

- **`local`, `sftp`, `s3`, `b2`, `gcs`** — write a `.squirrel-volume` marker
  under the destination's volume directory, through the same path the transfer
  takes: squirrel's own transport on a destination it writes itself (`local`,
  or `sftp` without crypt, in any layout), rclone and its overlay everywhere
  else. On a `local` destination whose root does not exist yet,
  `--init` also creates the root. Every later sync **requires** that marker and refuses if it is
  missing (a missing marker after the fact almost always means the root is wrong
  — an unmounted disk, a typo, or an unreachable remote). A marker that names a
  *different* volume is always refused, with or without `--init`. A native
  mirror also refuses a volume's first push, `--init` or not, onto a volume
  directory that already holds files squirrel has no record of writing (see
  [mirror](/squirrel/layouts/mirror/#how-a-mirror-is-written)). This holds
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

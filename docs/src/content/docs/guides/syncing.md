---
title: Syncing & first use
description: Push configured volumes to their destinations with BLAKE3 verification, and bootstrap first-use destinations safely with --init.
---

`squirrel sync` pushes configured volumes to their rclone (or kopia)
destinations.

```sh
squirrel sync pictures              # all destinations declared on pictures
squirrel sync pictures --to nas     # just one
squirrel sync                       # every (volume, destination) pair in config
```

- No argument → sync every `(volume, destination)` pair in config.
- One argument → every destination declared on that volume.
- `--to <dest>` → narrow to a single pair.

## End-to-end verification

Sync verifies each uploaded file's BLAKE3 against the destination using rclone's
`--checksum --hash blake3`. A mismatch aborts that file **before** the runs row
is marked success.

Use `--shallow` to fall back to rclone's default size+mtime comparison if you
want speed over integrity for a big initial push. Encrypted
([`crypt`](/squirrel/layouts/encrypted/)) destinations always use the size+mtime
comparison, and content-addressed/packed destinations use presence+size plus the
[scan-back fingerprint](/squirrel/guides/verification/).

Sync runs do **not** pass `--delete-*` to rclone. Files removed locally remain at
the destination.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--to <dest>` | all | Limit to this destination name. |
| `--shallow` | off | Skip BLAKE3 verification; trust rclone's size+mtime comparison. |
| `--dry-run` | off | Preview rclone actions without transferring; no runs row is written. |
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

- **`local`** — writes a `.squirrel-volume` marker under the destination's volume
  directory. Every later sync **requires** that marker and refuses if it is
  missing (a missing marker after the fact almost always means the root is wrong
  — an unmounted disk or a typo). A marker that names a *different* volume is
  always refused, with or without `--init`.
- **`kopia`** — permits `kopia repository create` when connecting finds no
  repository.
- **Remote rclone** (`sftp`, `s3`, `b2`, `gcs`) — do **not** yet enforce a
  marker, so they don't currently require `--init`; marker support for them is a
  tracked follow-up.

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

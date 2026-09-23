---
title: Mirror (default)
description: The default destination layout mirrors the volume's tree at the destination, with overwrites preserved under .squirrel-history.
---

Each mirrored destination (`layout = "mirror"`, the default) is a tree shaped
like the local volumes. This is the layout you get when you declare a
destination without a `layout` key.

```
<dest.root>/
  pictures/
    2024/cat.jpg
    .squirrel-history/run-7/2024/cat.jpg     # prior content of cat.jpg
    .squirrel-index/run-12                   # receipt: what run 12 changed (native mirrors)
    .squirrel-index/index-20260604T120000.000Z-run-12.db   # global index snapshot (ride-along)
    .squirrel-staging/                       # in-flight writes (native mirrors)
  docs/
    invoice.pdf
    .squirrel-history/run-9/invoice.pdf
```

## How a mirror is written

On a `local` destination, and on an `sftp` destination without
[crypt](/squirrel/layouts/encrypted/), squirrel writes the mirror itself: a
*native* mirror. A push plans from
the index: it sends the changes recorded since the destination's last confirmed
run, not what a fresh walk of the disk finds. For each changed path it:

1. streams the file into `.squirrel-staging/run-<id>/`, hashing the bytes as they
   go, and refuses the path if they no longer match the index;
2. moves whatever the path holds into `.squirrel-history/run-<id>/`;
3. renames the staged copy onto the path. The rename never replaces a file.

squirrel records each move before making it, so the next push finishes or
withdraws whatever a crashed or unplugged push left half done. Every successful
push leaves a **receipt** at `.squirrel-index/run-<id>`: the run's changes in the
[manifest segment format](/squirrel/reference/formats/), so the mirror can be
checked without the index. A push that finds no receipt for the last success it
recorded refuses, unless the root is empty and squirrel holds no records for it.
So a native mirror starts on a fresh or emptied root: squirrel does not adopt a
tree some other tool wrote, rclone included.

On sftp, squirrel checks the server's host key against `known_hosts` and refuses
a server it does not know (see [sftp keys](/squirrel/reference/configuration/#sftp)).

Encrypted mirrors, and mirrors on `s3`, `b2` and `gcs`, are written by rclone,
which compares both trees and moves overwritten files into its `--backup-dir`.

## Append-only history

`.squirrel-history/run-<run-id>/` holds the prior bytes of every file a sync run
replaced. They are moved there first, never deleted.

- On a native mirror, anything found at a path squirrel is about to write moves
  there, including bytes squirrel did not write. The run names each one in a
  warning and in its audit trail.
- It is **filtered out** of all subsequent comparisons, so it does not grow
  listing time or get uploaded back.
- A directory literally called `.squirrel-history` in your source volume is also
  filtered (with a warning), to keep the reserved name out of the destination
  tree by accident.

Files removed locally remain at the destination.

## Verification

A native mirror push hashes every file with BLAKE3 as it streams out and
confirms each written path's size and mtime. Nothing reads the landed bytes back
yet, so the run advances the destination's durability evidence under the
`presence+size` method. `--shallow` is refused on a native mirror: there is no
comparison to switch off.

An rclone mirror compares every file with its copy by checksum (rclone's
`--checksum`), under the first hash both ends support — MD5 on S3, independent
of the BLAKE3 in the index. A copy that fails the check after transfer is an
error, so the run is not marked success, and the run advances the evidence under
the `checksum` method.

[`squirrel status`](/squirrel/reference/cli/) shows either method, but the
offload gate accepts neither: a mirror cannot be named in
[`offload_requires`](/squirrel/guides/offloading/). See
[Syncing & first use](/squirrel/guides/syncing/).

## Index snapshots

`.squirrel-index/` holds the [index snapshots](/squirrel/configuration/index-snapshots/)
ridden along after each successful sync, and a native mirror's receipts. Like
`.squirrel-history`, it is filtered out of all sync and restore transfers and
from peer-sync.

## When to use another layout

Mirror keeps the destination browsable and identical to your source tree. Reach
for a different layout when you need:

- **Client-side encryption** → [Encrypted (crypt)](/squirrel/layouts/encrypted/)
- **Append-only cold archive** → [Content-addressed](/squirrel/layouts/content-addressed/)
- **Many small files bundled** → [Packed](/squirrel/layouts/packed/)
- **A second independent format** → [Kopia](/squirrel/layouts/kopia/)

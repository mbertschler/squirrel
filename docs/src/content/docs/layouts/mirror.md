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
    .squirrel-index/index-20260604T120000.000Z-run-12.db   # global index snapshot (ride-along)
  docs/
    invoice.pdf
    .squirrel-history/run-9/invoice.pdf
```

## Append-only history

`.squirrel-history/run-<run-id>/` is rclone's `--backup-dir` target for that
sync run. When a file at the destination is about to be overwritten, its prior
bytes are moved here first — never deleted.

- It is **filtered out** of all subsequent comparisons, so it does not grow
  rclone's listing time or get uploaded back.
- A directory literally called `.squirrel-history` in your source volume is also
  filtered (with a warning), to keep the reserved name out of the destination
  tree by accident.

Sync runs do **not** pass `--delete-*` to rclone. Files removed locally remain
at the destination.

## Verification

Sync compares every file with its copy by checksum (rclone's `--checksum`),
under the first hash both ends support — MD5 on local disks and S3, independent
of the BLAKE3 in the index. A copy that fails the check after transfer is an
error, so the run is not marked success. The run advances the destination's
durability evidence under the `checksum` method, which
[`squirrel status`](/squirrel/reference/cli/) shows but the offload gate does not
accept: a mirror cannot be named in
[`offload_requires`](/squirrel/guides/offloading/). See
[Syncing & first use](/squirrel/guides/syncing/).

## Index snapshots

`.squirrel-index/` holds the [index snapshots](/squirrel/configuration/index-snapshots/)
ridden along after each successful sync. Like `.squirrel-history`, it is filtered
out of all sync and restore transfers and from peer-sync.

## When to use another layout

Mirror keeps the destination browsable and identical to your source tree. Reach
for a different layout when you need:

- **Client-side encryption** → [Encrypted (crypt)](/squirrel/layouts/encrypted/)
- **Append-only cold archive** → [Content-addressed](/squirrel/layouts/content-addressed/)
- **Many small files bundled** → [Packed](/squirrel/layouts/packed/)
- **A second independent format** → [Kopia](/squirrel/layouts/kopia/)

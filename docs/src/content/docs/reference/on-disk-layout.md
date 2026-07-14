---
title: On-disk layouts
description: What each destination layout looks like at the remote — mirror, content-addressed, and packed — plus the reserved .squirrel-* directories.
---

This page shows the on-disk shape of each destination layout at the remote. For
the behavior behind each, see the [destination layout](/squirrel/layouts/mirror/)
guides.

## Mirror (default)

A tree shaped like the local volumes:

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

## Content-addressed

A shared `objects/` directory at the root and `index/` under each per-volume
directory (plus the same `.squirrel-index/` ride-along):

```
<dest.root>/
  objects/
    26e7…e5ad                                # raw bytes of one BLAKE3 content, uploaded once
    9f3a…1c02
  pictures/
    index/run-12                             # immutable manifest segment for run 12
    index/run-13
    .squirrel-index/index-…-run-13.db
```

## Packed

Adds a shared `packs/` directory (tar.zst bundles of small files plus one
placement map per run) alongside the same `objects/` and `index/`:

```
<dest.root>/
  objects/
    26e7…e5ad                                # large files (>= pack_threshold)
  packs/
    9f3a…1c02                                # one immutable tar.zst pack of small files
    map-13                                   # placement map for run 13
  pictures/
    index/run-13
    .squirrel-index/index-…-run-13.db
```

## Reserved directories

Three directory names are reserved and **filtered out** of all sync and restore
transfers (and from peer-sync), so squirrel's own bookkeeping is never mistaken
for user content:

| Directory | Purpose |
|---|---|
| `.squirrel-history/run-<id>/` | rclone's `--backup-dir` target — prior bytes of overwritten files ([mirror](/squirrel/layouts/mirror/)). |
| `.squirrel-index/` | Ride-along [index snapshots](/squirrel/configuration/index-snapshots/). |
| `.squirrel-restore-history/run-<id>/` | Files displaced by an [`--in-place` restore](/squirrel/guides/restore/). |

A directory literally called `.squirrel-history` in your **source** volume is
also filtered (with a warning) to keep the reserved name out of the destination
tree by accident.

## Related formats

The JSONL formats inside `index/run-<id>` and `packs/map-<run>` are documented in
[Manifest & pack formats](/squirrel/reference/formats/).

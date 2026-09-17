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

## Encrypted destinations key these names

The trees above show an unencrypted destination. Add a
[`crypt`](/squirrel/layouts/encrypted/) block to a content-addressed or packed
destination and squirrel names each artifact by a keyed BLAKE3 hash derived from
the crypt passwords, so no content hash or volume name appears at the remote:

```
<dest.root>/
  .squirrel-naming                 # records the naming scheme (carries no key material)
  objects/6f4f9d…cae3              # keyed, not the content hash
  packs/8b0b26…e1e9                # keyed, not the pack key
  packs/map-13                     # run id in clear
  a1c9f2…7b04/                     # keyed volume directory
    index/run-13                   # run id in clear
    .squirrel-index/index-…-run-13.db
```

A mirror is unchanged: its names are the volume's own paths. Run identifiers stay
in clear so segments and placement maps still sort into replay order without the
key — see [deriving the stored
names](/squirrel/reference/formats/#deriving-the-stored-names-on-an-encrypted-destination).

## Reserved directories

Three directory names are reserved and **filtered out** of all sync and restore
transfers (and from peer-sync), so squirrel's own bookkeeping is never mistaken
for user content:

| Directory | Purpose |
|---|---|
| `.squirrel-history/run-<id>/` | rclone's `--backup-dir` target — prior bytes of overwritten files ([mirror](/squirrel/layouts/mirror/)). |
| `.squirrel-index/` | Ride-along [index snapshots](/squirrel/configuration/index-snapshots/). |
| `.squirrel-restore-history/run-<id>/` | Files displaced by an [`--in-place` restore](/squirrel/guides/restore/). |

One reserved **file** sits at the destination root rather than in a volume tree:
`.squirrel-naming`, which records the artifact-naming scheme of an
[encrypted](/squirrel/layouts/encrypted/) content-addressed or packed root. It is
the gate that stops two naming generations from being mixed into one root, and it
never carries key material.

A directory literally called `.squirrel-history` in your **source** volume is
also filtered (with a warning) to keep the reserved name out of the destination
tree by accident.

## Related formats

The JSONL formats inside `index/run-<id>` and `packs/map-<run>` are documented in
[Manifest & pack formats](/squirrel/reference/formats/).

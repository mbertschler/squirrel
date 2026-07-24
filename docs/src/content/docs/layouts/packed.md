---
title: Packed
description: The content-addressed layout with a size split — large files stay one-object-per-hash, small files are bundled into big immutable tar.zst packs.
---

A [content-addressed](/squirrel/layouts/content-addressed/) destination stores
one object per file. That is ideal until a volume holds a great many *small*
files: one remote object apiece is slow to write, slow to list, and on some
providers billed per object.

The **packed** layout is the content-addressed layout with a size split — large
files stay one-object-per-hash, small files are bundled into a few big immutable
archives.

```toml
[destinations.archive]
type   = "sftp"
host   = "archive.example"
user   = "u"
root   = "/data"
layout = "packed"
pack_threshold = "1MiB"   # files smaller than this are packed; at/above land as objects
pack_size      = "512MiB" # target size of one pack before it is closed
zstd_level     = 3        # 1 fastest … 4 best
```

## Routing by size

Routing is by **size**: content at or above `pack_threshold` lands as
`objects/<hash>` exactly as in the content-addressed layout (same per-object
`remote_objects` record and scan-back fingerprint); content below it is bundled.

The destination root therefore holds three streams:

- **`objects/<hash>`** — large files, exactly as content-addressed (shared across
  volumes, uploaded once per hash).
- **`packs/<pack-key>`** — one immutable **tar.zst pack** per bundle: small
  files, hash-sorted into a normalized PAX tar and solid-compressed with zstd
  (encrypted client-side when the destination has a `crypt` block). The file name
  is the BLAKE3 of the compressed bytes, so an identical bundle names the same
  file. A pack is written once and never rewritten or deleted; content already
  packed is never re-packed.
- **`packs/map-<run>`** — one **placement map** per sync run: JSONL locating each
  newly packed content inside its pack, alongside the same
  `<volume>/index/run-<id>` manifest segment the content-addressed layout writes.
  See the [placement map format](/squirrel/reference/formats/#placement-map-format).

## Three-artifact durability

Durability is **transactional per run and three-artifact**: a run advances the
destination's durability evidence only once *all three* of its artifacts — every
pack, the run's `packs/map-<run>`, and its `index/run-<run>` segment — are
confirmed on the remote **and** every pack has a verified scan-back fingerprint.

A pack's fingerprint is read straight back from the provider after upload (one
check per ~512 MB pack vouches for every file it holds — the packed analogue of
the per-object scan-back). If that read is unavailable, the pack is left
**pending** with a warning and the vector is *not* advanced, so unverified packed
content is never counted durable.
[`squirrel verify`](/squirrel/guides/verification/) fills any pending pack
fingerprint and re-confirms the rest, per pack.

## Shared properties

Properties match the content-addressed layout:

- Verification is presence+size (recorded shallow).
- The layout is chosen at first use and refuses to run against a
  differently-shaped history.
- `--dry-run` is not supported yet on the push.
- [`squirrel restore`](/squirrel/guides/restore/) **restores the layout**: it
  resolves each present path to its content in the local index, fetches each
  pack once to serve all its members, and re-hashes every extracted member
  before writing. Cold tiers (Glacier / Deep Archive) need a manual thaw first;
  see the [restore guide](/squirrel/guides/restore/#content-addressed-and-packed-destinations).
  When the local index is lost, the format still
  [recovers without squirrel](/squirrel/reference/formats/#disaster-recovery-without-squirrel).

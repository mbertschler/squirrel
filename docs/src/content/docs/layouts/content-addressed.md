---
title: Content-addressed
description: An append-only, content-addressed layout for cold archive storage where objects are uploaded once by hash and never rewritten or moved.
---

By default a destination [mirrors](/squirrel/layouts/mirror/) the volume's tree.
Any rclone-remote destination — with or without a [`crypt`](/squirrel/layouts/encrypted/)
block — can instead opt into an **append-only, content-addressed** layout, built
for cold archive storage where objects should never be rewritten or moved.

```toml
[destinations.archive]
type   = "sftp"
host   = "archive.example"
user   = "u"
root   = "/data"
layout = "content-addressed"
```

## The two streams

Instead of a browsable tree, the destination holds:

- **`objects/<hash>`** (at the destination root, shared by all volumes) — one
  object per BLAKE3 content hash (lowercase hex), the raw file bytes (encrypted
  client-side when the destination has a `crypt` block). Each hash is uploaded
  **exactly once** per destination and never moved, overwritten, or deleted. A
  local rename or reorg changes only the path mapping — no re-upload, no
  server-side copy — and content duplicated across volumes is stored once.
- **`<volume>/index/run-<id>`** — one immutable **manifest segment** per sync
  run, per volume: the path-level delta of that run. Replaying a volume's
  segments in run order yields its full current path→content mapping, and any
  past state. See the [manifest segment format](/squirrel/reference/formats/#manifest-segment-format).

## Transactional durability

Durability is **transactional per run**: the run only counts as successful — and
only then feeds the durability evidence squirrel records per destination — once
*both* all its content objects *and* its manifest segment are confirmed on the
remote (each transfer's success plus a follow-up presence/size listing).

A failed run may leave objects without a segment; they are harmless (nothing maps
them) and the next run skips re-uploading anything already recorded, pushing only
what's missing.

## Properties that differ from mirrored destinations

- **Verification is presence+size**, recorded as such: per-object transfers
  can't carry the end-to-end BLAKE3 check (and `crypt` remotes expose no hashes
  at all), so the runs row is recorded shallow and the push never claims content
  verification. On top of that, each upload's provider-side ciphertext
  fingerprint is recorded and re-checked by
  [`squirrel verify`](/squirrel/guides/verification/).
- **Pick the layout when the destination is first used.** Switching an existing
  mirrored destination to `content-addressed` (or back) is not supported — point
  the new layout at a fresh destination or root. The push detects a mirrored
  history and refuses.
- **[`squirrel restore`](/squirrel/guides/restore/) restores the layout**: it
  resolves each present path to its content hash from the local index, fetches
  the per-hash object through the same rclone (`crypt`) read path the push uses,
  and re-hashes it before writing. When the local index itself is lost, the
  format is deliberately simple enough to
  [recover without squirrel](/squirrel/reference/formats/#disaster-recovery-without-squirrel).
- **`--dry-run` is not supported yet on the push** (it previews restore).

## Offsite verification

Cold archive storage is exactly the copy you can't cheaply re-download and
re-hash. Content-addressed destinations therefore get a metadata-only integrity
check — the **scan-back fingerprint** — that never transfers an object body. See
[Offsite verification](/squirrel/guides/verification/) for how it works per
backend and how to run `squirrel verify`.

## Related knobs

```toml
[destinations.archive]
# ...
hash_algo        = "sha256"  # sftp only: which server-side hash the fingerprint uses
checkers         = 4         # cap rclone's concurrent checkers (providers that limit connections)
force_path_style = true      # s3 only: path-style bucket addressing for the ETag reader
```

See the [configuration reference](/squirrel/reference/configuration/#content-addressed--packed-knobs)
for what each does.

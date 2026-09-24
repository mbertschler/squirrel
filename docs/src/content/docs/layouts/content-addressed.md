---
title: Content-addressed
description: An append-only, content-addressed layout for cold archive storage where objects are uploaded once by hash and never rewritten or moved.
---

By default a destination [mirrors](/squirrel/layouts/mirror/) the volume's tree.
Any destination but kopia — a `local` disk, or an `sftp` server, `s3`, `b2` or
`gcs` with or without a [`crypt`](/squirrel/layouts/encrypted/) block — can
instead opt into an **append-only, content-addressed** layout, built for cold
archive storage where objects should never be rewritten or moved.

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

:::note[On an encrypted destination these names are keyed]
Add a [`crypt`](/squirrel/layouts/encrypted/) block and the object name is no
longer the content hash, nor the directory the volume name: both become a keyed
BLAKE3 hash derived from the crypt passwords, so the remote discloses neither.
Deduplication is unaffected — identical content still derives one name and
uploads once.
:::

## How it is written

On a `local` disk, and on an `sftp` server without `crypt`, squirrel writes the
destination itself, without rclone. Objects land in batches of up to 64,
several at once ([`concurrency`](/squirrel/reference/configuration/#common-keys));
a manifest segment lands alone. Each:

1. streams into `<volume>/.squirrel-staging/run-<id>/` while BLAKE3 hashes the
   bytes sent, and is refused if they no longer match the index; the batch is
   then flushed to stable storage;
2. is confirmed before it gets its name: read back through BLAKE3 on a local
   disk, hashed by the server's `hash_algo` command (`sha256sum` by default) on
   sftp. A match becomes the object's fingerprint;
3. is renamed onto its name. A file already at that name is never replaced:
   squirrel checks that it holds the right bytes, or fails the object;
4. is recorded, with its fingerprint, once a second flush has made the batch's
   renames durable.

A server that runs no programs takes every object all the same, but
fingerprints nothing: those objects stay pending, with a warning, and the
destination cannot gate offload for them. Both kinds of destination carry the
`.squirrel-volume` marker, written under `--init`, which also creates a missing
local root.

Encrypted destinations, and those on `s3`, `b2` and `gcs`, are written by
rclone.

## Transactional durability

Durability is **transactional per run**: the run only counts as successful — and
only then feeds the durability evidence squirrel records per destination — once
*both* all its content objects *and* its manifest segment are confirmed on the
remote: squirrel confirms each artifact it writes itself as it lands, and
rclone's transfers are followed by a presence/size listing.

A failed run may leave objects without a segment; they are harmless (nothing maps
them) and the next run skips re-uploading anything already recorded, pushing only
what's missing.

## Properties that differ from mirrored destinations

- **Verification is presence+size**, recorded as such: each object is hashed
  with BLAKE3 on its way out and confirmed present at the expected size after
  it, and the runs row is recorded shallow. On top of that, each object's
  fingerprint is recorded — confirmed at landing where squirrel writes the
  destination itself, read back from the provider after the upload elsewhere
  (the ciphertext's, behind `crypt`) — and re-checked by
  [`squirrel verify`](/squirrel/guides/verification/). A push whose volume is
  fingerprinted throughout advances as `fingerprint-verified`.
- **Pick the layout when the destination is first used.** Switching an existing
  mirrored destination to `content-addressed` (or back) is not supported — point
  the new layout at a fresh destination or root. The push detects a mirrored
  history and refuses.
- **[`squirrel restore`](/squirrel/guides/restore/) restores the layout**: it
  resolves each present path to its content hash from the local index, fetches
  the per-hash object through the same read path the push uses — squirrel's
  own, or rclone and its `crypt` overlay — and re-hashes it before writing. When the local index itself is lost, the
  format is deliberately simple enough to
  [recover without squirrel](/squirrel/reference/formats/#disaster-recovery-without-squirrel).
- **`--dry-run` previews the push**: the objects it would upload and their
  bytes, written nowhere.

## Offsite verification

Cold archive storage is exactly the copy you can't cheaply re-download and
re-hash. Content-addressed destinations on a bucket therefore get a
metadata-only integrity check — the **scan-back fingerprint** — that never
transfers an object body; one squirrel writes itself is re-hashed in full. See
[Offsite verification](/squirrel/guides/verification/) for how it works per
backend and how to run `squirrel verify`.

## Related knobs

```toml
[destinations.archive]
# ...
hash_algo        = "sha256"  # sftp only: which server-side hash the fingerprint uses
checkers         = 4         # rclone destinations only: cap rclone's concurrent checkers
force_path_style = true      # s3 only: path-style bucket addressing for the ETag reader
```

See the [configuration reference](/squirrel/reference/configuration/#content-addressed--packed-knobs)
for what each does.

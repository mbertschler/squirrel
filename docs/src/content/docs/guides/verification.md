---
title: Offsite verification
description: Content-addressed and packed destinations get a metadata-only integrity check — the scan-back fingerprint — that re-checks stored objects without downloading a byte.
---

Cold archive storage is exactly the copy you can't cheaply re-download and
re-hash. [Content-addressed](/squirrel/layouts/content-addressed/) and
[packed](/squirrel/layouts/packed/) destinations therefore get a metadata-only
integrity check: the **scan-back fingerprint**.

## How the scan-back fingerprint works

After each object upload is confirmed, squirrel reads the *provider's own
checksum* of the stored bytes (the ciphertext, for `crypt` destinations) back
from the remote and records it in the index next to the upload.

Verification then re-fetches the same metadata later and compares **provider
value then vs provider value now** — squirrel never recomputes a provider
checksum, so provider-specific composite forms are handled as opaque strings, and
**no object body is ever transferred**.

The read is done via a direct S3 `ListObjectsV2` for `s3`, or `rclone lsjson
--hash` for every other backend.

## What gets recorded, by backend

- **`s3`** — the object **ETag**, recorded as `etag-md5` for a single-part
  upload's whole-object MD5, or `etag-md5-composite` for a multipart object's
  `<hex>-<parts>` value, stored verbatim either way. The ETag is read straight
  from the S3 API with a paginated `ListObjectsV2` over the `objects/` prefix,
  *not* through rclone: rclone funnels every hash read through `Object.Hash(MD5)`,
  which returns an empty string for a composite ETag, so a multipart (or
  client-encrypted, always-streamed) object would otherwise never expose a
  fingerprint. Listing is archive-tier-safe (no per-object `HEAD`, no restore),
  and the composite ETag is fixed at upload time and unaffected by later
  storage-class transitions or server-side encryption. For S3-compatible
  providers whose endpoint the client addresses wrongly, set `force_path_style =
  true`.
- **`sftp`** — the checksum computed server-side by the remote's hash command.
  Content-addressed sftp destinations default to **SHA-256** (`hash_algo =
  "sha256"`); set `hash_algo` if your server only offers another type.
- **other backends** — whatever hash `rclone lsjson --hash` exposes, recorded
  under its rclone hash name (e.g. `sha1` on b2). A backend exposing no checksum
  leaves the fingerprint pending, with a warning in the sync output.

## Running verify

Re-verify a destination (or all content-addressed and packed destinations) at any
time:

```sh
squirrel verify archive
squirrel verify
```

With no argument, `verify` covers every content-addressed or packed destination
in config. An explicit destination must have one of those layouts, else it
errors.

The pass lists the destination's `objects/` directory once (batched,
metadata-only), then per recorded object:

- a **match** stamps the object verified in the index;
- an object **without a fingerprint yet** (uploaded before this feature, or whose
  capture failed) gets one recorded and is counted separately;
- a **mismatch or missing object** prints one loud line per object and exits
  non-zero — that is potential offsite corruption or tampering, and squirrel
  deliberately leaves both the destination and the recorded fingerprint untouched
  for inspection.

Each pass is recorded as an `audit` run, with the destination and counters in the
run's audit trail.

:::note[Why the ciphertext fingerprint is stable]
Because crypt encrypts with a random per-file nonce, the fingerprint is a
property of the *uploaded ciphertext*, not of the content — which is exactly
right here: the layout is append-only and each object is uploaded once, so the
fingerprint is stable for the life of the object.
:::

## Related knobs

```toml
[destinations.archive]
# ...
hash_algo        = "sha256"  # sftp only: which server-side hash the fingerprint uses
checkers         = 4         # cap rclone's concurrent checkers
force_path_style = true      # s3 only: path-style bucket addressing for the ETag reader
```

`checkers` flows into `--checkers` on the rclone invocations squirrel runs
against that destination — useful when a provider caps simultaneous connections
(server-side hashing typically uses one connection per concurrent check).

`force_path_style` governs only squirrel's own S3 client (the one that reads
scan-back ETags), not the rclone transport. Leave it off for AWS and most
providers; set it `true` only for an S3-compatible provider (a minio host, an IP
endpoint) whose addressing the auto-detection gets wrong.

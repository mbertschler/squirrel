---
title: Offsite verification
description: squirrel verify re-checks what squirrel stored — content-addressed and packed objects by their fingerprint, a native mirror's copies by size and mtime, and on a local disk by BLAKE3.
---

Cold archive storage is exactly the copy you can't cheaply re-download and
re-hash. [Content-addressed](/squirrel/layouts/content-addressed/) and
[packed](/squirrel/layouts/packed/) destinations on a bucket therefore get a
metadata-only integrity check: the **scan-back fingerprint**. A destination
squirrel writes itself — `local`, or `sftp` without crypt — is re-hashed in
full instead (below).

## How the scan-back fingerprint works

After each object upload is confirmed, squirrel reads the *provider's own
checksum* of the stored bytes (the ciphertext, for `crypt` destinations) back
from the remote and records it in the index next to the upload.

Verification then re-fetches the same metadata later and compares **provider
value then vs provider value now** — squirrel never recomputes a provider
checksum, so provider-specific composite forms are handled as opaque strings, and
on a bucket **no object body is ever transferred**.

The read is done via a direct S3 `ListObjectsV2` for `s3`, `rclone lsjson
--hash` for every other backend rclone writes, and squirrel's own transport for
a destination it writes itself (below).

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
- **`local`** — the BLAKE3 of the stored bytes, which squirrel reads back itself,
  past the page cache where the system allows: as each object or pack lands,
  before it gets its name, and on every verify pass. Here the fingerprint is a
  content check too: an object's BLAKE3 must equal its name, a pack's its key.
- **`sftp`** — the checksum computed server-side by the remote's hash command.
  Content-addressed sftp destinations, and packed ones without crypt, default
  to **SHA-256** (`hash_algo = "sha256"`); set `hash_algo` if your server only
  offers another type. Without `crypt` squirrel runs the command itself — `md5sum`, `sha1sum`,
  `sha256sum` or `b3sum`, probed once per session — on the staged copy before it
  gets its name, and checks the answer against the bytes it sent. A server that
  runs no programs leaves every fingerprint pending, with a warning. The command
  line carries only the destination root and the artifact's hex name, and only
  when both hold nothing but letters, digits, `.`, `_`, `-` and `/`.
- **other backends** — whatever hash `rclone lsjson --hash` exposes, recorded
  under its rclone hash name (e.g. `sha1` on b2). A backend exposing no checksum
  leaves the fingerprint pending, with a warning in the sync output.

## Running verify

Re-verify a destination (or every destination verify covers) at any time:

```sh
squirrel verify archive
squirrel verify
```

With no argument, `verify` covers every content-addressed or packed destination
and every [native mirror](#native-mirrors) in config. An explicit destination
must be one of those, else it errors: a mirror rclone writes records nothing to
re-check.

The pass lists the destination's `objects/` directory once (batched, and
metadata-only on a bucket; on a `local` destination squirrel re-reads every
recorded artifact, on plain sftp the server re-hashes each one), then per
recorded object:

- a **match** stamps the object verified in the index;
- an object **without a fingerprint yet** (uploaded before this feature, or whose
  capture failed) gets one recorded and is counted separately;
- an object an sftp server **cannot hash this pass** — its hash command is
  gone, or the object was fingerprinted under another `hash_algo` — is counted
  as `unchecked`: neither re-confirmed nor a finding;
- a **mismatch or missing object** prints one loud line per object and exits
  non-zero — that is potential offsite corruption or tampering, and squirrel
  deliberately leaves both the destination and the recorded fingerprint untouched
  for inspection.

Each pass is recorded as an `audit` run, with the destination and counters in the
run's audit trail.

## A clean pass unlocks offload

Verification is not only a bitrot check — it is what makes a cold-archive
destination usable by [`offload`](/squirrel/guides/offloading/). Content-addressed
and packed uploads are recorded as `presence+size` at write time, and the offload
gate refuses that method. When a pass leaves every object and pack of a
(volume, destination) pair fingerprint-verified, squirrel **upgrades the
durability vector to a content-verified method** and re-attempts any advance that
was held back, relaying the upgraded method to peers.

A push whose objects all got their fingerprint as they landed advances the
vector content-verified by itself: squirrel reads each one back on a local disk,
hashes it with the server's command on sftp, and scans it back right after the
upload elsewhere. Where a fingerprint stays pending — a server that runs no
programs, a backend that exposes none at upload — the sequence is sync → verify
→ offloadable, not sync → offloadable. Giving verify [its own agent cadence](/squirrel/guides/agent/)
(`verify_every`) keeps that unattended.

## A mismatch latches an alarm

A mismatch does not just print and exit non-zero — it raises a **standing alarm**
on the destination that outlives the run. The alarm shows on the TUI dashboard
and in [`squirrel status`](/squirrel/reference/cli/#squirrel-status) until it is
dealt with, so a corruption finding cannot scroll away in the runs list while
later syncs push on as if nothing happened.

Clear it either way:

```sh
squirrel verify archive              # a clean pass clears the alarm itself
squirrel verify ack archive          # or acknowledge it explicitly
```

The raise and the clear are both recorded, with the operator's name on an
explicit ack — "someone decided this was fine" stays as auditable as the failure
that raised it.

:::note[Why the ciphertext fingerprint is stable]
Because crypt encrypts with a random per-file nonce, the fingerprint is a
property of the *uploaded ciphertext*, not of the content — which is exactly
right here: the layout is append-only and each object is uploaded once, so the
fingerprint is stable for the life of the object.

On an [encrypted](/squirrel/layouts/encrypted/) destination the listing is keyed
by the artifact's keyed name rather than its content hash, which squirrel derives
locally from the crypt passwords. Verification is otherwise identical, so keying
the names costs no depth of checking and no offload eligibility.
:::

## Native mirrors

A [mirror](/squirrel/layouts/mirror/) squirrel writes itself — on a `local`
disk, or on `sftp` without crypt — records every copy it stored, so verify
re-checks those instead of objects. It reaches the mirror through squirrel's own
transport, without rclone. Each pass:

- checks every copy squirrel stored, the live ones and those in
  `.squirrel-history`, by size and mtime — one `lstat` each;
- on a `local` disk, also re-reads the least recently checked copies through
  BLAKE3, until it has read a tenth of the stored bytes, so every byte is read
  again within about ten passes.

A pass first reads every volume's `.squirrel-volume` marker and stops, checking
nothing, when one is missing: an unmounted disk must not read as every copy
gone. A copy found gone or changed prints one loud line, latches the
[alarm](#a-mismatch-latches-an-alarm), and stops counting as evidence for its
file: the volume's evidence drops to `presence+size`, so offload checks each
file's own copy until the next push writes the lost one again. The next push writes it again as long as the index still holds that file
there, with a warning; the copy that was found keeps its place in history if the
push has to move it aside.

A local mirror earns its fingerprints at push time: every copy is flushed to the
disk, then read back through BLAKE3 before it is committed. An sftp mirror is checked by size and
mtime alone, and never gates offload.

## Related knobs

```toml
[destinations.archive]
# ...
hash_algo        = "sha256"  # sftp only: which server-side hash the fingerprint uses
checkers         = 4         # rclone destinations only: cap rclone's concurrent checkers
force_path_style = true      # s3 only: path-style bucket addressing for the ETag reader
```

`checkers` flows into `--checkers` on the rclone invocations squirrel runs
against that destination — useful when a provider caps simultaneous connections
(server-side hashing typically uses one connection per concurrent check).

`force_path_style` governs only squirrel's own S3 client (the one that reads
scan-back ETags), not the rclone transport. Leave it off for AWS and most
providers; set it `true` only for an S3-compatible provider (a minio host, an IP
endpoint) whose addressing the auto-detection gets wrong.

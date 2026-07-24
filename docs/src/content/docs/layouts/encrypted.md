---
title: Encrypted (crypt)
description: Any non-local destination can encrypt file contents client-side before upload via rclone's crypt overlay. Names stay in clear; verification falls back to size+mtime.
---

Any non-`local` destination can add a `crypt` block to encrypt file contents
client-side before upload, via rclone's [crypt](https://rclone.org/crypt/)
overlay.

```toml
[destinations.offsite.crypt]
password  = { env = "OFFSITE_CRYPT_PASSWORD" }
password2 = { env = "OFFSITE_CRYPT_SALT" }    # salt — optional but recommended
```

`password` and `password2` are **plaintext** — a literal or `{ env = "VAR" }`.
Squirrel obscures them into rclone's on-disk representation when it renders
`rclone.conf`, so you no longer run `rclone obscure` yourself.

:::note[Migrating an already-obscured config]
Configs written before this behaviour hold pre-obscured values. Add
`obscured = true` to the `crypt` block to keep them verbatim, or replace them
with the plaintext and drop the marker.
:::

Squirrel renders two sections into its `rclone.conf` — the underlying remote plus
a crypt remote wrapping it — and addresses all sync and restore transfers
through the crypt remote.

:::danger[Keep the passwords safe]
Restoring from an encrypted destination requires `password` (and `password2` if
set). Lose them and the data is unrecoverable.
:::

## Two properties to be aware of

### Contents only — names are in clear

File and directory names are stored in clear at the destination
(`filename_encryption = off`, fixed by design) — the tree stays browsable and
keeps the same layout as an unencrypted destination. **If the names themselves
are sensitive, this overlay does not hide them.**

### Verification falls back to size+mtime

rclone crypt remotes cannot expose content hashes, so the end-to-end BLAKE3 check
(`--checksum --hash blake3`) cannot pass through the overlay. Transfers to and
from an encrypted destination compare by **size+mtime** instead — the same
comparison `--shallow` uses — and say so in the run output; the runs row records
the transfer as shallow.

Content-addressed destinations regain deeper verification through provider-side
ciphertext fingerprints — see [Offsite verification](/squirrel/guides/verification/).

## Combining with other layouts

A `crypt` block composes with the [content-addressed](/squirrel/layouts/content-addressed/)
and [packed](/squirrel/layouts/packed/) layouts. When it does, the scan-back
fingerprint is a property of the uploaded **ciphertext** — which is exactly right
for an append-only layout where each object is uploaded once.

Kopia destinations **reject** a `crypt` block: [kopia](/squirrel/layouts/kopia/)
encrypts its own repository.

---
title: Encrypted (crypt)
description: Any non-local destination can encrypt file contents client-side before upload via rclone's crypt overlay. On the append-only layouts the stored names are keyed too; verification falls back to size+mtime.
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

### What the destination discloses depends on the layout

rclone's own filename encryption stays off (`filename_encryption = off`, fixed
by design). What that means for you depends on which layout the destination
uses, because the two layouts name their files in completely different ways.

On the [content-addressed](/squirrel/layouts/content-addressed/) and
[packed](/squirrel/layouts/packed/) layouts, every name at the destination is
one squirrel chose, and squirrel **keys** them: objects, packs, and the
per-volume directory are named by a keyed BLAKE3 hash derived from your crypt
passwords. The remote therefore discloses neither a path nor a content hash, and
nobody holding a candidate file can hash it and test whether your archive stores
it without first finding your passwords. The naming key is stretched from the
passwords by the same scrypt derivation rclone crypt uses, so guessing a password
through the names costs the same work as guessing it through the encrypted data.
Your paths live inside the manifest segments, which ride the overlay encrypted
like everything else.

On the [mirror](/squirrel/layouts/mirror/) layout the names *are* your own
tree, replicated path for path, and they stay in clear, together with each
file's size and modification time. **If the names themselves are sensitive, use
an append-only layout rather than a mirror** (or a
[kopia](/squirrel/layouts/kopia/) repository, which encrypts its own metadata).

What no layout hides:

- **Exact sizes.** rclone crypt adds a fixed overhead — a 32-byte header plus
  16 bytes per 64 KiB block — so every stored file's plaintext size follows from
  its ciphertext size. Each content object therefore discloses the exact size of
  one distinct content, however many paths share it; on a packed destination,
  files below `pack_threshold` show only as part of their pack's compressed
  size.
- **Source modification times.** rclone carries a source file's modification
  time onto the stored copy — the file's own mtime on sftp, object metadata on
  the bucket backends — and the overlay passes it through. Every content object
  (each file on a content-addressed destination, each file at or above
  `pack_threshold` on a packed one) therefore shows the modification time of the
  file it was first uploaded from. Packs, manifest segments, and placement maps are
  built during the run, so they show only when the run wrote them.
- **Counts and timing.** Object, pack, and volume counts; the run identifiers
  and snapshot timestamps in the names below; upload times; and your sync
  cadence.

#### What is keyed, and what stays readable

```
<dest.root>/
  .squirrel-naming                        # records the naming scheme (no key material)
  objects/<name("object", blake3)>        # keyed name of the content hash
  packs/<name("pack", pack)>              # keyed name of the pack key
  packs/map-13                            # run id in clear
  <name("volume", volume)>/index/run-13   # keyed volume directory, run id in clear
  <name("volume", volume)>/.squirrel-index/index-20260604T120000.000Z-run-13.db
                                          # snapshot time and run id in clear
```

Run identifiers stay in clear deliberately: replaying segments in run order is
what lets you [recover from the destination without
squirrel](/squirrel/reference/formats/#disaster-recovery-without-squirrel), and
that ordering has to survive without the key.

The `.squirrel-naming` marker records *which* scheme a root was written under, so
two naming schemes are never mixed into one root. The first `--init` push to an
empty root writes it, and a push that finds it missing from a root still holding
the volume's keyed directory — so written under these very passwords — writes it
back. A push refuses a root that holds anything else without a marker (files an
unencrypted destination or other passwords left there), or a marker recording a
scheme it does not write, and names the remedy: point the destination at a
fresh root, or wipe the remote root and run `squirrel destination reset <name>`.

:::note[The key is derived from your passwords, never stored]
The naming key comes from `password` and `password2` through rclone crypt's
scrypt key derivation and a BLAKE3 key-derivation step, so there is no new secret
to keep and no key file to lose — and the names stay reproducible from the config
alone. The exact derivation, including the `name(…)` shown above, is documented
for recovery tooling in
[Manifest & pack formats](/squirrel/reference/formats/#deriving-the-stored-names-on-an-encrypted-destination).
Losing the passwords costs you the ability to *locate* an artifact as well as to
decrypt it.
:::

### Verification falls back to size+mtime

rclone crypt remotes cannot expose content hashes, so the checksum comparison
(`--checksum`) cannot pass through the overlay. Transfers to and
from an encrypted destination compare by **size+mtime** instead — the same
comparison `--shallow` uses — and say so in the run output; the runs row records
the transfer as shallow.

Content-addressed destinations regain deeper verification through provider-side
ciphertext fingerprints — see [Offsite verification](/squirrel/guides/verification/).

## Combining with other layouts

A `crypt` block composes with the [content-addressed](/squirrel/layouts/content-addressed/)
and [packed](/squirrel/layouts/packed/) layouts, and those are the combinations
that also key their stored names. When it does, the scan-back
fingerprint is a property of the uploaded **ciphertext** — which is exactly right
for an append-only layout where each object is uploaded once. Verification and
fingerprint capture list the underlying remote by the same keyed names, so
neither is weakened by keying.

Kopia destinations **reject** a `crypt` block: [kopia](/squirrel/layouts/kopia/)
encrypts its own repository.

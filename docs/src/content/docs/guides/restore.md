---
title: Restoring
description: Pull a volume back from one of its destinations, with content verification on the way down (BLAKE3 for native mirrors and archive layouts, a checksum comparison for rclone mirrors).
---

`squirrel restore` pulls a volume back from one of its destinations.

```sh
squirrel restore pictures --from nas
squirrel restore pictures --from nas --to /tmp/pictures-restore
```

It takes exactly one positional argument — the **volume name**.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--from <name>` | — | Destination name to pull from, **or** peer node name to filter by content origin (names are unique across both kinds). |
| `--to <path>` | volume's declared path | Local target path. |
| `--shallow` | off | Skip the checksum comparison on the way down from an rclone mirror. Every other destination's bytes are re-hashed regardless. |
| `--dry-run` | off | Preview what the restore would fetch without transferring. |
| `--in-place` | off | Permit restore against a non-empty live `vol.Path`; overwritten files are moved to `.squirrel-restore-history/run-<id>/`. |

## Verification on the way down

A [native mirror](#native-mirrors) restore and a content-addressed or packed
restore hash every byte to BLAKE3 as it arrives and refuse what does not match
(see below). A restore from an rclone mirror compares each file with its copy by
checksum as it arrives, the same comparison [`sync`](/squirrel/guides/syncing/)
uses on the way up; pass `--shallow` to skip it.

Every restored file lands through a temporary file beside its path, which is
flushed and then renamed over it, so a restore that stops halfway never leaves a
truncated file behind.

:::note[Encrypted mirrors are always size+mtime]
An [encrypted (`crypt`)](/squirrel/layouts/encrypted/) mirror cannot expose
content hashes through rclone, so restore from one falls back to a size+mtime
comparison — recorded as shallow — **even without** `--shallow`, exactly as sync
does. Passing `--shallow` changes nothing there. An encrypted content-addressed
or packed destination is different: its restore re-hashes every file to BLAKE3,
with or without crypt.
:::

## Restoring in place

By default restore refuses to write into a non-empty live volume path, to avoid
clobbering current data. `--in-place` permits it — and any file it would
overwrite is first moved to `.squirrel-restore-history/run-<id>/`, mirroring the
append-only [`.squirrel-history`](/squirrel/layouts/mirror/) behavior on the sync
side. Nothing is destroyed.

## Native mirrors

A [native mirror](/squirrel/layouts/mirror/#how-a-mirror-is-written) — `local`,
or `sftp` without crypt — is read through squirrel's own transport; no rclone is
involved.

- **With an index** that holds present files for the volume, each present path
  is fetched by its path and its bytes are checked against the index's BLAKE3
  as they stream; a file whose bytes differ is refused, not written. A path that
  already holds its indexed bytes is left alone and counted as already correct.
- **Without one** — a fresh machine, before or instead of
  [`squirrel recover`](/squirrel/guides/recovery/) — restore walks the mirrored
  tree, leaving out `.squirrel-history/`, `.squirrel-index/`,
  `.squirrel-staging/` and the marker. The mirror's
  [receipts](/squirrel/layouts/mirror/#how-a-mirror-is-written) name the content
  each path last held, so every file a receipt names is checked against it and
  refused if it differs. A file no receipt names (one squirrel did not write) is
  restored unchecked, and the run's warnings count them. The `._` files macOS
  keeps beside each file on an exFAT or FAT disk hold that file's extended
  attributes; restore leaves them out and counts them in one warning.

## Content-addressed and packed destinations

`squirrel restore` handles the
[content-addressed](/squirrel/layouts/content-addressed/) and
[packed](/squirrel/layouts/packed/) layouts too — they have no mirrored tree to
copy, so restore works from the **local index** instead:

1. Each present path is resolved to its content BLAKE3 in the index.
2. The bytes are located per content: a per-hash object under `objects/`, or a
   member of a `tar.zst` pack under `packs/` (`pack_members` carries its
   offset and length).
3. Objects and packs are fetched through the same read path the push uses:
   squirrel's own transport on a `local` disk and on `sftp` without crypt, with
   no rclone involved, and rclone (and its `crypt` overlay) elsewhere. **Packs
   are fetched once** — one download serves every requested member of that
   pack, never one fetch per file.
4. Every fetched object and extracted pack member is **re-hashed to BLAKE3 and
   compared** before it is written, so a misplaced or corrupted byte is refused
   rather than restored. (Because of this, `--shallow` does not weaken an
   archive restore — the content check is intrinsic.)

:::note[Cold storage tiers need a manual thaw first]
Restore reads whole objects and packs, so an object stored on a cold tier that
must be *thawed before it can be read* (AWS S3 Glacier Flexible Retrieval / Deep
Archive) has to be restored-to-standard out of band first — e.g.
`aws s3 restore-object …` or `rclone backend restore …` — and then
`squirrel restore` run once the objects are readable. squirrel does not yet
orchestrate the Glacier `RestoreObject`-and-poll cycle. Warm and standard tiers
(including `GLACIER_IR`) and local destinations need no thaw.
:::

:::note[Restoring when the local index is lost]
The archive restore reads path→hash from the local index. If the index itself is
gone, first recover it — swap in the [ride-along index
snapshot](#restoring-the-index-too) — or recover the data directly from the
[on-disk format](/squirrel/reference/formats/#disaster-recovery-without-squirrel).
A `--from-manifest` mode that rebuilds the mapping from the destination's manifest
segments is a possible future addition.
:::

## Kopia destinations

`squirrel restore` still refuses [kopia](/squirrel/layouts/kopia/) destinations —
restore goes through the kopia CLI (`kopia snapshot restore`) instead.

## Restoring the index too

For a full disaster-recovery scenario, remember that squirrel rides an
[index snapshot](/squirrel/configuration/index-snapshots/) along to its
destinations under `.squirrel-index/`. A restore from a destination yields the
data *and* the index that explains it — use [`squirrel db restore`](/squirrel/reference/cli/#squirrel-db)
to swap that snapshot in as the live index.

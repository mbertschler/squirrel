---
title: Restoring
description: Pull a volume back from one of its rclone destinations, with content verification on the way down (BLAKE3 where the destination exposes hashes).
---

`squirrel restore` pulls a volume back from one of its rclone destinations.

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
| `--shallow` | off | Skip BLAKE3 verification on the way down. |
| `--dry-run` | off | Preview rclone actions without transferring. |
| `--in-place` | off | Permit restore against a non-empty live `vol.Path`; overwritten files are moved to `.squirrel-restore-history/run-<id>/`. |

## Verification on the way down

By default, restore verifies each file's BLAKE3 as it arrives, the same
end-to-end check [`sync`](/squirrel/guides/syncing/) uses on the way up. Pass
`--shallow` to skip it.

:::note[Encrypted destinations are always size+mtime]
[Encrypted (`crypt`)](/squirrel/layouts/encrypted/) destinations cannot expose
content hashes through rclone, so restore from one falls back to a size+mtime
comparison — recorded as shallow — **even without** `--shallow`, exactly as sync
does. Passing `--shallow` changes nothing for these destinations.
:::

## Restoring in place

By default restore refuses to write into a non-empty live volume path, to avoid
clobbering current data. `--in-place` permits it — and any file it would
overwrite is first moved to `.squirrel-restore-history/run-<id>/`, mirroring the
append-only [`.squirrel-history`](/squirrel/layouts/mirror/) behavior on the sync
side. Nothing is destroyed.

## Content-addressed and packed destinations

`squirrel restore` handles the
[content-addressed](/squirrel/layouts/content-addressed/) and
[packed](/squirrel/layouts/packed/) layouts too — they have no mirrored tree to
copy, so restore works from the **local index** instead:

1. Each present path is resolved to its content BLAKE3 in the index.
2. The bytes are located per content: a per-hash object under `objects/`, or a
   member of a `tar.zst` pack under `packs/` (`pack_members` carries its
   offset and length).
3. Objects and packs are fetched through the same rclone (`crypt`) read path the
   push uses. **Packs are fetched once** — one download serves every requested
   member of that pack, never one fetch per file.
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
[index snapshot](/squirrel/configuration/index-snapshots/) along to destination
buckets under `.squirrel-index/`. A restore-from-cloud yields the data *and* the
index that explains it — use [`squirrel db restore`](/squirrel/reference/cli/#squirrel-db)
to swap that snapshot in as the live index.

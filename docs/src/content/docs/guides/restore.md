---
title: Restoring
description: Pull a volume back from one of its rclone destinations, with BLAKE3 verification on the way down.
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

## Restoring in place

By default restore refuses to write into a non-empty live volume path, to avoid
clobbering current data. `--in-place` permits it — and any file it would
overwrite is first moved to `.squirrel-restore-history/run-<id>/`, mirroring the
append-only [`.squirrel-history`](/squirrel/layouts/mirror/) behavior on the sync
side. Nothing is destroyed.

## Layouts that restore does not support yet

`squirrel restore` currently refuses:

- **[Kopia](/squirrel/layouts/kopia/) destinations** — restore goes through the
  kopia CLI (`kopia snapshot restore`) instead.
- **[Content-addressed](/squirrel/layouts/content-addressed/) and
  [packed](/squirrel/layouts/packed/) destinations** — recovery tooling ships
  separately, and the formats are simple enough to
  [recover without squirrel](/squirrel/reference/formats/#disaster-recovery-without-squirrel).

## Restoring the index too

For a full disaster-recovery scenario, remember that squirrel rides an
[index snapshot](/squirrel/configuration/index-snapshots/) along to destination
buckets under `.squirrel-index/`. A restore-from-cloud yields the data *and* the
index that explains it — use [`squirrel db restore`](/squirrel/reference/cli/#squirrel-db)
to swap that snapshot in as the live index.

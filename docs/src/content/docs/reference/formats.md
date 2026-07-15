---
title: Manifest & pack formats
description: The JSONL manifest segment and placement map formats used by content-addressed and packed destinations, plus how to recover data without squirrel.
---

[Content-addressed](/squirrel/layouts/content-addressed/) and
[packed](/squirrel/layouts/packed/) destinations record their path→content
mapping in simple, append-only JSONL files. The formats are deliberately simple
enough to recover data **without squirrel** — see below.

## Manifest segment format

Each `<volume>/index/run-<id>` segment is JSONL — one JSON object per line, lines
sorted by `(path, status)`:

```json
{"path":"2024/cat.jpg","blake3":"26e7…e5ad","status":"present","size_bytes":123,"mtime_ns":1712345678901234567}
```

| Field | Meaning |
|---|---|
| `path` | Volume-relative path. |
| `blake3` | 64-char lowercase hex BLAKE3-256 of the file content; the bytes live at `objects/<blake3>`. |
| `status` | `present`, `superseded`, `missing`, or `offloaded`. |
| `size_bytes` | File size as indexed. |
| `mtime_ns` | Modification time (nanoseconds) as indexed. |

### Replaying segments

Process segments in **ascending run id**; each line with status `present`,
`missing`, or `offloaded` sets that path's current `(content, status)` — last
write wins per path.

- `superseded` lines are **history only** (the outgoing content of a path that
  changed) and update no mapping.
- `missing` paths are known-but-lost at the origin — the object may still exist
  from an earlier upload.

A full recovery is: replay every segment, then for each `present`/`offloaded`
path download `objects/<blake3>` (decrypting with the `crypt` password if one was
set).

## Placement map format

Each `packs/map-<run>` is JSONL — one JSON object per newly packed content, in the
pack's member order:

```json
{"blake3":"26e7…e5ad","pack":"9f3a…1c02","offset":512,"length":123}
```

| Field | Meaning |
|---|---|
| `blake3` | 64-char lowercase hex BLAKE3-256 of the file content. |
| `pack` | The pack key; the bytes live at `packs/<pack>`. |
| `offset` | Byte offset of the content inside the pack's **uncompressed** tar. |
| `length` | Byte length of the content inside the pack's uncompressed tar. |

## Disaster recovery without squirrel

The path→hash mapping comes from the manifest segments; the packs add hash→bytes.
To recover one packed file end-to-end:

1. **Replay** the volume's `index/run-*` segments to resolve a path to its
   `blake3` (as above).
2. **Locate** that hash in any `packs/map-*` to get its `pack`, `offset`, and
   `length`. (A hash at or above the pack threshold has no map entry — its bytes
   are at `objects/<blake3>` instead.)
3. **Extract** — stream the pack through stock zstd and tar. The member is named
   by its hash, or equivalently is the `offset..offset+length` slice of the
   decompressed tar:

   ```sh
   rclone cat archive:packs/<pack> | zstd -d | tar -xO <blake3>
   ```

   (Decrypt with the `crypt` password first if the destination has one.)

The recovered bytes hash back to `<blake3>`, so recovery is **self-checking**.

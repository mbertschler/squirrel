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

### Deriving the stored names on an encrypted destination

On an encrypted destination the names in steps 2 and 3 are not the hashes
themselves: an [encrypted](/squirrel/layouts/encrypted/) content-addressed or
packed destination stores each artifact under a **keyed** name, so the remote
discloses no content hash. The hashes inside the manifest segments and placement
maps are unchanged — it is only the filename that is keyed — so recovery needs
one extra derivation, from the crypt passwords you already need in order to
decrypt:

```
naming_key       = BLAKE3_derive_key(context = "squirrel destination artifact naming v1",
                                     material = password || 0x00 || password2)
name(domain, x)  = hex(BLAKE3_keyed(naming_key, domain || 0x00 || x))
```

This applies to a root that carries the `.squirrel-naming` marker. An archive
written before keyed naming existed has no marker and stores the literal names of
the previous section; squirrel itself resolves which of the two a root uses
before reading it, and so should any script you write.

`password` and `password2` are the **plaintext** crypt passwords (if your config
stores them pre-obscured, reveal them with `rclone reveal` first), and
`password2` is the empty string when no salt is configured. `domain` is the
literal `object`, `pack`, or `volume`; `x` is the raw 32 bytes of the content
hash or pack key, or the volume name as bytes. So:

- `objects/<blake3>` → `objects/<name("object", blake3)>`
- `packs/<pack>` → `packs/<name("pack", pack)>`
- `<volume>/index/run-<id>` → `<name("volume", volume)>/index/run-<id>`

Run identifiers stay in clear, so segments and placement maps still sort into
replay order without the key. Both BLAKE3 modes are stock — `b3sum --derive-key`
and `b3sum --keyed` expose them, as does any BLAKE3 library.

:::caution[The naming key is derived, never stored]
Nothing at the destination holds the key, and the marker that records the scheme
(`.squirrel-naming`) carries no key material — so losing the destination loses
nothing extra, but **losing the crypt passwords now also loses the ability to
locate an artifact**, not just to decrypt it. The passwords were already
required for recovery; this does not add a secret, it widens what the existing
one protects.
:::

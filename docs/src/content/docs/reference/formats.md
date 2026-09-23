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
| `blake3` | 64-char lowercase hex BLAKE3-256 of the file content; the bytes live at `objects/<blake3>` (under a [keyed name](#deriving-the-stored-names-on-an-encrypted-destination) on an encrypted destination). |
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
set). On an encrypted destination, derive the stored names first — see
[Deriving the stored names](#deriving-the-stored-names-on-an-encrypted-destination).

## Placement map format

Each `packs/map-<run>` is JSONL — one JSON object per newly packed content, in the
pack's member order:

```json
{"blake3":"26e7…e5ad","pack":"9f3a…1c02","offset":512,"length":123}
```

| Field | Meaning |
|---|---|
| `blake3` | 64-char lowercase hex BLAKE3-256 of the file content. |
| `pack` | The pack key; the bytes live at `packs/<pack>` (under a [keyed name](#deriving-the-stored-names-on-an-encrypted-destination) on an encrypted destination). |
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

An [encrypted](/squirrel/layouts/encrypted/) content-addressed or packed
destination stores each artifact under a **keyed** name, so the remote discloses
no content hash and no volume name. That changes names in all three steps above:
the volume directory holding `index/run-*` in step 1, and the `objects/` and
`packs/` names in steps 2 and 3. The hashes inside the manifest segments and
placement maps are unchanged — only file and directory names are keyed — so
recovery needs one extra derivation, from the crypt passwords you already need in
order to decrypt:

```
material         = scrypt(password, salt, N = 16384, r = 8, p = 1, length = 80 bytes)
naming_key       = BLAKE3_derive_key(context = "squirrel destination artifact naming v1",
                                     material)
name(domain, x)  = hex(BLAKE3_keyed(naming_key, domain || 0x00 || x))
```

The scrypt step is rclone crypt's own key derivation, with the same parameters,
so guessing a password through the names costs the same work as guessing it
through the encrypted data.

`password` and `password2` are the **plaintext** crypt passwords (if your config
stores them pre-obscured, reveal them with `rclone reveal` first). `salt` is
`password2` as bytes, or — when no `password2` is configured — rclone crypt's
default salt, the 16 bytes `a80df43a8fbd0308a7cab83e581f86b1` (hex). `domain` is
the literal `object`, `pack`, or `volume`; `x` is the raw 32 bytes of the content
hash or pack key, or the volume name as bytes. So:

- `objects/<blake3>` → `objects/<name("object", blake3)>`
- `packs/<pack>` → `packs/<name("pack", pack)>`
- `<volume>/index/run-<id>` → `<name("volume", volume)>/index/run-<id>`

In Python, with the standard library's `hashlib.scrypt` and the
[`blake3`](https://pypi.org/project/blake3/) package:

```python
import hashlib
from blake3 import blake3

DEFAULT_SALT = bytes.fromhex("a80df43a8fbd0308a7cab83e581f86b1")

def naming_key(password: str, password2: str = "") -> bytes:
    salt = password2.encode() if password2 else DEFAULT_SALT
    material = hashlib.scrypt(password.encode(), salt=salt, n=16384, r=8, p=1, dklen=80)
    return blake3(material, derive_key_context="squirrel destination artifact naming v1").digest()

def name(key: bytes, domain: str, x: bytes) -> str:
    return blake3(domain.encode() + b"\x00" + x, key=key).hexdigest()
```

With `key = naming_key(password, password2)`, `name(key, "volume", b"pictures")`
is the directory of volume `pictures`, and `name(key, "object",
bytes.fromhex(blake3))` the `objects/` name of a manifest line's content.

Run identifiers stay in clear, so segments and placement maps still sort into
replay order without the key.

:::caution[The naming key is derived, never stored]
Nothing at the destination holds the key, and the marker that records the scheme
(`.squirrel-naming`) carries no key material — so losing the destination loses
nothing extra, but **losing the crypt passwords also loses the ability to
locate an artifact**, not just to decrypt it. The passwords were already
required for recovery; this adds no secret, it widens what the existing one
protects.
:::

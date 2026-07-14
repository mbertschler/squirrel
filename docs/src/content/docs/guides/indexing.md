---
title: Indexing
description: Walk a configured volume, hash regular files by BLAKE3, and update the index incrementally. Rows are never deleted — missing files are flagged.
---

`squirrel index` walks a config-declared volume, hashes regular files, and
updates the index.

```sh
squirrel index pictures
```

Indexing takes exactly one positional argument — a **volume name** declared in
config. Indexing by raw path is not supported.

## Incremental by default

Re-running `squirrel index` updates the index incrementally:

- new files are **added**,
- modified files are **re-hashed**,
- files no longer on disk are flagged as **missing** — rows are **not** deleted.

When content at a path changes, the prior row is flipped to `superseded` and a
new row inserted; the old hash is never rewritten in place. This is the
[core principle](/squirrel/start/principle/) in action.

Symlinks are skipped.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--shallow` | off | Skip re-hash when `(size, mtime)` match the stored row. |
| `--dry-run` | off | Report what would change without writing to the database. |
| `--workers N` | `NumCPU()` | Number of hashing workers (`0` = `runtime.NumCPU()`). |
| `--progress`, `-P` | auto on a TTY | Show a live, throttled progress line on stderr. |

`--shallow` trades integrity for speed: it trusts the filesystem's `(size,
mtime)` rather than re-reading and re-hashing file bytes. Use it for quick
re-scans of large trees; use a full index (or [`squirrel audit --deep`](/squirrel/guides/auditing/))
when you want to catch silent bit-rot.

## Progress output

`--progress` is auto-enabled when stdout is a TTY and off for pipes, cron, and
the agent. It shows files, bytes, and rate. `--progress=false` forces it off. The
final summary line is unchanged either way.

## What indexing feeds

The index is the source of truth for every other command:

- [`squirrel query`](/squirrel/reference/cli/#squirrel-query) reads it
  to look up content.
- [`squirrel sync`](/squirrel/guides/syncing/) pushes indexed content to
  destinations.
- [`squirrel offload`](/squirrel/guides/offloading/) checks durability against it
  before deleting local bytes.
- A volume [hook](/squirrel/guides/hooks/) fires after every successful index
  run.

---
title: Auditing for drift
description: Walk a volume looking for out-of-band drift since the last index — a size+mtime shortcut, a deep re-hash for bit-rot, or a folder-Merkle divergence check.
---

`squirrel audit` walks a volume looking for out-of-band drift since the last
index — changes the normal [index](/squirrel/guides/indexing/) cadence may not
have caught yet.

```sh
squirrel audit               # every declared volume
squirrel audit pictures      # one volume
```

With no argument it fans out to every declared volume (sorted).

## Modes

| Flag | Default | Meaning |
|---|---|---|
| _(none)_ | on | Compare on-disk `(size, mtime)` against the index — the fast shortcut. |
| `--deep` | off | Re-hash **every** file (bit-rot check) instead of the size+mtime shortcut. |
| `--folders` | off | Re-derive folder Merkle hashes from the index and report divergence — **no disk walk**. |

`--folders` and `--deep` are mutually exclusive.

### The default: size+mtime

The default mode is a cheap comparison of the on-disk `(size, mtime)` against the
recorded rows — it catches files that changed without being re-indexed.

### `--deep`: catch bit-rot

`--deep` re-reads and re-hashes every file, catching silent corruption where the
bytes changed but `(size, mtime)` did not. It is the local counterpart to the
offsite [scan-back fingerprint](/squirrel/guides/verification/) — one checks the
copy on your disk, the other the copy in cold storage.

### `--folders`: index-only Merkle check

`--folders` re-derives folder Merkle hashes from the index and reports where they
diverge, without touching the disk at all — a fast integrity check of the index's
own internal consistency.

## Audit runs

An audit is recorded as an `audit`-kind run, visible in
[`squirrel runs`](/squirrel/reference/cli/#squirrel-runs) and the TUI. Like all
runs, audit rows are never auto-pruned — see
[Runs & the audit trail](/squirrel/concepts/runs/).

---
title: Offloading
description: Delete local bytes only after proving the content is durable on every required target. The only squirrel command that deletes user data — never a blind delete.
---

`squirrel offload` deletes the **local** copy of files whose content is provably
stored on every target the volume's offload policy requires — never a blind
delete. It is the **only** squirrel command that deletes user data.

```sh
squirrel offload pictures 2019/             # a subtree
squirrel offload pictures --older-than 90d  # by age (indexed mtime)
squirrel offload pictures . --dry-run       # print the gate decisions, touch nothing
```

Selectors are volume-relative paths/prefixes plus `--older-than` (combinable);
selecting the whole volume takes an explicit `.`. At least one selector is
required.

## The offload policy

```toml
[volumes.pictures]
path                     = "~/Pictures"
sync_to                  = ["nas", "offsite"]
offload_requires         = ["nas", "offsite"]
offload_max_evidence_age = "720h"   # optional; default disabled
```

`offload_requires` is the explicit per-volume policy: every named target's
recorded durability must cover a file's content before its bytes may go. **A
volume without the key refuses to offload entirely.** The names share the flat
destination/node namespace that `sync_to` uses; they may also name targets only a
*peer* pushes to (evidence arrives through the
[peer durability pull](/squirrel/guides/peer-sync/)), and a name with no recorded
evidence simply keeps the gate closed.

## The durability gate

The gate is evaluated **per file, entirely offline**, against the durability
version vectors in the local index: content with origin `(node, run)` passes for
a target iff the target's recorded vector component for that node is ≥ `run`, for
**every** required target.

Files failing the gate are skipped and reported per target
(`missing component for origin X` / `stale: have 40 need 45`).

## Evidence staleness (opt-in)

`offload_max_evidence_age` is an optional, opt-in defence-in-depth knob: when
set, a target whose durability evidence was last *re-verified* longer ago than
this (in wall-clock time) is treated as stale and refuses the offload, even when
its version-vector coverage is sound. This stops the gate from trusting a
destination that has been dead or unreachable for months on the strength of a
claim never since re-confirmed.

It is **fail-closed**: evidence with an unknown verification time (a row migrated
from before the column existed, or one only ever touched without re-verification)
is refused too.

- A local re-verification (a fresh push) re-stamps the evidence to now and clears
  the staleness.
- A durability pull re-stamps only to the *responding peer's own* last
  verification instant, relayed over the wire and capped at now — so a
  destination gone dead behind a peer that keeps answering ages out here too, and
  the pull cannot make evidence look fresher than the peer actually holds it.

The default is **disabled** (no maximum age), so existing configs are unaffected.
Only an equal-value re-confirmation that carries no verification never refreshes
the clock, so the age tracks genuine checks rather than no-op touches.

## The unlink is re-checked

Immediately before each unlink, squirrel re-verifies the on-disk bytes against
the indexed row — size, mtime, and BLAKE3, with symlink-refusing traversal — and
skips loudly on any difference: the disk is newer than the index, and unindexed
bytes are never deleted.

Offloaded files flip `present → offloaded` in the index under one `kind='offload'`
run. The indexer treats an offloaded row's on-disk absence as expected (it never
becomes `missing`), and re-acquiring the bytes ([restore](/squirrel/guides/restore/)
or copy-back) flips the row back to `present`.

:::tip[Preview first]
Run with `--dry-run` to print the per-file durability gate decisions without
deleting anything.
:::

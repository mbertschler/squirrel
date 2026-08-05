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
[peer durability pull](/squirrel/guides/peer-sync/)), and a name whose evidence
has not arrived yet simply keeps the gate closed.

:::caution[An unsatisfiable requirement is a config error, not a closed gate]
A target whose layout can *never* produce durability evidence — a plain crypt
mirror, say — is rejected when the config loads, rather than leaving a gate that
silently never opens. The check covers targets only a peer pushes to as well,
using the capabilities that peer reports. This distinction is the whole point:
a refusal you see always means **not yet**, never **never**.
:::

## The durability gate

The gate is evaluated **per file, entirely offline**, against the durability
version vectors in the local index. A file passes for a target only when all
three of these hold, for **every** required target:

1. **Coverage** — content with origin `(node, run)` needs the target's recorded
   vector component for that node to be ≥ `run`.
2. **Verification method** — the component must be *content-verified*. A
   component recorded as `presence+size` (the object is there and the right
   size) is refused: presence is not proof of bytes.
3. **Freshness** — if [`offload_max_evidence_age`](#evidence-staleness-opt-in)
   is set, the evidence must have been re-verified within it.

Files failing the gate are skipped and reported per target
(`missing component for origin X` / `stale: have 40 need 45` /
`not content-verified (method "presence+size", asserted by peer nas)`).

### How a component becomes content-verified

Content-addressed and packed uploads are recorded as `presence+size` at write
time — a crypt remote exposes no hash to compare, and a pack's members are not
individually addressable at the destination. They are **upgraded** when a
[`squirrel verify`](/squirrel/guides/verification/) pass finds every underlying
object and pack fingerprint-verified: the component is re-stamped as
content-verified and relays to peers that way, so a hub's certified archive can
open an edge machine's gate.

The practical consequence: on a cold-archive target, offload becomes possible
after verification has run, not merely after the sync succeeded. Give
`verify` [its own agent cadence](/squirrel/guides/agent/) and this happens
unattended; run it by hand and offload waits on you.

:::note[Verify also re-attempts the advance]
A destination with any still-pending fingerprint holds its whole durability
vector back, not just the pending object — an advance that skipped a pending
pack would vouch for content only reachable through it. Verify certifies the
outstanding set and re-attempts the advance itself, so the vector does not wait
for the next content-writing sync to notice.
:::

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

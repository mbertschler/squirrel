---
title: Peer sync
description: Inspect node-sync state and exchange peer metadata — the watermark transition log and pulling a peer's destination durability vectors into the local index.
---

`squirrel peer-sync` inspects node-sync state and exchanges peer metadata between
squirrel nodes. On its own it prints help — the behavior lives in its
subcommands.

```sh
squirrel peer-sync
```

Peer sync lets multiple squirrel nodes share durability evidence about
destinations, so a node can trust that content is durable on a target that only
a *peer* pushes to. This feeds the [offload](/squirrel/guides/offloading/)
durability gate.

## How the bytes travel

A peer sync is one conversation over one connection. The initiator opens a
session against the peer's `endpoint`, negotiates a per-path plan, streams
each content object it owes straight to that same endpoint, and asks the
receiver to verify and commit. Bearer token, TLS, and the optional
certificate pin cover the transfer exactly as they cover the plan — there is
no second address to configure and no second trust anchor to get right.

Two properties fall out of keying the transfer by content hash rather than
by path:

- **Duplicate files cross the wire once.** Several paths wanting the same
  BLAKE3 in one run are satisfied by a single upload, which the receiver
  fans out locally.
- **The receiver is the authority on what landed.** It hashes the stream as
  it writes and refuses anything that does not match the digest it was
  addressed to, so a file edited between indexing and sending is rejected
  rather than stored under the wrong hash. The verify phase then re-reads
  what is on disk, and only a clean verify advances durability.

A problem with one file costs only that file. If it was deleted or rewritten
since it was indexed, or the receiver cannot take its bytes, the run ends
**partial**: it names the path and why, and commits everything else. A
receiver-side failure is retried within the run; a local file that changed
waits for the next index. The run gives up on the peer as a whole only when
the peer itself is unwell — several uploads in a row failing, or one it stops
taking for ten minutes, as a hung disk behind a live agent would. Either way
the receiver keeps every upload it verified on the way in, so the next run
picks up where this one stopped rather than starting over.

Peer sync uses no external binary — [rclone](/squirrel/start/install/) is for
bucket destinations. A machine whose only targets are peers needs none
installed.

Both ends must speak peer-sync protocol v4 or later. An older peer is refused
with an upgrade instruction rather than silently degraded.

## Watermark history

```sh
squirrel peer-sync history <volume> <peer>
```

Lists the **watermark transition log** for a `(volume, peer)` pair — the record
of how the last-shared-run watermark advanced over time. Output is oldest-first,
with columns `AT` and `LAST_SHARED_RUN_ID`. Both arguments are required; `<peer>`
is a node name.

## Pull durability

```sh
squirrel peer-sync pull-durability <volume> <peer>
```

Fetches a peer's destination **durability vectors** for a volume into the local
index. This is how evidence about targets only a peer pushes to reaches this node
— a name in `offload_requires` with no locally recorded evidence keeps the
offload gate closed until a pull brings that evidence in.

| Flag | Default | Meaning |
|---|---|---|
| `--allow-rewind` | off | Accept peer components below the locally recorded value (a recovery override). |

By default a pull never lowers a locally recorded durability component — evidence
only moves forward. `--allow-rewind` overrides that for recovery scenarios where
the local index is known to be ahead of reality.

:::note[Effect on offload staleness]
A durability pull re-stamps evidence only to the *responding peer's own* last
verification instant, relayed over the wire and capped at now — it cannot make
evidence look fresher than the peer actually holds it. See
[offload evidence staleness](/squirrel/guides/offloading/#evidence-staleness-opt-in).
:::

Give the pull [its own cadence](/squirrel/guides/agent/) with
`pull_durability_every` rather than relying on it riding along with a sync. A
receive-only node never initiates a sync, and any node stops refreshing when
nothing changes — which is exactly when `offload_max_evidence_age` starts
counting against it.

## Conflicts and the contested freeze

When the same path is edited on two machines between cadences, both pushes are
legitimate and squirrel refuses to lose either. The receiver keeps one version
live and preserves the other under `<volume>/.squirrel-conflicts/run-<id>/`.

That alone would let the two machines re-assert their versions at each other
forever, one conflict copy per tick. So the first conflict also raises a
**contested freeze** on the path: while it stands, a divergent re-assertion from
any peer is refused rather than applied, and the flip-flopping stops at the first
conflict, preserved once.

```sh
squirrel conflicts                              # what is frozen
squirrel conflicts resolve <volume> <path>      # unfreeze it
```

The freeze is not just the hub's business — each node mirrors it into its own
index, so the losing edge machine sees a contested badge on its own dashboard
instead of green 0-file syncs while its local file quietly differs from the
household's copy. Conflict and contested counts also land on the initiators' run
rows.

Resolving **clears the latch; it does not pick a winner.** The version that is
live stays live. Adopting the preserved copy instead is a deliberate
[`restore`](/squirrel/guides/restore/) — never something `resolve` does silently.
Squirrel never resolves a conflict on its own; the latch exists precisely because
that call is yours.

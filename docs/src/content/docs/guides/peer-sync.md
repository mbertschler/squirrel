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

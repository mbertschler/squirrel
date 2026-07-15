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

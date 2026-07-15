---
title: Runs & the audit trail
description: Every index, sync, audit, and offload is recorded as a run. Runs are an audit trail, never auto-pruned — any retention is explicit and operator-driven.
---

Every operation squirrel performs is recorded as a **run** in the `runs` table:
[index](/squirrel/guides/indexing/), [sync](/squirrel/guides/syncing/),
[audit](/squirrel/guides/auditing/), [verify](/squirrel/guides/verification/), and
[offload](/squirrel/guides/offloading/) each write a run row.

## Runs are never auto-pruned

The `runs` table follows the same no-loss spirit as the
[content model](/squirrel/concepts/content-model/), by **policy** rather than
schema: squirrel never auto-prunes runs. They are an audit trail, and any
retention is explicit and operator-driven only.

This means the run history grows over time — that is intentional. It is the
record of what squirrel did to your data, and it is the evidence behind the
[offload durability gate](/squirrel/guides/offloading/): content with origin
`(node, run)` is only offloaded once a target's recorded durability vector covers
that run.

## Listing runs

```sh
squirrel runs
squirrel runs --volume pictures --limit 5
```

Runs are listed most-recent-first. `--limit 0` shows all of them.

## Run kinds

| Kind | Written by |
|---|---|
| index | `squirrel index` (and the [agent](/squirrel/guides/agent/) on cadence) |
| sync | `squirrel sync` |
| audit | `squirrel audit`, `squirrel verify`, and `squirrel runs fail` (a `manual-fail` audit row) |
| offload | `squirrel offload` (`kind='offload'`) |

## Fixing a stuck run

If a run is interrupted (a crash, a killed process) it can be left in status
`running`. Mark it failed so it no longer blocks:

```sh
squirrel runs fail <id>
```

Only runs currently in status `running` may be failed. This writes a
`manual-fail` audit row and preserves the running row's file count — nothing in
the audit trail is erased.

## Durability version vectors

Each destination's durability is tracked as a **version vector** — per origin
node, the highest run whose content is confirmed durable at that destination. The
[offload gate](/squirrel/guides/offloading/) and
[peer sync](/squirrel/guides/peer-sync/) both operate on these vectors: offload
reads them to decide what is safe to delete locally, and peer-sync exchanges them
so a node can trust durability on a target only a peer pushes to.

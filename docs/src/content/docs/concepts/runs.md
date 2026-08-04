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

Under agent cadences the list is dominated by no-ops — one row per
(volume × destination) per tick that found nothing to do. Two filters cut through
it:

```sh
squirrel runs --failed     # only failed, refused, aborted, or partial
squirrel runs --changes    # hide clean no-ops
```

`--changes` keys on how many files a run actually *changed*, which is not the
same as how many it considered: a bucket push or an index run counts everything
it examined. Runs recorded before that count existed are shown rather than
folded — their change count is unknown, and squirrel does not guess.

## Run statuses

| Status | Meaning |
|---|---|
| `running` | In flight. |
| `success` | Completed, no errors. |
| `partial` | Completed with some per-file errors. |
| `failed` | A mid-flight failure. The error is recorded on the row — including for rclone destinations, whose stderr tail is preserved. |
| `refused` | A preflight refusal: a missing `.squirrel-volume` marker, an un-bootstrapped kopia repository, a layout guard. The run row exists so the refusal is visible in the audit trail and the TUI, not only on the agent's stderr. |
| `aborted` | A `running` row reaped because the process that owned it died mid-run. The agent reaps its own orphans at startup, so a killed agent no longer leaves a phantom run ticking on the dashboard. |

A `refused` run consumes its cadence window like any other terminal status, which
is what backs the scheduler off a dead destination instead of re-minting an
identical refusal every tick. `aborted` is deliberately excluded: a run reaped
mid-flight never completed, so the cadence re-attempts it.

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

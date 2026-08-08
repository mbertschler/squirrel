---
title: The agent
description: Run squirrel unattended — an HTTP server with scheduled audits and cadence-driven index/sync, plus hook firing.
---

`squirrel agent` runs squirrel unattended: an HTTP server with scheduled audits
and cadence-driven index/sync. It is how you keep an index fresh and backups
flowing without running commands by hand.

```sh
squirrel agent
```

The agent requires an `[agent]` block in config.

## What the agent does

- **Cadence-driven index/sync** — runs [`index`](/squirrel/guides/indexing/) and
  [`sync`](/squirrel/guides/syncing/) on the configured cadence
  (`index_every` / `sync_every`).
- **Scheduled audits** — runs [`audit`](/squirrel/guides/auditing/) passes on a
  schedule.
- **Scheduled verify** — re-checks each content-addressed/packed destination's
  recorded objects and packs against their upload fingerprints on its
  `verify_every` cadence (per-destination, or an `[agent]` default) — the same
  pass as [`squirrel verify`](/squirrel/guides/verification/), recorded as an
  `audit` run. Offsite bitrot detection stops depending on anyone typing it.
- **Scheduled durability pull** — refreshes a peer's relayed durability
  evidence on its `pull_durability_every` cadence, independent of any sync. A
  receive-only node keeps its [offload](/squirrel/guides/offloading/) gate
  evidence fresh unattended.
- **Hook firing** — fires per-volume [hooks](/squirrel/guides/hooks/) on change
  (after each successful index run) and on their `interval`.
- **HTTP server** — exposes a health endpoint, serves peer syncs, and drives
  the state the [TUI](/squirrel/guides/tui/) and desktop app read. Only when
  `[agent] listen` is set (see below).
- **Config drift detection** — notices when the config file on disk stops
  matching the one it is running, and says so on every surface until it is
  restarted (see below).

## One slow destination cannot stall the rest

Syncs are dispatched **per destination**, so a cloud target that has gone dark
does not hold up local NAS→HTPC replication behind it. Two bounds keep a sick
destination from occupying its own worker forever:

- every automatic rclone transfer runs with connect and I/O timeouts, so a dead
  endpoint fails rather than hanging;
- a **stall timeout** (10 minutes without progress by default) covers the case
  those miss — an endpoint that is live but stuck, accepting the connection and
  then never moving bytes.

A pair that is already syncing when its cadence comes round again is skipped and
logged, not queued up behind itself.

## Config edits need a restart — and the agent says so

The agent reads its config **once**, at startup. Editing the file changes
nothing about the running process: rotated credentials stay unrotated in memory,
a newly declared volume is not indexed, a removed destination is still synced to.

What the agent will *not* do is stay quiet about it. It hashes the config file's
contents at load and re-reads that file every minute. When the bytes on disk stop
matching the ones it parsed, it raises a **standing state** — the same latch shape
a [verify](/squirrel/guides/verification/) alarm uses — and both
[`squirrel status`](/squirrel/reference/cli/#squirrel-status) and the
[TUI](/squirrel/guides/tui/) then say:

```
config on disk has changed since this agent started; restart to apply
```

The latch outlives the check that found it and stays up until one of two things
happens:

- **you restart the agent**, which is what applies the edit; or
- **the file's contents come back** to what the agent loaded — you undid the
  edit, so there is nothing left to apply.

Two properties are worth knowing:

- **It compares content, not timestamps.** A rewrite that produces identical
  bytes — `touch`, an editor saving an unmodified buffer, a
  configuration-management tool re-rendering the same template — is not a change
  and raises nothing.
- **It detects; it does not reload.** Swapping a live config under in-flight runs
  and armed cadences is a different and far more delicate thing, and a reload
  that could wedge the agent would be worse than the restart it replaced.

Each drift episode is recorded once as an `audit`
[run](/squirrel/concepts/runs/), and the clear is recorded against that same run,
so "the config changed at 02:14 and was applied at 09:30" survives in the audit
trail after the latch itself is gone.

:::note[Drift is not a config check]
The latch answers "is the running agent up to date with the file?", not "is the
file valid?". For the second question — does it parse, do the `{ env = "…" }`
secrets resolve, do the paths exist — run
[`squirrel config check`](/squirrel/reference/cli/#squirrel-config-check).
:::

## Orphaned runs are reaped at startup

A run interrupted by a killed agent would otherwise sit at `running` forever and
render as a live, elapsed-ticking banner on the dashboard. The agent reaps its
own orphans on startup, marking them
[`aborted`](/squirrel/concepts/runs/#run-statuses) — a terminal status that,
unlike the others, does *not* consume the cadence window, so the work is
re-attempted rather than treated as a finished pass.

## Listener-less (cadence-only) machines

A machine that only *originates* content — a roaming laptop that pushes to a
hub and never receives peer syncs — has no reason to run an HTTP listener. Omit
`[agent] listen` and the agent runs its schedulers (index/sync/audit cadences)
**without binding a listener**; with no listener there is nothing to
authenticate, so `[agent.auth]` is optional too.

```toml
[agent]
# no `listen`: schedulers only, no HTTP server

[volumes.photos]
path       = "~/Pictures"
sync_to    = ["nas"]
sync_every = "1h"
```

When `listen` *is* set, behaviour is unchanged: the agent runs the schedulers
**and** the HTTP server (and then a bearer token is required). A listener-less
agent with no cadence and no `scan_interval` has nothing to do and refuses to
start, rather than idling silently.

## Database path precedence

When run as the agent, the database path resolves as:

1. the `--db` flag,
2. `cfg.Agent.DB`,
3. `cfg.DB`,
4. the built-in default.

## The agent never bootstraps

:::caution[The agent never passes `--init`]
An unattended sync driven by the agent can never silently create an empty target
on a transient outage — [first-use bootstrap](/squirrel/guides/syncing/#first-use-and-the-squirrel-volume-marker)
is always a human-driven, one-time act with `--init`. This is a deliberate safety
property: it keeps the [offload](/squirrel/guides/offloading/) durability gate
from ever trusting a freshly minted empty destination.
:::

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
- **HTTP server** — exposes a health endpoint and drives the state the
  [TUI](/squirrel/guides/tui/) and desktop app read.

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

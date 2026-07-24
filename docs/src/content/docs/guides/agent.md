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
- **Hook firing** — fires per-volume [hooks](/squirrel/guides/hooks/) on change
  (after each successful index run) and on their `interval`.
- **HTTP server** — exposes a health endpoint, serves peer syncs, and drives
  the state the [TUI](/squirrel/guides/tui/) and desktop app read. Only when
  `[agent] listen` is set (see below).

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

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
- **Config reload** — notices when the config file on disk stops matching the
  one it is running, applies what it can without a restart, and says on every
  surface which part of the edit a restart still owes (see below).

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

## Config edits apply themselves

Edit the config file and save it. Within a minute the running agent is using it
— no restart, no command, nothing to remember.

The agent hashes the config file's contents at load and re-reads that file every
minute. When the bytes on disk stop matching the ones it is running it loads the
file, works out what changed, and adopts what it can. **It compares content, not
timestamps**: a rewrite that produces identical bytes — `touch`, an editor
saving an unmodified buffer, a configuration-management tool re-rendering the
same template — is not a change and does nothing at all.

### What reloads, and what still wants a restart

The line is between what the agent *does* and what the agent *is*.

**Reloaded in place** — policy the agent consults on its next tick:

| Config | Effect of a reload |
|---|---|
| `[volumes.*]` | Added, removed, repathed; new `sync_every` / `index_every` / `hook` cadences arm on the next scheduler tick, and the peer-sync endpoints host a new volume immediately |
| `[destinations.*]` | Added, removed, retuned; the managed `rclone.conf` is re-rendered as part of adopting the change |
| `[nodes.*]` | Added, removed, re-addressed; a new `pull_durability_every` arms on the next tick |
| `[backups]` | Applies to the next snapshot-on-sync |
| `[agent] verify_every` | The fleet-wide default cadence is re-resolved per tick |

**Restart required** — the shape of the process itself, which cannot change
under a running listener or an armed loop:

| Config | Why |
|---|---|
| `[agent] listen`, `[agent.tls]` | Rebinding the HTTP surface under live peer syncs |
| `[agent.auth] token`, `[agent.auth.peers.*]` | Swapping the credentials in-flight requests are being authenticated against |
| `[agent] scan_interval`, `scan_strategy` | Re-arming the audit loop mid-pass |
| `db`, `[agent] db`, `node_name` | The index handle is open and this node's identity is already recorded in it |

When an edit touches both halves, the agent applies the first half and latches a
**standing state** for the second — the same latch shape a
[verify](/squirrel/guides/verification/) alarm uses. Both
[`squirrel status`](/squirrel/reference/cli/#squirrel-status) and the
[TUI](/squirrel/guides/tui/) then name what is left:

```
config on disk has changed; the agent applied what it could and agent.auth.token still need a restart
```

That latch stands until you restart — or until the file's process-shaped keys go
back to what the running process actually bound, which is what happens if you
rotate a credential and then think better of it.

### A config that will not load never reaches the agent

The agent validates the whole file before it applies any of it. If the file no
longer parses, or state derived from it cannot be rebuilt (rclone has gone
missing, say, for a destination the edit just added), **nothing** is adopted: the
agent keeps running the last configuration it knows works, and the latch says
why.

```
config on disk has changed but could not be applied (parse …: …); the agent is still running the last config it loaded
```

Fix the file and the next check picks it up. A reload can never wedge the agent,
which is exactly why it is safe to do automatically.

:::note[Drift is not a config check]
The latch answers "is the running agent up to date with the file?", not "is the
file valid?" ahead of time. For the second question — does it parse, do the
`{ env = "…" }` secrets resolve, do the paths exist — run
[`squirrel config check`](/squirrel/reference/cli/#squirrel-config-check) before
you save.
:::

### In-flight work is never disturbed

A reload lands between scheduler ticks and between requests. A sync already
pushing when the config changed finishes against the policy it started with; a
peer-sync session already open keeps the volume it was opened for. Nothing is
cancelled, re-planned, or re-armed underneath itself — the new configuration
governs the *next* piece of work, not the one in progress.

### It is all in the audit trail

Every reload is recorded as an `audit` [run](/squirrel/concepts/runs/) naming
which config keys were applied and which are still pending, so "the agent
changed its own configuration at 02:14, and this is what it changed" outlives
the event. A latch raised for a pending restart is its own `audit` run, and the
clear is recorded against that same run — restart, revert, or reload, the trail
says which resolved it.

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

---
title: Hooks
description: A per-volume hook nudges an external tool when content changes or on a timer. Squirrel stays tool-agnostic — it exec's the command without a shell and records only the generic outcome.
---

A volume can declare a per-volume **hook** — a command the [agent](/squirrel/guides/agent/)
runs to nudge an external tool when the volume's content changes. Squirrel stays
tool-agnostic: it never learns what the command does (a backup with
kopia/restic, an `rclone copy`, a shell script — all the same to squirrel). It
exec's the command **without a shell**, passes context through environment
variables, and records only the generic outcome (exit code, timestamps).

```toml
[volumes.pictures.hook]
command  = ["kopia", "snapshot", "create", "."]
timeout  = "30m"   # optional, defaults to 1h
interval = "24h"   # optional — also fire on this cadence (see below)
```

:::note[The generic outcome is the ceiling]
Only the built-in destination types report verification results; a hook's exit
code never counts as one. If you want squirrel to own a kopia snapshot end-to-end
and record its verification, use a [kopia destination](/squirrel/layouts/kopia/)
instead.
:::

## Two triggers

A hook fires on two triggers, both reusing the same command:

- **on change** — after every successful index run on the volume (which the agent
  runs on the `index_every` / `sync_every` cadence). This answers *"is the latest
  content backed up?"*. It keys off content settling, not a sync to a remote, so
  a volume needs no `sync_to` destination for the hook to be useful.
- **on interval** — every `interval`, *regardless of whether anything changed*.
  This answers *"is the existing backup still intact?"*. Verification is
  orthogonal to change — bitrot happens to static data — so re-checks have to run
  on a clock. Omit `interval` to fire on-change only.

## Environment passed to the command

The command tells the two triggers apart via `SQUIRREL_TRIGGER`, so a single
command can back up on change and verify on interval:

| Variable | Meaning |
|---|---|
| `SQUIRREL_VOLUME` | volume name |
| `SQUIRREL_PATH` | absolute volume path |
| `SQUIRREL_RUN_ID` | the index run that triggered the hook (empty on the interval trigger) |
| `SQUIRREL_CHANGED` | `true`/`false` — whether the run observed changes (so the command can cheaply no-op); always `false` on the interval trigger |
| `SQUIRREL_TRIGGER` | `change` or `interval` |

Because the command is exec'd without a shell, the volume path is never
string-concatenated into a command line. If you want shell features, make the
command `["sh", "-c", "…"]` yourself.

## Best-effort

A hook is **best-effort**: a hook failure or timeout never fails or blocks the
run that triggered it, and overlapping invocations for the same volume are
skipped rather than stacked.

Recorded outcomes are visible via `squirrel hooks` and the TUI's Hooks tab:

```sh
squirrel hooks
squirrel hooks --volume pictures --limit 5
```

:::caution[Don't double-schedule verification]
If your external tool already runs its own verify on a timer (e.g. a
cron/systemd job), don't *also* set `interval` for a verify command — two heavy
passes will step on each other. Pick one driver: let squirrel schedule it (so the
result lands in `squirrel hooks` / the TUI) **or** let the tool schedule it
(maximum independence — verification keeps happening even when the agent is
down), not both.
:::

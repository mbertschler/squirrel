---
title: Terminal UI
description: The trust surface — answer "am I safe?" at a glance, with per-target coverage, durability, standing alarms, contested paths, and config drift, plus live runs and an ncdu-style index browser.
---

`squirrel tui` opens an interactive terminal UI. Its job is to answer one
question without scrolling or drilling: **am I safe?**

```sh
squirrel tui
squirrel        # bare invocation opens the TUI when stdin/stdout are a terminal
```

`squirrel tui` always opens the TUI regardless of TTY state. A bare `squirrel`
invocation opens it **only** when both stdin and stdout are a terminal; otherwise
it prints help.

## The dashboard

The first screen is ordered by urgency — anything wrong appears above the
routine state, so a healthy household is a short screen.

- **Config drift** — shown right under the agent's health when the config file
  on disk no longer matches the one the [agent](/squirrel/guides/agent/) is
  running: *"config on disk has changed since this agent started; restart to
  apply"*. It stands until the agent is restarted or the file's contents come
  back, so a forgotten restart cannot hide.
- **Alarms** — standing per-destination alarms raised by a
  [verify](/squirrel/guides/verification/) mismatch. An alarm latches: it
  outlives the run that found it and stays until a clean pass or an explicit
  [`squirrel verify ack`](/squirrel/reference/cli/#squirrel-verify-ack) clears
  it. A destination in alarm does not quietly go back to green on the next sync.
- **Contested paths** — peer-sync conflicts frozen by the
  [contested freeze](/squirrel/guides/peer-sync/), with a hint at how to clear
  them. Every node mirrors the freeze, so a losing edge machine sees it too.
- **Live runs** — index and sync progress as it happens.
- **Coverage** — per volume, one row per configured target: role, last sync,
  state, durability, verify method, and evidence age, each cell coloured by
  severity. This is the per-(volume × destination) grid: a target that has been
  failing for a week cannot hide behind a fresh ✓ earned by a different one.
  Each volume's header line carries its index freshness and offload readiness.
- **Fleet** — under each volume's grid, one row per *other* place it lives: the
  other machines as well as the destinations. Whether each is behind or ahead of
  this machine, how many files have not reached it, when it last changed, and
  when it was last verified. This is the answer to "where else does this volume
  live, and is any of it out of date" without walking to another machine — and
  it is the only place a machine that pushes *to* this one shows up at all,
  since no target row mentions it.

The coverage panel and
[`squirrel status`](/squirrel/reference/cli/#squirrel-status) render the same
facts from the same query layer, so a headless box and a TUI never disagree.
Use `status` when you want an exit code; use the TUI when you want to look.

## Other tabs

| Tab | Key | What it shows |
|---|---|---|
| Dashboard | `1` | The "am I safe?" screen above. |
| Runs | `2` | The [audit trail](/squirrel/concepts/runs/), newest first — drill into an individual run and what it touched. |
| Volumes | `3` | Declared volumes; browse the indexed tree **ncdu-style**, walking by size the way [ncdu](https://dev.yorhel.nl/ncdu) explores disk usage. |
| Hooks | `4` | Recorded [hook](/squirrel/guides/hooks/) runs and their outcomes. |

## Beyond the terminal

Squirrel also ships a **desktop app** (`squirrel-desktop`) built on the same
underlying handlers — a menubar app with a browsable index, run history, and
progress view. The TUI and desktop app both read the state the
[agent](/squirrel/guides/agent/) maintains, so what you see reflects live
activity.

:::note[Every row carries an "as of"]
Fleet rows describe another machine as of the last exchange with it, not as of
now. A peer that has gone dark reads as **unknown**, never as fine — the same
fail-closed stance the [offload gate](/squirrel/guides/offloading/) takes toward
stale evidence.
:::

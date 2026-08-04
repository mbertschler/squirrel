---
title: Terminal UI
description: The trust surface — answer "am I safe?" at a glance, with per-target coverage, durability, standing alarms, and contested paths, plus live runs and an ncdu-style index browser.
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
  Each volume block closes with its offload readiness.

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

:::note[One node's answer]
Every surface reads the local index, so a machine's TUI answers for *that*
machine. In a multi-machine household each node has its own database and its own
dashboard; there is no combined fleet view yet.
:::

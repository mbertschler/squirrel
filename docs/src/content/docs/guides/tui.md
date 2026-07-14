---
title: Terminal UI
description: Watch live runs, browse the index ncdu-style, drill into run records, and see hook outcomes — all from an interactive terminal UI.
---

`squirrel tui` opens an interactive terminal UI to watch live runs, browse the
index ncdu-style, and drill into individual run records.

```sh
squirrel tui
squirrel        # bare invocation opens the TUI when stdin/stdout are a terminal
```

`squirrel tui` always opens the TUI regardless of TTY state. A bare `squirrel`
invocation opens it **only** when both stdin and stdout are a terminal; otherwise
it prints help.

## What you can do

- **Watch live runs** — index and sync progress as it happens.
- **Browse the index ncdu-style** — walk the indexed tree by size, the way
  [ncdu](https://dev.yorhel.nl/ncdu) lets you explore disk usage.
- **Drill into run records** — inspect an individual [run](/squirrel/concepts/runs/)
  and what it touched.
- **See hook outcomes** — the Hooks tab shows recorded
  [hook](/squirrel/guides/hooks/) runs.

## Beyond the terminal

Squirrel also ships a **desktop app** (`squirrel-desktop`) built on the same
underlying handlers — a menubar app with a browsable index, run history, and
progress view. The TUI and desktop app both read the state the
[agent](/squirrel/guides/agent/) maintains, so what you see reflects live
activity.

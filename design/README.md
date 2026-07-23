# design/

Design documents for squirrel's user experience. The feature set is
considered adequate; the current work is making it *feel* right for the
target user. These documents are the yardstick that UX changes are
measured against — when a proposed change conflicts with them, either
the change or the document has to give, explicitly.

- [`ux-principles.md`](ux-principles.md) — the north star: set up once,
  then trust. What the CLI, agent, and TUI are each *for*.
- [`reference-setup.md`](reference-setup.md) — the canonical household
  setup all UX work is anchored on: five machines, three volumes, every
  supported destination type in play.
- [`testbed.md`](testbed.md) — how the reference setup is simulated on
  one development machine with real commands and no containers.

The method: walk the reference setup through its whole lifecycle on the
testbed — bootstrap, steady state, the scary moments, recovery — with
real commands, and log every point of friction. Fixes are prioritized
from that log, not from intuition.

# design/

The durable product vision and design principles for squirrel. This
folder — together with the content-safety core principle in
[`AGENTS.md`](../AGENTS.md) — is the standing context for anyone (human
or agent) building features: it exists so that every change pulls in
the same direction without the maintainer having to restate the vision
each time.

- [`ux-principles.md`](ux-principles.md) — the north star: set up once,
  then trust. What the CLI, agent, and TUI are each *for*.
- [`reference-setup.md`](reference-setup.md) — the canonical household
  setup every feature must make sense in: five machines, three volumes,
  every supported destination type in play.
- [`testbed.md`](testbed.md) — how the reference setup is simulated on
  one development machine with real commands and no containers.

## How to use this folder

Before designing a feature or changing user-facing behaviour, read the
principles and place the change in the reference setup: which machine's
seat does it improve, and what does it look like from the others?
A change that conflicts with these documents needs the document amended
in the same PR — deliberately and visibly — not silently overridden.
The documents are living: when reality proves one wrong, fix the
document; never quietly diverge from it.

## Current focus

The feature set is considered adequate; the present effort is user
experience. The method: walk the reference setup through its whole
lifecycle on the testbed — bootstrap, steady state, the scary moments,
recovery — with real commands, and log every point of friction. Fixes
are prioritized from that log, not from intuition.

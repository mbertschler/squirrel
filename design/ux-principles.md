# UX principles

## Who this is for

A prosumer who is adequately technical: knows what SSH and S3 storage
are, may run a NAS for themselves or their family, is comfortable
editing a TOML file and reading a terminal. Several of their machines
have no graphical environment at all (NAS, home server, HTPC), so the
terminal — CLI and TUI — is the primary surface. The desktop app is a
nice-to-have layered on later; nothing may *require* it.

## 1. Set up once, then trust

Squirrel is installed and configured once per machine. From then on the
agent owns the loop: indexing, syncing, verification, snapshots, and
durability exchange happen on their configured cadences without anyone
typing anything. A correctly configured household should be able to go
months without running a single squirrel command — and lose nothing.

The corollary: **any routine action that must be typed by hand is a
design bug.** Known gaps against this principle (a living list — remove
entries as they close, add new ones as they're found):

- `squirrel verify` (offsite fingerprint re-check) has no agent cadence;
  it only runs when typed. Bitrot checking that depends on someone
  remembering it is not trustworthy.
- Peer durability pulls run automatically only after a successful node
  sync; a node that stops syncing (nothing changed) also stops
  refreshing the evidence its offload gate depends on.
- `offload` is manual-only. That is partly deliberate — it is the one
  command that deletes user data — but the *decision support* should be
  automatic: the operator should see "N GB offloadable now" without
  computing it themselves.

## 2. The CLI is for change and for questions — never for operations

Every squirrel command belongs to one of two families:

- **Change**: edit config, `sync --init`, `offload`, `restore`,
  `--allow-rewind`. Deliberate, human-driven, often irreversible —
  these *should* be explicit typed acts, and should feel weighty
  (confirmations, dry-runs, loud reporting).
- **Introspection**: `tui`, `runs`, `query`, `volumes`, `hooks`,
  `verify` (read-only against the remote), `db schema`. Safe at any
  time, instant, and never mutate anything.

A command that is neither — a routine operation the agent should have
run — indicates a missing cadence, not a missing habit.

## 3. The TUI must answer "am I safe?" in one glance

The TUI is the trust surface. Its first screen has to answer, per
volume, without scrolling or drilling: when was this last indexed, is
every configured target caught up, when was durability evidence last
*verified* (not just touched), is anything drifting, did any hook fail.
Green means "you may close the laptop." Anything not green says what to
do about it.

Open problem: the TUI reads one node's local index, so the NAS's TUI
knows nothing about the laptop's state. The household has five squirrel
databases; the human has one question. Some form of fleet view —
plausibly on the hub node, fed by the peer metadata that already flows
— is needed for the principle to hold at household scale.

## 4. Scary moments are first-class UX

Trust is won in the failure paths, not the happy path. A verify
mismatch, a destination gone dark, a peer-sync conflict, an offload
gate refusal, a missing `.squirrel-volume` marker — each must be loud,
specific, and actionable ("preserved at X, do Y to inspect") — and
must never cascade into data loss on its own. Silent degradation is
worse than failure: a cadence that stops firing, evidence going stale,
a hook that has been failing for a month must all surface in the TUI
without being asked.

## 5. Automatic never means invisible

Everything the agent does on its own leaves a run row; the audit trail
is the contract that makes "trust" rational rather than hopeful.
Automation moves work from human hands to the agent, never from the
record to nowhere. The flip side of set-up-once: squirrel never
escalates on its own — the agent never passes `--init`, never offloads,
never rewinds a watermark. The irreversible acts remain human.

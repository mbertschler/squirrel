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
design bug.** Every known violation is catalogued with evidence and a
severity in [`friction-log.md`](friction-log.md) — the log is the
single home for gaps; this document states only the principles they
are measured against. (The first walk found five — F17, F21, F32, F33,
F9 — all since closed. F9, the last of them, was a config edit needing
a manual agent restart: the agent now reloads its volumes,
destinations, nodes, backups, and verify cadence in place, and where
an edit touches the shape of the process itself — its listener, its
credentials, its scan loop — it latches a standing state naming those
specific keys rather than the whole file. A restart is no longer a
routine step, only the residue of one narrow class of edit, and the
agent says precisely when that class is what you typed.)

## 2. The CLI is for change and for questions — never for operations

Every squirrel command belongs to one of two families:

- **Change**: edit config, `sync --init`, `offload`, `restore`,
  `--allow-rewind`, `destination reset`, `db restore`, `agent cert`,
  `node pair`. Deliberate, human-driven, often irreversible — these
  *should* be explicit typed acts, and should feel weighty
  (confirmations, dry-runs, loud reporting).
- **Introspection**: `tui`, `status`, `runs`, `query`, `volumes`,
  `hooks`, `conflicts`, `config check`, `peer-sync history`,
  `db schema`, `db check`. Safe at any time, instant, and never mutate
  anything.

A command that is neither — a routine operation the agent should have
run — indicates a missing cadence, not a missing habit.

One shape inside the change family is worth naming, because failure
paths keep producing it: the **acknowledgement** (`conflicts resolve`,
`verify ack`) exists only to clear a latch the agent raised
(principle 4). It changes almost nothing and destroys nothing, yet it
must stay typed — the latch is there precisely because squirrel will
not decide for you.

`verify` sits deliberately across the line: read-only against the
remote, but it writes what it learns locally — recording fingerprints,
upgrading a durability vector to content-verified once a pass certifies
every underlying object, and latching an alarm on a mismatch. It is a
question whose answer squirrel keeps.

## 3. The TUI must answer "am I safe?" in one glance

The TUI is the trust surface. Its first screen has to answer, per
volume, without scrolling or drilling: when was this last indexed, is
every configured target caught up, when was durability evidence last
*verified* (not just touched), is anything drifting, did any hook fail.
Green means "you may close the laptop." Anything not green says what to
do about it.

This now holds. The coverage grid answers per (volume × destination)
rather than per volume, so a target that has been failing for a week
can no longer hide behind a fresh ✓ earned by another one, and the
durability panel carries vector coverage, verify method, and evidence
age — the same answers `squirrel status` gives on a headless box.

It holds at household scale too: the fleet block answers, for a volume,
where else it lives — the other machines as well as the destinations —
how many files have not reached each place, whether any of them holds
content this machine has never seen, and when each was last verified.
The household has one squirrel database per machine; the human has one
question, and can ask it from whichever machine they happen to be
sitting at. One asymmetry survives: the *behind* direction is counted
from watermarks this machine already holds, while the *ahead* direction
can only be pointed at, not counted — a watermark is not an inventory,
and counting needs the folder Merkle work.

The standing requirement that comes with it: a fleet answer is only as
current as the last exchange with that node, so every row carries an
"as of" and a dark peer reads as *unknown*, never as fine. A trust
surface that quietly presents stale hearsay as fact would violate this
principle more thoroughly than having no fleet view at all.

## 4. Scary moments are first-class UX

Trust is won in the failure paths, not the happy path. A verify
mismatch, a destination gone dark, a peer-sync conflict, an offload
gate refusal, a missing `.squirrel-volume` marker — each must be loud,
specific, and actionable ("preserved at X, do Y to inspect") — and
must never cascade into data loss on its own. Silent degradation is
worse than failure: a cadence that stops firing, evidence going stale,
a hook that has been failing for a month, an agent still running the
config its operator edited last week must all surface in the TUI
without being asked.

Walking these paths produced a repeatable shape: **latch, then require
an acknowledgement.** An abnormal state raises a standing latch that
outlives the run which found it, shows on every surface until it is
dealt with, and clears only on an explicit human act (principle 2's
acknowledgements). An alarm that lives only in a scrolled-away runs row
is not an alarm. A new failure path should reach for this shape rather
than inventing a quieter one.

## 5. Automatic never means invisible

Everything the agent does on its own leaves a run row; the audit trail
is the contract that makes "trust" rational rather than hopeful.
Automation moves work from human hands to the agent, never from the
record to nowhere. The flip side of set-up-once: squirrel never
escalates on its own — the agent never passes `--init`, never offloads,
never rewinds a watermark. The irreversible acts remain human.

The human's own interventions are held to the same standard: clearing a
latch writes a `runs_audit` entry naming the operator, so "someone
decided this was fine" stays as recoverable a fact as the failure that
raised it.

# Positioning brief — proposed

*Draft for review. Not adopted yet: nothing in `docs/` or `README.md` has
been changed to match. If accepted, this becomes the source for the
landing page and README rewrite.*

## The problem

The front page sells the **storage engine**. The product is now an
**unattended household system**.

Today's four cards are "Content, not paths", "Verified uploads",
"Append-only by design", "Many backends, one config". All true, all
still worth saying — and all describing the layer *underneath* the
thing someone actually adopts. The word *agent* does not appear on the
landing page. Neither do cadences, the household, the "am I safe?"
answer, or offload. A reader comes away thinking squirrel is a sync
tool they will have to remember to run, which is exactly what
[`ux-principles.md`](ux-principles.md) §2 says it must never be.

The engine framing was right when the engine was the whole product.
The UX phase built a second product on top of it and nobody re-pitched.

## The pitch

> ### Set up once, then trust.
> Squirrel keeps your household's files safe across every machine you
> own — and proves it, so you never have to wonder.

Configure each machine once. From then on squirrel's agent owns the
loop: indexing, syncing, verification, snapshots, and durability
exchange all happen on their own cadences. A correctly configured
household goes months without anyone typing a command — and loses
nothing.

*Alternates, if the imperative reads too soft:* "Backup that runs
itself, and can prove it worked." / "The backup system you stop
thinking about."

## The four pillars

Replacing the current cards. Order matters — the product first, the
guarantee it rests on last.

**1 · Set up once, then trust.**
Install and configure a machine once. The agent handles indexing,
syncing, verification and evidence exchange on their cadences.
Every command squirrel has is either a deliberate change or a
question you asked — never a chore it should have done for you.

**2 · One glance, one answer.**
`squirrel status` and the TUI answer "am I safe?" per volume and per
destination: caught up, durable, when the evidence was last actually
*verified*. Green means you can close the laptop. Anything else says
what to do about it — and says it until you deal with it, because
alarms latch instead of scrolling away.

**3 · Proof, not hope.**
Squirrel's one destructive act — dropping local bytes once they're
safe elsewhere — happens only against proof: the content must be
verified present on every target *you* required, by fingerprint, not
by "the upload didn't error". A machine with a small disk can lean on
the household without you auditing it by hand.

**4 · Nothing is ever lost.**
Content is addressed by BLAKE3, so a hash ever observed stays
retrievable. Destinations are append-only — an overwrite preserves the
prior bytes. Both sides of a sync conflict survive. The audit trail is
never auto-pruned. This is the floor everything else stands on.

## What changes, concretely

| Where | Now | Proposed |
|---|---|---|
| Hero tagline | "Backup tool for your own NAS + cloud offsite storage. Indexes by BLAKE3 content hash…" | "Set up once, then trust." + the household line |
| Four cards | Content / Verified / Append-only / Backends | The four pillars above |
| Missing entirely | — | A "household, not a machine" section: hub NAS, roaming laptop, receive-only HTPC |
| README opening | Same engine framing | Mirror the new hero, keep the engine detail below the fold |

The engine claims don't disappear — they move down. Someone evaluating
squirrel against restic or kopia still needs them, just not as the
opening move.

## Why this framing

Restic, kopia and borg are **backup programs you schedule**. Squirrel
is a **household that runs itself and can prove what's safe**. That is
the difference worth leading with, and it is the one a prosumer
choosing between them will care about. Content-addressing is how
squirrel earns the claim — it is the evidence, not the pitch.

One thing to be honest about: the reference household is five machines,
and most readers will arrive with two. The copy should scale down
gracefully rather than imply a NAS is required.

## Open questions

1. **Does offload get a card?** It's the most distinctive thing squirrel
   does and the hardest to explain in forty words. Pillar 3 is my
   attempt; it may deserve its own page linked from the card instead.
2. **How loudly do we say "household"?** It's the honest framing and it
   scales down, but it may read as "you need a NAS" to a laptop-plus-S3
   user.
3. **Is "trust" overclaiming before #129?** Offload has never run
   against a real cold archive. Pillar 3 describes shipped, tested
   behaviour, but the shakedown is still open — worth deciding whether
   the pitch waits on it.
4. **Fleet view is still missing.** Pillar 2 is true per machine; the
   household has five databases and the human has one question. Say
   "per machine" plainly, or wait for a fleet view before making the
   glance claim household-wide?

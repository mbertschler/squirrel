# Positioning brief — adopted

*Adopted and shipped (#189). The landing page
(`docs/src/content/docs/index.mdx`), the site-wide meta description
(`docs/astro.config.mjs`) and `README.md` carry this copy; the "What changes,
concretely" table below records what they said before. This file stays the
source for that copy — a change to the front-page framing amends this brief in
the same PR.*

## The problem

*The state of the front page before #189, kept as the rationale.*

The front page sold the **storage engine**. The product is now an
**unattended system that spans every device you own**.

The four cards it led with were "Content, not paths", "Verified uploads",
"Append-only by design", "Many backends, one config". All true, all
still worth saying — and all describing the layer *underneath* the
thing someone actually adopts. The word *agent* does not appear on the
landing page. Neither do cadences, the multi-device story, the "am I
safe?" answer, or offload. A reader comes away thinking squirrel is a sync
tool they will have to remember to run, which is exactly what
[`ux-principles.md`](ux-principles.md) §2 says it must never be.

The engine framing was right when the engine was the whole product.
The UX phase built a second product on top of it and nobody re-pitched.

## The pitch

> ### Set up once, then trust.
> Squirrel keeps your files safe — on one computer, or across every
> device you own — and proves it, so you never have to wonder.

Configure each machine once. From then on squirrel's agent owns the
loop: indexing, syncing, verification, snapshots, and durability
exchange all happen on their own cadences. A correct setup goes months
without anyone typing a command — and loses nothing.

*Alternates, if the imperative reads too soft:* "Backup that runs
itself, and can prove it worked." / "The backup system you stop
thinking about."

## The five pillars

Replacing the current cards. Order matters — the product first, the
guarantee it rests on last.

**1 · Set up once, then trust.**
Install and configure a machine once. The agent handles indexing,
syncing, verification and evidence exchange on their cadences.
Every command squirrel has is either a deliberate change or a
question you asked — never a chore it should have done for you.

**2 · One glance, one answer.**
`squirrel status` and the TUI answer "am I safe?" for a volume
everywhere it lives — every destination and every other machine:
whether each is caught up, how far behind it is, whether it is durable,
and when its evidence was last actually *verified*. Green means you can
close the laptop. Anything else says what to do about it — and says it
until you deal with it, because alarms latch instead of scrolling away.

**3 · Verified, not assumed.**
Every upload is checked against what actually landed, and offsite
copies are re-checked on their own schedule long after the transfer
succeeded. "The upload didn't error" is not evidence; a fingerprint
that still matches months later is. When a check fails, squirrel says
so and keeps saying so.

**4 · Deleting locally never deletes the copy.**
Squirrel never propagates a delete. Removing a file locally leaves every
durable copy exactly where it is, and a destination keeps content the
source no longer has. That is what makes freeing up space safe:
`squirrel offload` removes local bytes *on purpose*, and only once the
content is verified on every destination you required. Old photos
leave your laptop; they don't leave your archive.

**5 · Nothing is ever lost.**
Content is addressed by BLAKE3, so a hash ever observed stays
retrievable. Destinations are append-only — an overwrite preserves the
prior bytes. Both sides of a sync conflict survive. The audit trail is
never auto-pruned. This is the floor everything else stands on.

## What changes, concretely

| Where | Now | Proposed |
|---|---|---|
| Hero tagline | "Backup tool for your own NAS + cloud offsite storage. Indexes by BLAKE3 content hash…" | "Set up once, then trust." + the hero line above |
| Four cards | Content / Verified / Append-only / Backends | The five pillars above — offload gets one of its own |
| Missing entirely | — | A **scaling** section (copy below) |
| README opening | Same engine framing | Mirror the new hero, keep the engine detail below the fold |

The engine claims don't disappear — they move down. Someone evaluating
squirrel against restic or kopia still needs them, just not as the
opening move.

### The scaling section

The page never says what size of setup squirrel is for, which leaves a
reader with one computer guessing whether they need a NAS. Say the range
outright, smallest first:

> **From one computer to every device you own.**
> Squirrel works the same at any size: one machine backing up to one
> destination, or many machines sharing several — local drives, network
> storage, cloud buckets, cold archive. Same config file, same commands.
> Add machines and destinations as you get them.

Lead with the smallest case; the many-device setup is the top of the
range, not the entry price. Name categories of destination rather than a
particular topology — the point is that the shape is yours to choose, and
a concrete example here would read as the required one.

## Standing rules for shipped copy

**Avoid "house" and "household."** Plenty of readers don't live in a
house, and "household" is insider vocabulary borrowed from
`reference-setup.md` that tells a newcomer nothing. Say *devices* — it
covers the apartment, the desk, and the rack equally.

**Name no other tool, and never invite the comparison.** Shipped copy
says what squirrel does on its own terms. No "unlike most backup tools",
no "restic, kopia or borg" — a comparison makes the reader evaluate
squirrel inside someone else's category, and the interesting half of
squirrel is the part that category has no slot for. State the behaviour
and let it be obvious that it is more than a scheduled copy.

**Don't lead with "backup" as the category.** Backup is a large part of
what squirrel does and the word is fine deeper in a sentence, but it is
not the opening claim: it sets the reader up to expect snapshots that
expire and a source that is authoritative forever, and squirrel is
neither. The hero says *keeps your files safe … and proves it* precisely
because it commits to no category. See also the archival note below.

**Squirrel is not purely a backup tool, and not purely an archive.**
The local tree is authoritative *until you offload*; from then on the
destinations are, permanently. Nothing expires — there is no content
retention or pruning anywhere in the tree by design, only explicit
operator-driven `db backup --keep` for index artifacts. Offload is the
verb that moves content across that line, and it is gated on proof. Copy
that implies expiry, or implies the destination is only ever a mirror of
the source, is wrong on both counts.

## Why this framing

*Internal rationale. The comparisons in this section are for us; they do
not appear in shipped copy — see the standing rules above.*

Restic, kopia and borg are **backup programs you schedule**. Squirrel
**runs itself and can prove what's safe**. That is the difference worth
leading with, and it is the one a prosumer choosing between them will
care about. Content-addressing is how squirrel earns the claim — it is
the evidence, not the pitch.

The second axis is deletion. A tool that mirrors deletions makes
"clear some space locally" a dangerous sentence: the copy you were
relying on goes with it, often silently. Squirrel splits the two —
local bytes and the durable copy are separate decisions — so freeing
space is a normal thing to do rather than something to be careful
about. Readers who have been burned by this will recognise it
immediately; readers who haven't will not know to look for it, which
is exactly why it belongs on a card instead of in the FAQ.

The claim has to hold at both ends of the range. It does: one machine
with one destination gets the same agent, the same "am I safe?" answer,
and the same proof-gated offload as a dozen machines do. The copy should
never make the small setup feel like the degenerate case.

## Decisions

1. ~~**Does offload get a card?**~~ **Yes — pillar 4.** It leads with
   the behaviour, not the mechanism: squirrel never propagates a delete,
   and a destination keeps content the source no longer has. That is the
   feature, and it is what makes offload safe rather than frightening.
   The proof-gating is the second sentence, not the first.

   *Amended in #189 (maintainer's call):* this originally led with the
   comparison — "most backup tools mirror a deletion, and squirrel does
   not". Shipped copy no longer names or implies other tools at all, so
   the card states squirrel's own behaviour and lets the reader draw the
   contrast. See the standing rules above; the comparison survives only
   as internal rationale under "Why this framing".
2. ~~**How loudly do we say "household"?**~~ **Not at all — say the
   range, in devices.** Smallest first: one machine and one destination,
   scaling up to every computer and drive you own. "Household" is
   insider vocabulary that tells a first-time reader nothing, and
   "house" assumes a kind of home not everyone has. The hero and the
   scaling section say the range in categories rather than a worked
   example.
3. ~~**Is "trust" overclaiming before #129?**~~ **No — write the copy
   as if the shakedown has passed.** If it turns something up, that is
   fixed inside #129 rather than by a second pass over the docs.
4. ~~**Fleet view is still missing.**~~ **Tracked as #187; the copy
   assumes it.** Pillar 2 makes the glance claim across every place a
   volume lives rather than qualifying it per machine — the qualified
   version was the weaker half of the pitch, and a reader with more
   than one computer would have asked the question anyway. Same rule as
   #129: fix anything the implementation turns up inside that ticket.

Nothing is open. The copy shipped in #189, after #187 landed the fleet view
pillar 2 depends on — the preferred sequencing in that issue, so the page makes
the glance claim about behaviour that exists.

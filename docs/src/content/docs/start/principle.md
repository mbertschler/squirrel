---
title: The core principle
description: Squirrel indexes content by BLAKE3 hash, not paths. A hash ever observed stays retrievable — paths are observations of content, and history is never lost.
---

Squirrel indexes **content** (BLAKE3 hashes), not paths. A hash ever observed
must stay retrievable. **Paths are observations of content; content is the
entity.**

When content at a path changes, the prior row is flipped to `superseded` and a
new row is inserted; the old hash is never rewritten in place.
`squirrel query <hash>` will still find a hash whose path now holds different
content.

## Content is the entity

The schema makes this literal:

- `contents` is the **append-only content entity** — one row per BLAKE3, with
  size and origin. Contents rows are never updated, and the `contents.blake3`
  column is `UNIQUE`, so the id↔hash binding is immutable by construction.
- `files` rows are **path↔content observations** referencing a `contents` row.

`Upsert` never rewrites a row's `content_id` in place: when content at a path
changes it marks the prior row `superseded` and inserts a new one, keeping at
most one live (non-`superseded`) row per path — enforced by the
`uniq_files_live_per_path` partial unique index.

:::note[The principle extends to sync]
Overwrites at the destination are preserved under
`<dest>/<volume>/.squirrel-history/run-<id>/`, and `squirrel sync` never deletes
files at the destination even when the local copy is gone.
:::

## History is never lost

The `runs` table follows the same no-loss spirit by policy: squirrel never
auto-prunes runs — they are an audit trail, and any retention is explicit and
operator-driven.

Any feature that touches stored state (sync, prune, dedup, GC) preserves both
guarantees: **no deleting or overwriting history without an explicit, opt-in
retention policy.**

## Freeing space is not a deletion

Squirrel never propagates a delete, in either direction: removing a file locally
leaves every durable copy where it is, and sync never deletes at the
destination. That separation is what makes
[`squirrel offload`](/squirrel/guides/offloading/) safe rather than frightening
— it reclaims local bytes *on purpose*, and only after proving the content is
durable on every target the volume's offload policy requires.

## Learn more

- [Content, not paths](/squirrel/concepts/content-model/) — the schema and
  migration model in depth.
- [Runs & the audit trail](/squirrel/concepts/runs/) — how runs are recorded and
  why they are never auto-pruned.

---
title: Recovery & disaster runbooks
description: Reset a wrecked destination, rebuild a dead machine from its hub, and the manual disaster-recovery paths that already work — mirror restore, index-snapshot recovery, and packed/content-addressed recovery.
---

Recovery in squirrel is deliberately a set of **explicit, human-driven acts**
— never something the agent does on a cadence. Each one preserves the audit
trail: the runs table is never pruned, and no history is overwritten without
an opt-in operator command. This page collects the recovery paths.

## Resetting a wrecked destination

`squirrel destination reset <name>` forgets everything the index records about
a destination's remote state — its per-content and per-pack upload ledgers, its
live durability vector, and its push-freshness maxima — so the next sync treats
the destination as fresh and re-uploads.

Use it when a destination's recorded state no longer matches reality and a sync
refuses to proceed. The classic case: after wiping a remote, or pointing an
existing destination name at a fresh `root`, the
[content-addressed](/squirrel/layouts/content-addressed/) or
[packed](/squirrel/layouts/packed/) layout guard refuses — its recorded history
under that destination name expects a manifest segment or placement map that is
no longer there. Before this verb, the only escape was hand-editing SQLite or
renaming the destination across every machine's config.

```sh
squirrel destination reset s3archive --dry-run   # preview what would clear
squirrel destination reset s3archive --yes        # actually clear it
```

It is a **change** command, so it is weighty by design: it prints what it will
clear, and refuses without `--yes` (or a `--dry-run` preview).

### What is and isn't cleared

Cleared (derived state only):

- `remote_objects` and `remote_packs` — the upload ledgers.
- the destination's durability vector and push-freshness rows.

Preserved (the audit trail):

- the **runs** table — every past sync, verify, and audit stays; the reset is
  itself recorded as a new audit run.
- the append-only **durability advance log** — the record of what was ever
  asserted durable survives, exactly as [revoking a peer](/squirrel/guides/peer-sync/)
  leaves its history intact.
- the **content and files** rows — squirrel never loses track of content.

The **remote bytes are not touched** — reset only clears squirrel's *record*.
If you want the destination genuinely empty, wipe or repoint the remote root
separately. Once the configured root is empty on the remote, the layout guard
recognises the fresh start on its own and the next sync re-uploads from scratch.

:::note[Stop the agent first]
A running agent may kick a sync on its cadence mid-reset. Nothing is lost — a
content-addressed re-upload is idempotent — but pausing the agent keeps the
recovery legible.
:::

## Rebuilding a machine from its hub

When an edge machine dies (the most likely household disaster), rebuild it from
the hub with a **reverse peer push** — the same peer-sync mechanism the hub uses
to feed a receive-only node every day. There is no separate restore verb for
this: `squirrel restore` pulls from bucket destinations, not from peer nodes,
and the machinery to push a volume to a node already exists.

The story, using the [reference setup](/squirrel/reference/configuration/)
(rebuilding `laptop` from `nas`):

1. **Install and re-pair the replacement.** Install squirrel, generate its
   agent cert, and re-establish the peer relationship with the hub. Use the
   pairing helper so the token matrix is correct by construction:

   ```sh
   squirrel agent cert                 # on the replacement: new cert + pin
   squirrel node pair nas              # emits matching config halves for both
   ```

   Paste each half into the named machine's config and fill any placeholders
   (see [Peer sync](/squirrel/guides/peer-sync/)). The replacement runs an agent
   that **listens** so the hub can dial it — the reverse of its normal
   initiate-only role.

2. **Point the hub at the replacement, temporarily.** On the hub, add the
   replacement as a `[nodes.laptop]` entry and list it in the `sync_to` of the
   volumes to restore. Then drive the push:

   ```sh
   squirrel sync photos --to laptop
   squirrel sync docs   --to laptop
   ```

   The hub enumerates its master copy and pushes; the replacement receives and
   re-hashes every path (`peer-blake3`), so the restore is content-verified end
   to end. Both sides record correlated runs — the recovery is in the audit
   trail like any other sync.

3. **Return to steady state.** Once the replacement holds the volumes, remove
   the temporary hub-side `sync_to`/`[nodes.laptop]` push config, and resume the
   normal edge → hub direction (`laptop` initiating to `nas`).

## Manual disaster recovery that already works

These paths need no new tooling and are the right answer for whole-repository or
hub loss. They stay first-class.

### Mirror restore

For a [mirror](/squirrel/layouts/mirror/) destination (including an encrypted
one), [`squirrel restore`](/squirrel/guides/restore/) pulls the volume back
byte-for-byte, decrypting on the way down and verifying BLAKE3 where the
destination exposes hashes.

### Recovering the index too

squirrel rides an [index snapshot](/squirrel/configuration/index-snapshots/)
along to destination buckets under `.squirrel-index/`. When the hub itself dies,
a restore-from-cloud yields the data *and* the index that explains it — fetch
the ride-along snapshot and swap it in as the live index with
[`squirrel db restore`](/squirrel/reference/cli/#squirrel-db). `runs` and `query`
answer immediately against the recovered catalog.

### Packed & content-addressed recovery

The [content-addressed](/squirrel/layouts/content-addressed/) and
[packed](/squirrel/layouts/packed/) formats are deliberately simple enough to
[recover without squirrel](/squirrel/reference/formats/#disaster-recovery-without-squirrel)
— the manifest and pack formats are documented, so the bytes are recoverable
with standard tools even if squirrel is unavailable.

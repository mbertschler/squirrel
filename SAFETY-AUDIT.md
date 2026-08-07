# Squirrel data-safety audit

This document is a blueprint for hardening squirrel against silent data
loss — both of file bytes (local volume, remote destinations) and of
index metadata (file rows, run history, watermarks, hashes). Each
finding is sized for one GitHub issue. Severities reflect _potential_
impact assuming the codepath fires; likelihood is annotated separately
because some are theoretical until the surrounding feature lands.

Read this in tandem with the project principle at the top of
`CLAUDE.md`: paths are observations of content; content is the entity;
historical observations are append-only by design.

**This document is a record, not a backlog.** Most of it has shipped.
Every finding carries a `**Status:**` note verified against the tree, and
[the standing summary](#standing-summary-what-is-actually-left) below
states what is genuinely left. A second audit round ran later against the
offload-v1 surface; it is recorded separately in
[round two](#round-two--the-offload-v1-audit-103118) rather than folded
into the numbering here.

---

## Standing summary: what is actually left

Every one of the 22 round-one findings was re-checked against the tree in
August 2026 — behaviour spot-checked in the code, not inferred from issue
titles. The result:

- **18 discharged** — 17 by a shipped fix, one (M4) dissolved by a schema
  change that removed the mutable field the finding was about.
- **2 partial** — C3 and M8, detailed below.
- **2 open** — M7 and L3, detailed below.

Nothing in the Critical or High tiers is open.

### Still open

| Finding | Why it is still open |
|---|---|
| **M7** — orphan-volume warning is only a warning | `warnOrphanVolumes` (`cmd/squirrel/root.go`) still only prints a stderr advisory; nothing refuses to run and there is no acknowledgement to clear. The latch-then-acknowledge shape this needs now exists twice over (`destination_alarms` + `verify ack`; `contested_paths` + `conflicts resolve`), so closing it is wiring, not design. See [principle 4](design/ux-principles.md#4-scary-moments-are-first-class-ux). |
| **L3** — no rclone-version gate on restore-from-node | Still prospective, as written: `restoreFromNode` does not exist, so there is nothing to gate. `runRestore` now calls `EnsureMinVersion` unconditionally before any transfer, so a restore-from-node routed through that command would inherit the gate for free; a separate code path would not. Keep the placeholder. |

### Partial

| Finding | Shipped | Not shipped |
|---|---|---|
| **C3** — no backup of the SQLite index | Layer 1 (`squirrel db backup` / `db check` / `db restore` via `VACUUM INTO`, [#73](https://github.com/mbertschler/squirrel/pull/73)) and layer 2 (index snapshot on every successful sync, riding along to `<dest>/<volume>/.squirrel-index/`, [#91](https://github.com/mbertschler/squirrel/pull/91)) | Layer 3, the documented Litestream wiring. No mention of Litestream survives anywhere in `README.md` or `docs/`. This is the only piece of C3 still outstanding, and it is documentation plus a tested example config, not code. |
| **M8** — runs table has no retention policy flag | The policy itself, stated in `AGENTS.md`, `README.md`, and `docs/src/content/docs/concepts/runs.md`: runs are never auto-pruned, retention is explicit and operator-driven | The `squirrel runs prune --list-candidates` placeholder command. Arguably it should stay unshipped: [principle 2](design/ux-principles.md#2-the-cli-is-for-change-and-for-questions--never-for-operations) says every command is a change or a question, and a no-op placeholder is neither. The policy was the load-bearing half and it landed. Worth closing deliberately rather than leaving it to read as pending. |

---

## Severity legend

- **Critical** — direct path to silent loss of user file bytes or index
  rows with no recovery path other than restoring from outside squirrel.
- **High** — silent loss of audit trail, content history, or
  forensic-quality metadata; recoverable only by re-walking or
  re-syncing, with information loss in between.
- **Medium** — defence-in-depth gaps; today's code is mostly OK but the
  invariant relies on assumptions that aren't enforced.
- **Low** — paper cuts, observability gaps, documentation drift.

The "Likelihood" tag describes how realistic the trigger is _today_
given current code paths and typical usage; a high-severity / low-
likelihood finding still warrants fixing because the cost of being
wrong is total.

---

## Audit checklist (proposed issue list)

Each section ends with a one-line "Issue:" string that names the GitHub
issue we should open for that item, followed by the state of that item
today: ✅ resolved, ◐ partial, ○ open.

(The original list skipped numbers 12 and 13 — a numbering slip, not two
missing findings. There have always been exactly 22.)

### Critical

1. ✅ [#C1](#c1-restore-overwrites-the-local-volume-without-history) —
   `restore` overwrites the local volume in place with no
   `--backup-dir`, no confirmation, no marker check.
   → tracked in [#61](https://github.com/mbertschler/squirrel/issues/61),
   fixed in [#72](https://github.com/mbertschler/squirrel/pull/72)
2. ✅ [#C2](#c2-copy-from-existing-pre-stage-can-clobber-out-of-band-files)
   — receiver's CopyFromExisting pre-stage uses `os.Rename` over the
   destination path; an out-of-band file there is overwritten without
   being moved to history.
   → tracked in [#62](https://github.com/mbertschler/squirrel/issues/62),
   fixed in [#70](https://github.com/mbertschler/squirrel/pull/70)
3. ◐ [#C3](#c3-no-backup-of-the-sqlite-index) — there is no built-in
   backup, snapshot, or Litestream story for `~/.squirrel/index.db`. A
   single disk failure loses the entire index.
   → tracked in [#65](https://github.com/mbertschler/squirrel/issues/65) (combined with H5 + M3);
   layer 1 in [#73](https://github.com/mbertschler/squirrel/pull/73),
   layer 2 in [#91](https://github.com/mbertschler/squirrel/pull/91);
   **layer 3 (Litestream docs) not written**
4. ✅ [#C4](#c4-sync-runid-fallback-collides-history-into-one-bucket) —
   `backupDirURI` falls back to `run-dry-run/` when `runID == 0` outside
   dry-run mode; any future bug that calls into that branch silently
   merges all overwritten history into one directory.
   → fixed in [#67](https://github.com/mbertschler/squirrel/pull/67)

### High

5. ✅ [#H1](#h1-cross-process-index-run-race-corrupts-the-live-set) —
   `index` has no atomic begin-if-clear gate; two concurrent indexers
   (CLI + agent scheduler) can both finish, and the loser's MarkMissing
   will flip the winner's freshly-touched rows to `missing`.
   → tracked in [#63](https://github.com/mbertschler/squirrel/issues/63),
   fixed in [#69](https://github.com/mbertschler/squirrel/pull/69)
6. ✅ [#H2](#h2-finishrun-blindly-overwrites-terminal-state) — `FinishRun`
   accepts a transition from any status, so a double-finish bug or a
   buggy retry overwrites the original terminal status, error message,
   and timestamp.
   → tracked in [#78](https://github.com/mbertschler/squirrel/issues/78),
   fixed in [#81](https://github.com/mbertschler/squirrel/pull/81)
7. ✅ [#H3](#h3-runs-fail-erases-the-original-end-state-without-an-audit-line)
   — the manual recovery command (`runs fail`) flips a stuck `running`
   row to `failed` with no record that this was an operator action
   rather than a real failure, and clobbers any partial `file_count`.
   → tracked in [#78](https://github.com/mbertschler/squirrel/issues/78),
   fixed in [#81](https://github.com/mbertschler/squirrel/pull/81)
8. ✅ [#H4](#h4-toctou-between-classify-and-pre-stage-can-misattribute-bytes)
   — receiver classifies on the index, then pre-moves on disk; if the
   on-disk content drifted out-of-band between those steps, the
   `.squirrel-history/run-N/` payload no longer matches the recorded
   blake3.
   → tracked in [#76](https://github.com/mbertschler/squirrel/issues/76),
   fixed in [#80](https://github.com/mbertschler/squirrel/pull/80)
9. ✅ [#H5](#h5-no-pre-migration-snapshot-of-the-index) — schema
    migrations run in a transaction (good) but there is no on-disk
    snapshot taken before the migration starts. A corruption or
    in-flight crash leaves no point-in-time backup to fall back to.
    → tracked in [#65](https://github.com/mbertschler/squirrel/issues/65) (combined with C3 + M3),
    fixed in [#73](https://github.com/mbertschler/squirrel/pull/73)
10. ✅ [#H6](#h6-the-watermark-and-correlated-run-id-are-overwrite-in-place)
    — `UpsertPeerSyncState` and `SetCorrelatedRunID` rewrite values
    with no audit row. If a bug or hostile peer pushes a bad
    watermark, the prior watermark is lost.
    → tracked in [#77](https://github.com/mbertschler/squirrel/issues/77),
    fixed in [#87](https://github.com/mbertschler/squirrel/pull/87)
11. ✅ [#H7](#h7-destination-root-and-restore-target-are-not-marker-guarded)
    — sync writes into `<dest.root>/<volume>/` and restore writes into
    `vol.Path`; nothing validates that those locations are the squirrel-
    owned tree we think they are.
    → resolved by [#64](https://github.com/mbertschler/squirrel/issues/64)
    (marker mechanism, local destinations, and restore; shipped in
    [#71](https://github.com/mbertschler/squirrel/pull/71)) and
    [#150](https://github.com/mbertschler/squirrel/issues/150) (remote
    rclone destinations across the mirror, content-addressed, and packed
    layouts; shipped in
    [#170](https://github.com/mbertschler/squirrel/pull/170))

### Medium

12. ✅ [#M1](#m1-shallow-mode-on-bucket-sync-can-skip-a-divergent-destination)
    — `--shallow` drops `--checksum --hash blake3`; a destination that
    drifted out-of-band but kept the same (size, mtime) silently stays
    divergent.
    → tracked in [#79](https://github.com/mbertschler/squirrel/issues/79),
    fixed in [#82](https://github.com/mbertschler/squirrel/pull/82)
13. ✅ [#M2](#m2-folder-merkle-hashes-have-no-self-check) — there is no
    audit subcommand that re-derives every folder's deep_blake3 from
    its children and compares to what's stored.
    → tracked in [#77](https://github.com/mbertschler/squirrel/issues/77),
    fixed in [#87](https://github.com/mbertschler/squirrel/pull/87)
14. ✅ [#M3](#m3-no-pragma-integrity_check-anywhere) — the sqlite file is
    only verified lazily, page-by-page, when a row is read. A silent
    corruption can sit undetected for months.
    → tracked in [#65](https://github.com/mbertschler/squirrel/issues/65) (combined with C3 + H5),
    fixed in [#73](https://github.com/mbertschler/squirrel/pull/73)
15. ✅ [#M4](#m4-source_node_id-attribution-is-overwritten-on-touch) —
    when an unchanged-content row is touched on a different sync's
    closeSession or by the indexer, `updateLiveRow` rewrites
    `source_node_id` / `source_run_id`, losing the prior attribution.
    → **dissolved** by the contents/files split in
    [#97](https://github.com/mbertschler/squirrel/pull/97): the columns
    no longer live on the mutable row
16. ✅ [#M5](#m5-rclone-config-is-the-only-place-secrets-get-resolved-then-re-resolved)
    — `WriteRcloneConfig` is rewritten on every sync. There's no
    "diff against last-written" guard, so a buggy resolver could
    silently regress a secret.
    → tracked in [#79](https://github.com/mbertschler/squirrel/issues/79),
    fixed in [#82](https://github.com/mbertschler/squirrel/pull/82)
17. ✅ [#M6](#m6-tempfile-write-in-copyfromexisting-lacks-fsync) —
    `copyFileToPath` closes and renames the tempfile without
    `f.Sync()`, so a system crash mid-pre-stage can leave a zero-byte
    file in place after rename; verify catches it but the prior bytes
    may already be elsewhere.
    → fixed in [#67](https://github.com/mbertschler/squirrel/pull/67)
18. ○ [#M7](#m7-orphan-volume-warning-is-only-a-warning) — a volume
    declared in the DB but missing from config logs once at every
    command invocation; nothing escalates it or refuses commands until
    the operator confirms.
    → **still open**; no tracking issue
19. ◐ [#M8](#m8-runs-table-has-no-retention-policy-flag) — runs grow
    forever, which is fine; but the lack of any explicit retention
    command means there's no documented stance against eventual
    pruning. We should state the policy ("never prune unless `runs
    prune` is run explicitly") and add the no-op command as a
    placeholder.
    → policy documented; **placeholder command deliberately not shipped**

### Low

20. ✅ [#L1](#l1-buildDSN-collapses-on-trailing-spaces-in-path) — `'?'`
    and `'#'` are rejected but path normalisation doesn't catch
    trailing whitespace or unicode lookalikes.
    → tracked in [#79](https://github.com/mbertschler/squirrel/issues/79),
    fixed in [#82](https://github.com/mbertschler/squirrel/pull/82)
21. ✅ [#L2](#l2-hostname-fallback-can-silently-produce-an-empty-node-name-on-edge-platforms)
    — `sanitiseNodeName` could in theory yield `""` on hostnames with
    no alnum characters; we then error, but there's no fallback to a
    UUID-based name.
    → tracked in [#79](https://github.com/mbertschler/squirrel/issues/79),
    fixed in [#82](https://github.com/mbertschler/squirrel/pull/82)
22. ○ [#L3](#l3-no-pre-flight-rclone-version-check-for-restore-from-node)
    — restoreFromNode isn't yet implemented, but when it lands we need
    the same `EnsureMinVersion` gate the bucket path has.
    → **still open, still prospective**: the feature does not exist yet

---

## Critical findings — detail

### C1: `restore` overwrites the local volume without history

**Severity:** Critical • **Likelihood:** High (this is the default
behaviour today).

**Where**

- `sync/sync.go:471-557` — `Restore` and `buildRestoreArgs`.
- `cmd/squirrel/restore.go:16-49` — default target is `vol.Path`.

**What can go wrong**

`Restore` invokes `rclone copy <dest>:<root>/<volume>/ <vol.Path>/`
with no `--backup-dir`. The comment at `sync/sync.go:529-534` is
explicit:

> Restore does not pass --backup-dir: the local target is the recovery
> surface, and the user opted in to overwrites by invoking restore.

The user "opts in" by running the command, but the command takes no
explicit "yes really overwrite" flag and the default `--to` is the
live volume path. A user who restores after editing files locally
loses the local edits irrecoverably. There is no equivalent of
`.squirrel-history/run-N/` on the local side.

This is the most likely realistic data-loss path in current squirrel:
the user types `squirrel restore pictures` after thinking the photos
might be missing, and rclone happily replaces the on-disk versions
with whatever the remote holds.

**Proposed mitigation**

- Default `restore` to refuse running against a non-empty target
  unless `--in-place` is passed.
- When `--in-place` is passed, write a local `--backup-dir` equivalent
  (`<vol.Path>/.squirrel-restore-history/run-<runID>/`) so the prior
  on-disk content is preserved in the same way destinations preserve
  overwrites.
- The reserved name must be filtered out of `index` walks and `sync`
  uploads the same way `.squirrel-history` and `.squirrel-conflicts`
  are.
- The `--to` flag stays as-is for "restore into a scratch directory"
  workflows; the safety only kicks in when `--to` is unset.

**Acceptance**

- Default `restore` against a populated volume errors out with a
  message naming the local backup-dir and `--in-place` flag.
- A test simulates "user has local edit, runs restore", asserts the
  edit is moved into the local backup-dir, asserts the restored bytes
  land on the live path.

**Status:** Resolved. [#72](https://github.com/mbertschler/squirrel/pull/72)
shipped exactly the proposed shape: `restore` refuses a populated `vol.Path`
unless `--in-place` is passed, and an in-place run moves every overwritten
file into `<vol.Path>/.squirrel-restore-history/run-<id>/` before writing
the restored bytes. The reserved name is filtered out of index walks and
sync uploads like `.squirrel-history`, and `--to` still bypasses the gate
for scratch-directory restores. Both acceptance tests exist
(`TestRestoreRefusesPopulatedVolumeWithoutInPlace`,
`TestArchiveRestoreInPlacePreservesOverwritten`); the preservation path was
later extended to the packed and content-addressed layouts by
[#169](https://github.com/mbertschler/squirrel/pull/169).

**Issue:** [#61 — restore: gate in-place overwrites behind --in-place and write a local history dir](https://github.com/mbertschler/squirrel/issues/61)

---

### C2: CopyFromExisting pre-stage can clobber out-of-band files

**Severity:** Critical • **Likelihood:** Medium (only fires when a peer
sync runs against a receiver whose volume gained a file the receiver
hadn't yet indexed).

**Where**

- `agent/sync.go:744-766` — `preStageCopyFromExisting`.
- `agent/sync.go:777-821` — `copyFileToPath` and its `os.Rename`.

**What can go wrong**

`classify` decides `CopyFromExisting` when the receiver has no
**indexed** row at the destination path but holds the requested blake3
at some other path. The pre-stage then materialises the file via
`copyFileToPath`, which finishes with `os.Rename(tmpPath, dstAbs)`.

If a file actually exists at `dstAbs` on disk but is not represented
in the index (the receiver gained the file out-of-band — a NAS web
app dropped it in, a user `scp`'d it directly, a previous sync was
aborted before MarkMissing fired), the `os.Rename` atomically
overwrites it with no history move. The bytes are gone.

The same risk exists on the disposition `Transfer` rclone path, but
there the receiver's local files would be the destination's "drift"
relative to its own index — rclone's `--checksum --hash blake3` would
catch the divergence as a transfer rather than a no-op. With
CopyFromExisting we deliberately bypass rclone, so there's no checksum
gate.

**Proposed mitigation**

- Before `os.Rename`, stat the destination. If it exists, move it to
  `.squirrel-history/run-N/` first (mirroring the supersede branch's
  `preMoveSupersedes`).
- Re-classify defensively: if the destination exists on disk, treat
  the path as a `Transfer` (or a `Conflict` if it differs from the
  initiator's blake3) rather than `CopyFromExisting`. The dedup
  optimisation is not worth a silent overwrite.

**Acceptance**

- A test seeds a file at the destination path on disk that has no
  index row, runs a sync where the path classifies as
  CopyFromExisting, asserts the prior bytes are at
  `.squirrel-history/run-N/<path>` after the sync.

**Status:** Resolved. [#70](https://github.com/mbertschler/squirrel/pull/70)
took the first mitigation: `preStageCopyFromExisting` now Lstats the
destination and moves any out-of-band file into `.squirrel-history/run-N/`
before the `os.Rename`, so the dedup optimisation is kept without the
silent overwrite.

The finding's own aside — "the same risk exists on the disposition
`Transfer` rclone path" — turned out to be real and was *not* covered by
#70. Round two caught it as
[#106](https://github.com/mbertschler/squirrel/issues/106) and
[#119](https://github.com/mbertschler/squirrel/pull/119) added the
symmetric `preStageTransfers` pass, which applies the same Lstat-and-
preserve treatment to every rclone-delivered path. Both passes now share
the same contract.

**Issue:** [#62 — agent: CopyFromExisting pre-stage must preserve any out-of-band file at the destination path](https://github.com/mbertschler/squirrel/issues/62)

---

### C3: No backup of the SQLite index

**Severity:** Critical • **Likelihood:** Inevitable on a long-enough
timeline (single disk).

**Where**

- `store/store.go` — opens the DB, no backup hook.
- `cmd/squirrel/` — no `db backup` / `db check` / `db dump` commands.
- Repo-wide — no Litestream integration, no docs about it.

**What can go wrong**

The index is the single source of truth for:

- Which content has ever been observed (the supersede chain).
- Which content is currently live where.
- Which runs touched which volume when.
- Cross-volume duplicates.
- Per-peer watermarks.
- Folder Merkle hashes.

A single corruption or filesystem loss of `~/.squirrel/index.db` wipes
all of that. The data files themselves survive (they're on a separate
filesystem if the user configured destinations that way), but the
**history** of how they got there, the per-path supersede ladder, the
record of "this file was once observed at this hash and is now gone"
— all evaporate.

The README's principle "paths are observations of content; content is
the entity" only works if the observations are durable. They aren't.

**Proposed mitigation**

This is the biggest single safety win in the audit. Implement in three
layers, each independently useful:

1. **`squirrel db backup --to <path>`** — uses SQLite's `VACUUM INTO`
   to write a consistent online snapshot of the DB to `<path>`. This
   is a single transaction, requires no extra dependencies, and runs
   while the agent is live. Default `<path>` is
   `~/.squirrel/backups/index-<ISO8601>.db`. Add a `--keep N` rotation
   policy so we don't unbounded-grow the backup tree.

2. **Automatic snapshot as part of every successful `sync`** — after
   `FinishRun(success)` for a sync run, atomically `VACUUM INTO` a
   snapshot and stash it under
   `<dest>:<root>/<volume>/.squirrel-index/index-<run-id>.db`. The
   snapshot rides along with the data it indexes; on restore from
   that destination, the user has both the bytes and the index.

3. **Documented Litestream wiring** — for users who want continuous
   replication. Litestream wraps the SQLite database and streams the
   WAL to S3/SFTP/etc. We don't depend on it; we provide an opinion in
   `README.md` (and ideally a tested example config) about how to set
   it up alongside squirrel. The key compatibility check: Litestream
   needs WAL mode (we already enable it) and `synchronous=FULL` or
   `NORMAL` (we keep FULL — perfect).

**Acceptance**

- `squirrel db backup` produces a snapshot that opens cleanly with
  `squirrel volumes` (i.e. it's a valid v8 DB).
- The post-sync snapshot lands at the destination and is excluded
  from future syncs (filtered the same way `.squirrel-history` is).
- A `db restore <snapshot.db>` command (or just docs about
  `cp snapshot.db ~/.squirrel/index.db`) demonstrates the recovery
  path.

**Scope of tracking issue #65**

Issue #65 covered layer 1 (`squirrel db backup` via `VACUUM INTO`) plus
the pre-migration snapshot from H5 and the `PRAGMA integrity_check`
from M3 — i.e. the immediately shippable primitives. Layer 2
(per-sync destination snapshots under
`<dest>/<volume>/.squirrel-index/`) and layer 3 (documented Litestream
wiring) were deferred follow-ups, listed as out-of-scope in #65 and
worth re-opening as separate issues once the core primitives landed.
Layer 2 did land, in #91; layer 3 never got its own issue and is the
one piece of C3 still outstanding — see the Status note below.

**Status:** Partial — layers 1 and 2 shipped, layer 3 has not.

- **Layer 1 — shipped** in [#73](https://github.com/mbertschler/squirrel/pull/73):
  `store.Backup` uses `VACUUM INTO`, exposed as `squirrel db backup` with
  `--keep N` rotation, alongside `db check` (M3) and `db restore`. The
  restore path was later hardened by
  [#117](https://github.com/mbertschler/squirrel/pull/117) after round two
  found it destroyed the live index with no undo
  ([#111](https://github.com/mbertschler/squirrel/issues/111)): it now
  preserves the displaced database at `<db>.pre-restore-<ns>` and rolls
  back on failure.
- **Layer 2 — shipped** in [#91](https://github.com/mbertschler/squirrel/pull/91),
  earlier than this document's "deferred" note assumed. Every successful
  sync snapshots the index locally and rides a copy along to
  `<dest>/<volume>/.squirrel-index/`, with its own `cloud_keep` rotation
  bound (`config/backups.go`, `sync/snapshot.go`). Round two found that
  this rotation was also sweeping away H5's pre-migration snapshots
  ([#112](https://github.com/mbertschler/squirrel/issues/112)); fixed in
  [#117](https://github.com/mbertschler/squirrel/pull/117), which scoped
  the two retention policies apart.
- **Layer 3 — not shipped.** There is no Litestream mention left anywhere
  in `README.md` or `docs/`. This is the only outstanding piece of C3, and
  it is prose plus a tested example config rather than code.

**Issue:** [#65 — store, cli: ship squirrel db {backup,check,restore} and snapshot before migrations](https://github.com/mbertschler/squirrel/issues/65) (covers layer 1 + H5 + M3; layer 2 landed separately in #91, layer 3 outstanding)

---

### C4: Sync runID fallback collides history into one bucket

**Severity:** Critical (potential) • **Likelihood:** Low today (no live
code path reaches it).

**Where**

- `sync/sync.go:339-355` — `backupDirURI` literal `"dry-run"` fallback.

**What can go wrong**

When `runID == 0` or `dryRun` is true, the per-run history bucket is
named `run-dry-run/` instead of `run-<id>/`. Today this only fires in
dry-run mode, but the guard mixes two intents into one branch (real
runID==0 vs dryRun=true).

A future bug — say, a regression where `beginSyncRunGuarded` succeeds
but returns runID==0 in a non-dry-run mode, or a refactor that drops
the `runID == 0` check on the wrong path — would silently funnel every
overwrite into the same `.squirrel-history/run-dry-run/` directory on
the destination, where successive runs would clobber each other.

**Proposed mitigation**

- Split the two intents: `dryRun` paths can keep the `run-dry-run/`
  literal because they never write; non-dry-run paths must reject
  `runID == 0` with a hard error before composing argv.
- Add a defensive assertion in `buildRcloneArgs` (or wherever the
  backup-dir is computed): `if !opts.DryRun && runID == 0 { panic
  or error }` — a real run must have a real id.
- Add a test that calls `buildRcloneArgs` with `DryRun=false, runID=0`
  and asserts the failure.

**Acceptance**

- The fallback string is gone from non-dry-run paths.
- A unit test exercises the guard.

**Status:** Resolved. [#67](https://github.com/mbertschler/squirrel/pull/67)
split the two intents as proposed: `buildRcloneArgs` now opens with
`if !opts.DryRun && runID == 0 { return nil, … }`, refusing before argv is
composed, while dry-run paths keep the `run-dry-run/` literal because they
never write. `sync/sync_test.go` exercises both halves — refusal at
`DryRun=false, runID=0`, success with a real id regardless of mode.

**Issue:** `sync: refuse to build rclone args with runID=0 outside dry-run`
(fixed in [#67](https://github.com/mbertschler/squirrel/pull/67); no
separate tracking issue was opened — it rode along with M6)

---

## High findings — detail

### H1: Cross-process index run race corrupts the live set

**Severity:** High • **Likelihood:** Medium (agent + manual CLI on the
same host).

**Where**

- `agent/scheduler.go:277-289` — `indexGatePassed` calls
  `HasRunningRun` then proceeds; the gate is TOCTOU.
- `index/index.go:201-215` — `beginRun` unconditionally inserts.
- Compare to `store/runs.go:408-449` — `BeginSyncRunIfClear` is the
  atomic equivalent for sync, but no index counterpart exists.

**What can go wrong**

`HasRunningRun` checks for an in-flight index row, then the caller
falls through and inserts a new one. Between the check and the insert
two processes can both observe "no running run" and both begin.

The volume lock the scheduler holds is in-process; a CLI `squirrel
index pictures` invocation has no visibility into it. Once both
indexers finish their walk, each calls `MarkMissing(volumeID,
currentRunID)`, which flips every row where `last_seen_run_id !=
currentRunID && status='present'` to `missing`. The two runs have
different `currentRunID`s, so each marks the other's freshly-touched
rows missing.

The net effect: a clean disk gets marked partially-missing in the
index. A subsequent peer sync sees those rows as `missing`, treats
them as new content, and may decide to re-transfer or — worse —
overwrite-on-supersede with an out-of-date watermark.

**Proposed mitigation**

- Add `BeginIndexRunIfClear` to `store.Store`, symmetric to
  `BeginSyncRunIfClear`. Atomic check + insert inside `BEGIN
  IMMEDIATE`.
- Convert `index.Index` and `agent/scan.go` to use it. Refuse to start
  if another index/audit run is in flight against the same volume,
  with the same "stale running rows clear via `runs fail`" recovery
  story sync already has.
- Audit runs likewise (`Kind == 'audit'`) — these go through the same
  index path so the gate covers them too.

**Acceptance**

- Concurrent `squirrel index` invocations against the same volume
  produce one success and one "already running" diagnostic.
- A regression test exercises the race directly via two goroutines
  hitting `Index` simultaneously.

**Status:** Resolved. [#69](https://github.com/mbertschler/squirrel/pull/69)
added `store.BeginIndexRunIfClear`, the atomic check-and-insert companion
to `BeginSyncRunIfClear`, and wired `index.Index` and the agent's scan path
through it; the gate validates the run kind so `index` and `audit` both
pass through it. A loser gets the blocking run back rather than a second
row, and the scheduler reports it (`agent/scheduler.go`) instead of racing.
`TestBeginIndexRunIfClearAtomic` drives concurrent begins directly, and
`TestBeginIndexRunIfClearRejectsWrongKind` pins the kind validation.

**Issue:** [#63 — store: add BeginIndexRunIfClear and wire it through index/audit paths](https://github.com/mbertschler/squirrel/issues/63)

---

### H2: `FinishRun` blindly overwrites terminal state

**Severity:** High • **Likelihood:** Low today (one known double-call
path), but the invariant is unenforced.

**Where**

- `store/runs.go:131-156` — `FinishRun` updates without checking the
  current status.
- `agent/sync.go:1013-1018` — `handleClose` calls `FinishRun(Failed)`
  on top of any `FinishRun` `closeSession` may already have made.

**What can go wrong**

`FinishRun` accepts any transition. `handleClose` has a real
double-finish: `closeSession` calls `FinishRun(...)` on its happy
path; if that call's commit succeeds but it returns an error to the
caller for any later reason (e.g. peer_sync_state advance fails), the
outer handler calls `FinishRun(..., Failed, ...)` and the original
status, error message, and ended-at timestamp are silently rewritten.

Even outside that specific path, any future buggy retry that calls
`FinishRun` twice corrupts the audit log: the "original" failure is
gone forever and replaced by the second writer's view.

**Proposed mitigation**

- Add a status guard inside `FinishRun`: refuse the update if the row
  is already in a terminal state, returning a structured "already
  finished" error the caller can match on.
- Fix `handleClose` and any other double-call sites to honour the
  guard (they should pass the error through rather than try to
  finalise again).
- Add a test that asserts a finished row cannot be re-finalised.

**Acceptance**

- `FinishRun` rejects transitions away from terminal status.
- The handleClose double-call is replaced with a "log only" fallback.

**Status:** Resolved. [#81](https://github.com/mbertschler/squirrel/pull/81)
(tracked as [#78](https://github.com/mbertschler/squirrel/issues/78)) put
the guard inside `finishRun`: a row already in a terminal status is never
re-finalised, and the caller gets `store.ErrAlreadyFinished` to match on.
The first terminal write wins — its status, error, and `ended_at_ns` all
stand. `handleClose` was converted to the log-only fallback the acceptance
criteria asked for. The terminal-status set has since grown (`refused`,
`aborted`, added by [#174](https://github.com/mbertschler/squirrel/pull/174));
`isTerminalStatus` is the single place that defines it, so the guard
covered the new states for free.

**Issue:** `store: FinishRun must refuse to overwrite a terminal-status row`
→ tracked in [#78](https://github.com/mbertschler/squirrel/issues/78),
fixed in [#81](https://github.com/mbertschler/squirrel/pull/81)

---

### H3: `runs fail` erases the original end state without an audit line

**Severity:** High • **Likelihood:** Every operator recovery.

**Where**

- `cmd/squirrel/runs_fail.go:38-65`.

**What can go wrong**

`runs fail` is the documented recovery path for a row stuck in
`running` after a crashed process. It calls the same `FinishRun` as a
real finalisation, so the row ends up indistinguishable from any other
clean failure aside from the synthesized "marked failed manually at
…" message. The file_count is passed in verbatim from whatever was
left in the row.

There's no separate column / row that records "this transition was
operator-driven, not real". A forensic reader of the runs table can't
tell a real failure from a manual cleanup.

In addition: the `error` column gets the synthesized timestamp string,
not the underlying failure reason — which we don't have anyway, but
the original `started_at_ns` is preserved while the `ended_at_ns` is
moved to "now". A reader looking at duration sees "ran for 4 hours"
when really the process died after 4 minutes and was recovered 3:56
later.

**Proposed mitigation**

- Add a `runs_audit` table (insert-only) capturing
  `(run_id, transition, operator, at_ns, note)`. Every `FinishRun`
  call writes a row; `runs fail` writes a row tagged
  `transition='manual-fail'`.
- Or, extend the `runs` schema with a nullable
  `finished_by_operator_at_ns` column distinct from `ended_at_ns`, so
  the read shape stays one row per run.
- Bonus: `runs fail` could refuse to clobber the file_count, leaving
  it at whatever the running row last carried.

**Acceptance**

- After `runs fail`, the user can distinguish manual vs real
  failures from the runs listing without parsing the error string.
- A test asserts the audit row / column is populated.

**Status:** Resolved. [#81](https://github.com/mbertschler/squirrel/pull/81)
(tracked as [#78](https://github.com/mbertschler/squirrel/issues/78)) took
the first of the two proposed shapes: an insert-only `runs_audit` table
of `(run_id, transition, operator, at_ns, note)`. Every `FinishRun` writes
a `finish` row; `runs fail` writes one tagged `manual-fail`
(`store.TransitionManualFail`) naming the operator, so a forensic reader
distinguishes operator cleanup from a real failure without parsing the
error string.

The table has since become the general audit surface for operator acts,
which is why `design/ux-principles.md` can promise that clearing a latch
stays recoverable: alarm raise/clear, contested-path raise/clear, and
`set-correlated-run-id` (H6) all append to it.

**Issue:** `runs: distinguish manual-fail from real-fail in the audit log`
→ tracked in [#78](https://github.com/mbertschler/squirrel/issues/78),
fixed in [#81](https://github.com/mbertschler/squirrel/pull/81)

---

### H4: TOCTOU between classify and pre-stage can misattribute bytes

**Severity:** High • **Likelihood:** Low (requires a writer touching
the receiver volume mid-sync — possible on a NAS with web access).

**Where**

- `agent/sync.go:622-720` — `classify` / `dispositionForExisting`.
- `agent/sync.go:829-851` — `preMoveSupersedes`.
- `agent/sync.go:870-905` — `preStageConflicts`.

**What can go wrong**

`classify` reads the receiver's index to get `existing.Blake3` and
decides `Supersede` or `Conflict`. The pre-stage then does
`os.Rename(srcAbs, dstAbs)` where `srcAbs` is the path on disk and
`dstAbs` is the history/conflict bucket. Nobody re-hashes `srcAbs`
between classify and pre-stage.

If the on-disk bytes drifted out-of-band (a user dropped a new version
in via a NAS web app between the last index and this sync), the bytes
that land in `.squirrel-history/run-N/<path>` are NOT the bytes the
index says they were. The index row at the supersede path keeps
`blake3 = old`, but the history file at `run-N/path` actually contains
`drift`. Content history is silently corrupted.

The verify phase only checks the receiver's destination path post-
rclone; it does not check that the supersede bucket carries the
recorded blake3.

**Proposed mitigation**

- Re-hash `srcAbs` immediately before the rename in supersede and
  conflict pre-stages. If the hash matches `priorRow.Blake3`, proceed.
  If it doesn't, downgrade the disposition to `Conflict` with reason
  "on-disk drift during sync"; the drifted bytes are preserved at the
  conflict path and the initiator's bytes still land live.
- The pre-existing audit-scan (#17) flow already re-hashes, so this is
  the same logic localised to the sync hot path.

**Acceptance**

- A test seeds the receiver with content X at path P, indexes it,
  then mutates P on disk to bytes Y _before_ the sync runs; asserts
  the pre-stage detects the drift and preserves Y under
  `.squirrel-conflicts/`.

**Status:** Resolved. [#80](https://github.com/mbertschler/squirrel/pull/80)
(tracked as [#76](https://github.com/mbertschler/squirrel/issues/76))
shipped the proposed mitigation verbatim: `rehashSource` re-hashes the
on-disk bytes immediately before the rename in both `preMoveSupersedes`
and `preStageConflicts`, and a mismatch downgrades the disposition to a
conflict stamped with the reason `"on-disk drift during sync"`. The
drifted bytes are preserved under `.squirrel-conflicts/` and the
initiator's bytes still land live, so neither version is lost.

**Issue:** `agent: re-hash on-disk bytes before supersede/conflict pre-stage moves`
→ tracked in [#76](https://github.com/mbertschler/squirrel/issues/76),
fixed in [#80](https://github.com/mbertschler/squirrel/pull/80)

---

### H5: No pre-migration snapshot of the index

**Severity:** High • **Likelihood:** Every binary upgrade.

**Where**

- `store/migrations.go` — runs migrations in transactions but never
  snapshots.

**What can go wrong**

Schema migrations are transactional inside SQLite (good — a crash
mid-migration rolls back). But:

- If a migration is buggy and **commits** bad state, the rollback
  doesn't help.
- If the DB has pre-existing corruption that a migration walks over
  without noticing, the post-migration state can be worse than the
  pre.
- If the user upgrades, hits a migration bug, and reports it, the dev
  side has no "before" copy to compare against.

**Proposed mitigation**

- Before running any migration that ends at `current < target`,
  `VACUUM INTO ~/.squirrel/backups/pre-migration-v<from>-to-v<to>-<ts>.db`.
- Document the backups, including how to restore from one.
- (Pairs naturally with #C3.)

**Acceptance**

- A migration that crashes mid-flight is recoverable by copying the
  pre-migration backup over the live db.
- A test seeds a v7 DB, runs migrate to v8, asserts a backup file is
  present.

**Status:** Resolved. [#73](https://github.com/mbertschler/squirrel/pull/73)
made `migrate` take a `VACUUM INTO` snapshot before stepping an *existing*
database (a fresh DB has nothing to lose, so it skips), landing at
`<BackupDir>/pre-migration-v<from>-to-v<to>-<ts>.db`. `store_test.go`
asserts the file appears with that name after a v5→current migration, and
`store/migrate_v27.go` leans on it explicitly as the rollback surface for
the bulk STRICT rebuild.

Round two found the snapshots were being swept away by the layer-2
snapshot rotation ([#112](https://github.com/mbertschler/squirrel/issues/112));
[#117](https://github.com/mbertschler/squirrel/pull/117) separated the two
retention policies so a routine sync can no longer discard the artefact an
upgrade depends on.

**Issue:** [#65 — store, cli: ship squirrel db {backup,check,restore} and snapshot before migrations](https://github.com/mbertschler/squirrel/issues/65) (combined with C3 + M3), fixed in [#73](https://github.com/mbertschler/squirrel/pull/73)

---

### H6: The watermark and correlated_run_id are overwrite-in-place

**Severity:** High (forensic) • **Likelihood:** Low today, but the
record loss is permanent.

**Where**

- `store/peer_sync.go:43-55` — `UpsertPeerSyncState` `DO UPDATE SET ...`.
- `store/runs.go:114-129` — `SetCorrelatedRunID` blind update.

**What can go wrong**

Both fields are overwritten with no history. If a peer-sync writes a
bad watermark (a future bug, a hostile peer claiming a correlated run
id we never agreed to, a misordered close…) the prior watermark is
gone. Drift detection then anchors against the bad value forever.

This is "forget metadata about volumes that were used recently"
exactly.

**Proposed mitigation**

- Add an insert-only `peer_sync_state_history` table; every
  `UpsertPeerSyncState` writes both the upsert and a history row.
  Similarly for `SetCorrelatedRunID`: a `runs_audit` entry per
  transition.
- Optional: refuse non-monotonic watermark moves
  (`new_last_shared_run_id < old`) by default, behind a `--allow-
  rewind` opt-in for genuine recovery.

**Acceptance**

- A `squirrel peer-sync history <volume> <peer>` listing shows every
  watermark transition.
- A test exercises monotonicity refusal.

**Status:** Resolved, including the optional half.
[#87](https://github.com/mbertschler/squirrel/pull/87) (tracked as
[#77](https://github.com/mbertschler/squirrel/issues/77)) added the
insert-only `peer_sync_state_history` table (schema v12) and writes the
upsert plus its history row in one transaction, so the append-only log can
never diverge from the live row. `SetCorrelatedRunID` got the same
treatment against `runs_audit`, with the note carrying old→new.

Both acceptance criteria hold: `squirrel peer-sync history <volume> <peer>`
lists every watermark transition, and `guardWatermarkMonotonicTx` refuses
a rewind with `ErrWatermarkRewind` unless `allowRewind` is passed — the
optional monotonicity bullet, shipped rather than skipped. The
`--allow-rewind` opt-in surfaces on `squirrel peer-sync pull-durability`.
Fittingly for a finding about lost provenance, the code says so out loud:
`store/peer_sync.go` and `store/runs.go` both cite "SAFETY-AUDIT H6".

**Issue:** `store: keep watermark and correlation history append-only`
→ tracked in [#77](https://github.com/mbertschler/squirrel/issues/77),
fixed in [#87](https://github.com/mbertschler/squirrel/pull/87)

---

### H7: Destination root and restore target are not marker-guarded

**Severity:** High • **Likelihood:** Low (requires misconfiguration),
but blast radius is very high.

**Where**

- `sync/sync.go:326-355` — `destinationVolumeURI` and `backupDirURI`
  compose paths from `dest.Root` and `vol.Path` directly.
- `config/destinations.go:88-101` — `root` is validated as non-empty
  but not for path safety.

**What can go wrong**

`dest.Root` is taken verbatim. A user who typos
`root = "/var/lib/squirrel"` as `root = "/var/lib"` would tell rclone
to write into `/var/lib/<volume>/`, intermixed with other system
directories. With `--backup-dir` rclone will move overwritten files
into `/var/lib/<volume>/.squirrel-history/`, but the destination's
non-squirrel directories may be silently treated as part of the
"comparison set", and `rclone copy` may overwrite or move them.

Same risk on restore against the local volume path: a misconfigured
`vol.Path` points restore at a directory of unrelated personal files;
rclone overwrites them.

**Proposed mitigation**

- On first sync to a destination, write a marker file at
  `<dest>:<root>/<volume>/.squirrel-volume` containing the volume name
  and the squirrel version that initialised it.
- On every subsequent sync, refuse to run if the marker is missing
  (the destination dir was either wiped or this isn't actually our
  destination). Optional `--init` flag to bootstrap a new destination
  intentionally.
- Same for the source side of restore: the local volume path should
  carry a `.squirrel-volume` marker too, with the same gating logic.
- The marker file gets filtered out of all comparison and transfer
  flows (similar to `.squirrel-history`).

**Acceptance**

- Sync against a destination missing the marker fails with a clear
  message and a `--init` workaround.
- A regression test asserts the marker is created on first sync.

**Status:** Resolved. #64 (shipped in
[#71](https://github.com/mbertschler/squirrel/pull/71)) shipped the marker
mechanism, local-destination gating, and the restore source-side check.
#150 (shipped in [#170](https://github.com/mbertschler/squirrel/pull/170))
extended enforcement to remote rclone destinations (`sftp`, `s3`, `b2`,
`gcs`) — read/written through the same overlay the transfer uses — across
the mirror, content-addressed, and packed layouts, with the marker filtered
out of every transfer, comparison, and restore. A read that fails for any
reason other than a definite "not found" refuses without writing, so a
reachability blip cannot be mistaken for a fresh root.

**Issue:** [#64 — sync: require .squirrel-volume markers on destination and source to gate against misconfiguration](https://github.com/mbertschler/squirrel/issues/64); [#150 — sync: enforce .squirrel-volume markers on remote rclone destinations](https://github.com/mbertschler/squirrel/issues/150)

---

## Medium findings — detail

### M1: --shallow mode on bucket sync can skip a divergent destination

**Severity:** Medium • **Likelihood:** Only when users opt in with
`--shallow`.

**Where**

- `sync/sync.go:36-44` — Options.Shallow documented.
- `sync/sync.go:296-313` — `--checksum --hash blake3` only added when
  not shallow.

**What can go wrong**

With `--shallow`, rclone uses size + mtime as the comparison key.
Destination drift that preserves size+mtime stays invisible. Tested
behaviour, but the README only briefly mentions the trade-off.

**Proposed mitigation**

- Make `--shallow` print a warning at run start: "skipping BLAKE3
  verification; destination drift with matching size/mtime will not
  be detected".
- Persist the choice on the runs row (`shallow_mode boolean`) so
  forensic readers can tell which runs were verified vs assumed.
- Periodic full verification: agent's scan loop already runs
  drift-detection locally; consider an agent-side option to also
  verify the latest sync's destination subset.

**Acceptance**

- The warning fires; tests assert the runs row records the mode.

**Status:** Resolved. [#82](https://github.com/mbertschler/squirrel/pull/82)
(tracked as [#79](https://github.com/mbertschler/squirrel/issues/79)) took
both of the first two bullets: `shallowSyncWarning` prints at run start on
every shallow sync and restore, and `runs.shallow` persists the choice so a
forensic reader can tell verified runs from assumed ones. `BeginRun` now
*refuses* index-kind runs outright to force them through `BeginIndexRun`,
so the column can never be silently left unset for a walk.

The third bullet — periodic full verification — was not built here but has
since been answered better than proposed: `squirrel verify` plus the
agent's `verify_every` cadence re-check offsite fingerprints on a schedule
(F32), rather than opportunistically re-verifying the last sync's subset.

**Issue:** `sync: surface --shallow trade-off in logs and persist it on the runs row`
→ tracked in [#79](https://github.com/mbertschler/squirrel/issues/79),
fixed in [#82](https://github.com/mbertschler/squirrel/pull/82)

---

### M2: Folder Merkle hashes have no self-check

**Severity:** Medium • **Likelihood:** Theoretical (the hashes are
populated and updated in the same transaction as the file rows), but
a bug in the recomputation logic could silently desync.

**Where**

- `store/folders.go` — recompute helpers.

**What can go wrong**

A bug in `recomputeFoldersClosure` (or any future hash-related
migration) could leave folder hashes stale relative to the file rows
they cover. Peer-sync's Merkle walk would then either skip subtrees
that did change (silent dataloss) or replan whole subtrees that
didn't (just performance).

**Proposed mitigation**

- Add `squirrel audit --folders` mode that walks every folder, derives
  shallow/deep from current file/child rows, and asserts equality with
  the stored value. Report any divergence.
- Run it as part of CI on test fixtures and optionally as a periodic
  agent check.

**Acceptance**

- The audit command exists, lists divergences, and exits non-zero on
  any.

**Status:** Resolved. [#87](https://github.com/mbertschler/squirrel/pull/87)
(tracked as [#77](https://github.com/mbertschler/squirrel/issues/77)) added
`squirrel audit <volume> --folders`, a pure index self-check that re-derives
every folder's shallow and deep hash from the live file and child rows and
compares against what is stored. It touches no disk, writes no run, reports
each divergence, and exits non-zero when any is found — the acceptance
criteria as written. `cmd/squirrel/audit_test.go` covers both directions by
corrupting a stored `shallow_blake3` and asserting the command notices.

The "run it in CI on fixtures" half of the mitigation is covered by that
test; wiring it onto an agent cadence was left out and remains optional.

**Issue:** `audit: self-check folder Merkle hashes against the live row set`
→ tracked in [#77](https://github.com/mbertschler/squirrel/issues/77),
fixed in [#87](https://github.com/mbertschler/squirrel/pull/87)

---

### M3: No `PRAGMA integrity_check` anywhere

**Severity:** Medium • **Likelihood:** Long-running databases on
imperfect hardware.

**Where**

- `store/store.go` — never invokes integrity_check.

**What can go wrong**

SQLite corruption (bit-flip in storage, kernel cache bug, etc.) only
surfaces when the affected page is read. Whole subtrees of the index
can be silently broken until a `query` happens to touch them.

**Proposed mitigation**

- `squirrel db check` runs `PRAGMA integrity_check` (full mode) and
  prints the result.
- Run it as part of every `audit` invocation, or at agent startup.
- On failure: surface the error, point at the most recent backup
  (#C3), don't attempt repair (SQLite's `RECOVER` is opt-in and
  destructive).

**Acceptance**

- `squirrel db check` exits 0 on a clean DB and non-zero on a corrupted
  one; tests both via a `.bak`-then-corrupt fixture.

**Status:** Resolved. [#73](https://github.com/mbertschler/squirrel/pull/73)
added `store.IntegrityCheck` (`PRAGMA integrity_check`, full mode) behind
`squirrel db check`, which exits 0 on a clean database and non-zero on a
corrupted one. It reports and points at the backups rather than attempting
repair, as the mitigation insisted.

Not adopted: running it inside every `audit` invocation or at agent
startup. A full integrity check reads every page, which on a large index is
the kind of routine cost that would make the agent's cadences unpredictable
— it stays an explicit question the operator asks
([principle 2](design/ux-principles.md#2-the-cli-is-for-change-and-for-questions--never-for-operations)).

**Issue:** [#65 — store, cli: ship squirrel db {backup,check,restore} and snapshot before migrations](https://github.com/mbertschler/squirrel/issues/65) (combined with C3 + H5), fixed in [#73](https://github.com/mbertschler/squirrel/pull/73)

---

### M4: source_node_id attribution is overwritten on touch

**Severity:** Medium • **Likelihood:** Every peer-sync touch of an
unchanged-content path.

**Where**

- `store/files.go:452-471` — `updateLiveRow` rewrites
  `source_node_id` / `source_run_id`.

**What can go wrong**

When a file is observed by a different node (or run) with the same
blake3, `updateLiveRow` rewrites the provenance. The previous
attribution is gone. A forensic question like "who first attributed
this content to this path?" can't be answered from the row alone — the
history walk reconstructs blake3 changes, but the *attribution* of an
unchanged row is mutable.

**Proposed mitigation**

- Preserve `first_seen_source_node_id` / `first_seen_source_run_id`
  alongside the live ones, set on insert and never updated.
- Or, when the touch's `source_node_id` differs from the current row's
  `source_node_id`, supersede the row instead of updating it (giving
  the prior attribution a permanent superseded row). That's a heavier
  change with index-bloat implications; the dual-column approach is
  cheaper.

**Acceptance**

- A path observed first by node A then touched by node B can be
  queried for "originally attributed to A".

**Status:** Dissolved rather than fixed — neither proposed mitigation was
needed, because the field the finding is about no longer exists on a
mutable row.

[#97](https://github.com/mbertschler/squirrel/pull/97) split `files` into
an append-only `contents` entity plus path↔content observations (schema
v14–v16). Origin attribution moved with it: `contents.origin_node_id` /
`origin_run_id` now hang off the content, which is written once and never
updated, so there is no `updateLiveRow` rewrite left to lose. The
`files` row keeps `first_seen_run_id`, which is likewise never rewritten.
[#125](https://github.com/mbertschler/squirrel/pull/125) made the
invariant load-bearing rather than conventional, adding the
`contents_no_update` and `contents_no_delete` triggers (schema v21) that
abort any attempt at either.

The forensic question the acceptance criterion posed — "a path observed
first by node A then touched by node B can be queried for 'originally
attributed to A'" — is answerable today, via the content row rather than a
second pair of columns on the file row. Worth noting the direction of
travel: the fix that landed is stronger than the one proposed, because it
removes the mutation instead of shadowing it. Round two then attacked the
same surface from the other side —
[#105](https://github.com/mbertschler/squirrel/issues/105), where a peer
could *self-attribute* content origin and poison the durability vector,
fixed in [#119](https://github.com/mbertschler/squirrel/pull/119).

**Issue:** `store: preserve first-seen provenance on files independently from current attribution`
→ obsoleted by the contents/files split in
[#97](https://github.com/mbertschler/squirrel/pull/97); no separate issue
was opened and none is needed

---

### M5: rclone config is the only place secrets get resolved, then re-resolved

**Severity:** Medium • **Likelihood:** Low.

**Where**

- `sync/rclone.go:117-161` — `WriteRcloneConfig` rewrites the file on
  every sync.

**What can go wrong**

Every sync resolves secrets from env and writes them to disk. There's
no "did this match what we wrote last time?" check. A buggy resolver
or env-var manager could silently rewrite the file with empty
credentials, causing the next sync to fail oddly.

**Proposed mitigation**

- Hash the rendered config before write and skip the write if
  identical. Log a single line on real rewrites.
- Or just persist a per-section "last-written digest" on the destination
  row and assert it matches expectations.

**Acceptance**

- A test asserts back-to-back syncs don't churn `~/.squirrel/rclone.conf`'s
  mtime.

**Status:** Resolved. [#82](https://github.com/mbertschler/squirrel/pull/82)
(tracked as [#79](https://github.com/mbertschler/squirrel/issues/79)) took
the first mitigation: `WriteRcloneConfig` renders the config, compares the
bytes against what is already on disk, and leaves an identical render
untouched — no truncate, no mtime bump. It returns `wrote bool` so a caller
can log the single line on a genuine rewrite, which is what turns a buggy
resolver silently regressing credentials into a visible event.

Two hardening details went beyond the finding: a real rewrite is atomic
(fsync'd temp file renamed over the target), so a crash mid-write cannot
leave a half-rendered config holding live secrets; and the skip path still
chmods to 0600, so a 0644 file left by an older squirrel gets tightened
even when its content already matched.

**Issue:** `sync: only rewrite rclone.conf when its content actually changes`
→ tracked in [#79](https://github.com/mbertschler/squirrel/issues/79),
fixed in [#82](https://github.com/mbertschler/squirrel/pull/82)

---

### M6: tempfile write in CopyFromExisting lacks fsync

**Severity:** Medium • **Likelihood:** System-crash-window-only.

**Where**

- `agent/sync.go:790-820` — `copyFileToPath` closes the tempfile
  without `tmp.Sync()` first.

**What can go wrong**

The OS may buffer the tempfile's contents in page cache. If the system
crashes after `os.Rename` but before the kernel writes the data,
`dstAbs` exists but is zero-length. Subsequent `verify` catches this
(re-hash mismatch). But the original copy_from source might have been
moved/changed in the meantime, and the index pre-stage already
recorded the conflict row.

The damage radius is small (verify rejects, sync goes partial, the
next sync replans), but the audit chain captures a brief "this file
existed and now doesn't" state.

**Proposed mitigation**

- `tmp.Sync()` before `tmp.Close()` in `copyFileToPath`.
- Same for any other tempfile-then-rename flow.

**Acceptance**

- `copyFileToPath` calls `Sync` before close.

**Status:** Resolved. [#67](https://github.com/mbertschler/squirrel/pull/67)
added the `tmp.Sync()` before close in `copyFileToPath`, so the rename can
no longer publish a name whose bytes are still only in page cache.

**Issue:** `agent: fsync the CopyFromExisting tempfile before atomic rename`
(fixed in [#67](https://github.com/mbertschler/squirrel/pull/67); no
separate tracking issue — it rode along with C4)

---

### M7: Orphan volume warning is only a warning

**Severity:** Medium • **Likelihood:** Frequent for users restructuring
configs.

**Where**

- `cmd/squirrel/root.go:127-140`.

**What can go wrong**

The warning prints once and scrolls off the screen. A user with five
shells doing different things will easily miss it. An automated
caller never sees it.

**Proposed mitigation**

- On orphan detection, refuse to run sync/restore against any
  orphan-affected volume until the operator explicitly acknowledges
  (e.g. `squirrel volumes acknowledge-orphan <name>` which writes a
  row to `volumes_acks` so the warning subsides).
- Pair this with the archive workflow in #H5: the right resolution is
  to archive the volume, not just suppress the warning.

**Acceptance**

- Orphan-affected operations error until the orphan is archived or
  acknowledged.

**Status:** **Open.** Nothing has changed here.
`warnOrphanVolumes` (`cmd/squirrel/root.go`) still prints one stderr line
per orphan on every config-aware invocation, swallows its own errors, and
gates nothing. There is no `volumes_acks` row, no archive workflow, and no
refusal. The `#H5` cross-reference in the mitigation is a typo for `#M8`'s
neighbourhood — the archive workflow it points at was never specified
anywhere.

What *has* changed is that squirrel now has the mechanism this needs, twice
over. `design/ux-principles.md` principle 4 names the shape explicitly —
latch, then require an acknowledgement — and two implementations exist to
copy: `destination_alarms` cleared by `squirrel verify ack`, and
`contested_paths` cleared by `squirrel conflicts resolve`. Both write a
`runs_audit` row naming the operator, which is exactly the "someone decided
this was fine" record this finding wants. Closing M7 is now a wiring
exercise against an established pattern rather than a design question, and
it is the last Medium finding with real work left in it.

**Issue:** `cli: escalate orphan-volume warnings into a refuse-to-run gate with an explicit acknowledgement`
→ **still open; no tracking issue has been filed**

---

### M8: runs table has no retention policy flag

**Severity:** Low/Medium (documentation) • **Likelihood:** N/A.

**Where**

- `store/runs.go` — runs grow forever, by design.

**What can go wrong**

Nothing today. But the policy "we never prune runs" should be
explicit, both in CLAUDE.md and in a no-op `runs prune --confirm`
placeholder command. If/when the user wants to prune (e.g. after a
decade) the command's shape forces the multi-step confirmation
philosophy upfront rather than us inventing it under pressure.

**Proposed mitigation**

- Document the policy in `CLAUDE.md` and `README.md`.
- Add `squirrel runs prune` that today only `--list-candidates` (no
  destructive operation yet); when destruction is added it requires
  `--confirm-volume <name>` and `--before <iso8601>` and a count
  matching the operator's announcement.

**Acceptance**

- The doc and the placeholder command exist.

**Status:** Partial, and the remainder is probably a deliberate no.

The policy half — the load-bearing half — landed and then some. It is
stated in `AGENTS.md` ("squirrel never auto-prunes runs — they're an audit
trail, and any retention is explicit and operator-driven"), in
`README.md`, and across the docs site
(`docs/src/content/docs/concepts/runs.md`, `start/principle.md`,
`guides/auditing.md`). `runs_audit` inherited the same stance when it was
added for H3.

The placeholder `squirrel runs prune --list-candidates` command was never
added, and on reflection it should probably stay unwritten.
`design/ux-principles.md` principle 2 says every command is either a change
or a question; a no-op reservation is neither, and shipping one would put a
destructive-sounding verb in `--help` years before it does anything. The
argument the finding makes — that the confirmation shape is better designed
in calm than under pressure — is a good one, but it is satisfied by writing
the shape down rather than by compiling it.

Recommendation: close this deliberately as done-in-substance rather than
leaving it to read as pending work.

**Issue:** `runs: codify the "never auto-prune" policy and add a placeholder retention command`
→ policy shipped; placeholder command not built and not planned

---

## Low findings — detail

### L1: buildDSN collapses on trailing spaces in path

**Where**: `store/store.go:67-78`.

**Mitigation**: After rejecting `?` and `#`, also reject paths with
leading/trailing whitespace or NUL bytes. Tighten with a regex.

**Status:** Resolved. [#82](https://github.com/mbertschler/squirrel/pull/82)
(tracked as [#79](https://github.com/mbertschler/squirrel/issues/79))
extended `validateDBPath` to reject NUL bytes, leading/trailing ASCII
whitespace, and — beyond the finding — anything that looks like a URI
(`://` or a `file:` prefix), so a DSN can never be smuggled in through the
path. The whitespace check is deliberately scoped to the ASCII set rather
than `unicode.IsSpace`: a path legitimately containing a non-breaking space
is the user's business, while the ASCII set is what shell-quoting and
copy-paste slips actually produce. The finding's "unicode lookalikes"
suggestion was considered and rejected on those grounds.

**Issue:** `store: validateDBPath should reject trailing whitespace and NUL`
→ tracked in [#79](https://github.com/mbertschler/squirrel/issues/79),
fixed in [#82](https://github.com/mbertschler/squirrel/pull/82)

---

### L2: hostname fallback can silently produce an empty node name on edge platforms

**Where**: `store/store.go:119-162`.

**Mitigation**: When `sanitiseNodeName` returns "", generate a
deterministic fallback (e.g. hash of MAC or content of
`/etc/machine-id`) instead of erroring.

**Status:** Resolved. [#82](https://github.com/mbertschler/squirrel/pull/82)
(tracked as [#79](https://github.com/mbertschler/squirrel/issues/79)) added
`fallbackNodeName`: when `sanitiseNodeName` yields nothing usable, the node
name becomes `node-<short hash>` seeded from `/etc/machine-id` where it
exists (stable across reboots and independent of the unusable hostname) and
from the raw hostname otherwise. The `node-` prefix satisfies the
leading-alphanumeric anchor the name regex requires. A MAC address was
considered as the seed and passed over — machine-id is stabler and needs no
interface enumeration.

**Issue:** `store: deterministic fallback node name when hostname sanitises to empty`
→ tracked in [#79](https://github.com/mbertschler/squirrel/issues/79),
fixed in [#82](https://github.com/mbertschler/squirrel/pull/82)

---

### L3: no pre-flight rclone-version check for restore-from-node

**Where**: future feature; placeholder.

**Mitigation**: Track in the issue tracker so the gate isn't forgotten
when restoreFromNode lands.

**Status:** **Open, and still prospective** — correctly so. `restoreFromNode`
does not exist; `squirrel restore` always pulls from an rclone destination.
`--from <node>` looks like the missing feature but is not: it filters the
restore to paths whose *content originates* at that node, and the bytes
still come from a destination.

Two things have improved the odds since. `runRestore` now calls
`EnsureMinVersion` unconditionally before any transfer, so a
restore-from-node routed through that command inherits the gate without
anyone remembering to add it; and the version gate is now shallow-aware
(`EffectiveShallow`), so it checks the right minimum for the mode. A
peer-API restore built as a separate path would still miss it. Keep the
placeholder until the feature lands or is ruled out.

**Issue:** `restore: ensure EnsureMinVersion runs in the restore-from-node path when implemented`
→ **still open**; nothing to gate yet

---

## Durability evidence & offsite verification (offload-v1)

Findings specific to the offload-v1 feature set — the durability version
vectors that gate `offload`, the peer durability pull that carries
evidence between nodes, and the content-addressed offsite push. These
are framed for the intended deployment: a **single operator** whose
nodes (laptop, NAS) are all machines they control and hold the only
credentials to. Under that model the adversary is overwhelmingly
*entropy and bugs*, not a hostile peer; the findings are sized
accordingly.

### D1: Durability-pull trust boundary (relayed offsite evidence)

**Severity:** Medium (defence-in-depth) • **Likelihood:** N/A
(documented assumption, not a live defect).

**Where**

- `sync/durability.go` — `PullDurability` / `pullDurability`,
  `validateComponent`, `validateFreshness`.
- `offload/gate.go` — `check`, `methodVerified`, `freshnessFailure`.
- `store/nodes.go` — `GetOrCreateOriginNode`.

**The boundary**

A durability component is recorded one of two ways, and they differ in
what they trust:

- **Direct, self-verified.** When this node pushes to a target itself —
  the NAS via peer sync (`sync/node.go`, tagged `peer-blake3` after the
  receiver re-hashes every path) or a bucket via rclone (`sync/sync.go`)
  — it writes the component into its **own** store from its **own**
  confirmed transfer (`AdvanceDestinationVectorTo`). No peer is trusted.
- **Relayed, peer-asserted.** For a target this node never pushes to (an
  offsite only the NAS reaches), the only evidence is what the NAS
  reports over the durability pull (`UpsertDestinationRunIDVerified`,
  reached only from `pullDurability`). Putting such a target in a
  volume's `offload_requires` means the local delete decision trusts the
  NAS's recorded `(origin, run, method)` assertion. The pull validates
  shape (positive run, valid origin name, recognised method) and is
  monotonic, but carries **no proof of possession** — a peer that
  asserts an inflated run for a destination in the accepted set would be
  believed.

**Decision (intended):** the relayed-evidence trust is **accepted**. The
NAS is in the same trust domain as the laptop; a NAS that lies about
durability is a compromised-or-broken NAS, in which case the archive it
holds is already in question and `offload` is a footnote. The gate fails
*closed* on absent evidence, so a peer can only ever *withhold*
offload-eligibility, and the redundancy decision (gate on **all** copies
via `offload_requires`, not the fewest-trusted subset) is what protects
against data loss — see the offload section of `README.md`.

**Defence-in-depth implemented in this branch** (cheap; turns *bugs*
into loud failures, not a security control):

- **Verify-method allow-list at the pull boundary** — `validateComponent`
  refuses a non-empty `verify_method` that isn't one this build defines
  (`store.KnownVerifyMethod`). Previously an unknown method was stored
  and then silently treated as unverified by the gate; now a peer bug or
  version-skew string is rejected at receipt. Empty (legitimately
  "unverified") still passes.
- **Origin-node creation cap** — `pullDurability` refuses a pull that
  names more than `maxOriginNodesPerPull` (256) distinct origins, so a
  runaway peer cannot grow the local `nodes` table without bound via
  `GetOrCreateOriginNode`. A real volume references a handful of origins;
  the cap only converts a flood into an observable refusal.

**Not done (deliberately):** no proof-of-possession protocol, no
laptop-side independent verification of relayed offsites. Those defend
against a malicious NAS, which is out of model. The random per-file
nonce in the rclone crypt overlay also makes "the NAS proves the stored
ciphertext decrypts to the right content" impractical without either a
content-derived nonce or the laptop downloading and decrypting the
object — neither warranted here.

**Status:** Resolved. "This branch" was
[#128](https://github.com/mbertschler/squirrel/pull/128), long since
merged. Both guards are live: `validateComponent` rejects any non-empty
`verify_method` outside `store.KnownVerifyMethod`, and `pullDurability`
refuses a pull naming more than `maxOriginNodesPerPull` (256) distinct
origins. `sync/durability.go` and `syncproto/syncproto.go` cite
"SAFETY-AUDIT.md D1" at the relevant call sites, so the documented decision
is anchored in the code rather than only here.

The accepted trust has since been narrowed twice by round two without
changing the model: pulls are scoped to the volume's accepted destinations
([#123](https://github.com/mbertschler/squirrel/pull/123)) and pulled
components are tagged with their asserting peer, making revocation possible
([#133](https://github.com/mbertschler/squirrel/pull/133)).

**Issue:** `durability: document the relayed-evidence trust boundary; add verify-method allow-list and origin-node cap as defence-in-depth`
→ shipped in [#128](https://github.com/mbertschler/squirrel/pull/128)

### D2: Content-addressed offsite push proves presence+size, not decrypt-correctness

**Severity:** Medium • **Likelihood:** Low (requires a transfer-time
corruption that preserves decrypted size, or the documented
re-hash→read TOCTOU window to fire).

**Where**

- `sync/content_addressed.go` — `uploadOneObject` (re-hash → `copyTo` →
  `statRemote` size check), `captureFingerprints`.
- `sync/verify_remote.go` — `VerifyRemote` (scan-back re-check).

**What it does / doesn't establish**

At upload the push: (1) re-hashes the **local plaintext** and refuses on
drift, so the encryption input is the right content; (2) confirms the
object is **present** and its **decrypted size** (stat is through the
crypt overlay) matches the index; (3) records the provider's checksum of
the ciphertext as the scan-back baseline. The underlying backend's own
transfer integrity (e.g. S3 Content-MD5 on PUT) covers "the ciphertext
rclone sent is the ciphertext stored."

It does **not** confirm that the stored ciphertext *decrypts back* to the
indexed hash — there is no post-upload decrypt-and-rehash. The
unguarded slivers are the documented fork/exec window between the
re-hash and rclone's open (`uploadOneObject` "Residual:" comment) and a
hypothetical crypt bug that produces a right-decrypted-size, wrong
content object. Ongoing bitrot is caught by the scan-back re-verify; a
*wrong-at-upload* object is the gap.

**Proposed mitigation (opt-in, NAS-local — sketch, not built):**

- Add a `--verify` mode to the content-addressed push that, after an
  object lands, downloads it back **through the crypt overlay**,
  BLAKE3s the plaintext, and compares to the indexed hash. This is the
  only check that closes decrypt-correctness, and it lives entirely on
  the pushing node (which holds the plaintext) with no protocol or
  laptop change.
- Scope it to the **initial upload** of each content hash (or a sampled
  subset), not every run — the object is append-only and immutable, so
  one read-back per object is sufficient. Cost is one download per
  verified object (egress), so it must be opt-in and never run against
  cold-tier targets (e.g. Glacier Deep Archive, where a read needs a
  restore).
- Tightening the re-hash→read TOCTOU window further (snapshot/lock the
  source) is **not** recommended — the window is one fork/exec, the
  indexer and scrub already surface drift, and chasing it is
  disproportionate.

**Acceptance**

- With `--verify`, a seeded object whose stored ciphertext decrypts to
  the wrong content is caught and the run fails before the durability
  vector advances; without it, behaviour is unchanged.

**Status:** Open by design — the sketch was never built, and the gap it
describes has narrowed rather than closed.

What did land is the scan-back fingerprint
([#116](https://github.com/mbertschler/squirrel/pull/116),
[#137](https://github.com/mbertschler/squirrel/pull/137) for multipart S3
ETags): the provider's checksum of the stored *ciphertext* is captured at
upload and re-confirmed by `squirrel verify` on the agent's `verify_every`
cadence. Round two then made the offload gate honest about what that
proves — `presence+size` is explicitly **not** content-verified, and
`fingerprint-verified` counts only while a verify cadence keeps it fresh
([#109](https://github.com/mbertschler/squirrel/issues/109),
`store.ContentVerifiedMethod`). So the *consequence* the finding worried
about — a presence-only advance gating an offload — is gone.

The narrow original gap remains: nothing downloads an object back through
the crypt overlay and BLAKE3s the plaintext, so a wrong-at-upload object
that happens to preserve decrypted size is still theoretically undetected.
No `--verify` mode exists. This stays a deliberate non-decision rather than
an oversight; the cost (one egress download per object, impossible against
cold tiers) is the reason, and it is unchanged.

**Issue:** `sync: optional read-back-decrypt-rehash verification for content-addressed uploads`
→ **not built**; no tracking issue filed

---

## Round two — the offload-v1 audit (#103–#118)

A second audit ran in June 2026 against the offload-v1 surface: the
durability version vectors that gate `offload`, the peer durability pull,
the content-addressed and packed offsite push, kopia verification, and the
agent's network boundary. Its findings were filed straight as issues
prefixed `[critical]` / `[high]` / `[medium]` / `[low]` and never written
up here.

**Decision: recorded as a separate round, not folded into the C/H/M/L
numbering above.** Three reasons, in order of weight:

1. **The round-one identifiers are load-bearing.** `H6`, `M2`, and `D1` are
   cited in code comments (`store/peer_sync.go`, `store/runs.go`,
   `cmd/squirrel/audit.go`, `sync/durability.go`, `syncproto/syncproto.go`),
   in commit messages, and in PR titles. Renumbering to interleave a second
   round would break every one of those references for no gain.
2. **The two rounds audited different codebases.** Round one predates
   offload-v1 entirely — durability vectors, the content-addressed layout,
   and the peer durability pull did not exist when it was written. A single
   merged severity ranking would imply a coherence across fourteen months
   of code that never existed.
3. **Round two is already fully discharged**, so folding it in would import
   fourteen closed items into a document whose value is telling a reader
   what is left.

The table below is the index, not a write-up: each issue is closed and its
GitHub thread carries the detail. Round two found no finding that
contradicts or reopens a round-one one; where the two touch the same
surface, the round-one Status notes above say so and link across (C2↔#106,
C3↔#111/#112, H5↔#112, M4↔#105, D1↔#104, D2↔#109).

| Issue | Finding | Closed by |
|---|---|---|
| [#103](https://github.com/mbertschler/squirrel/issues/103) | **[critical]** Durability vector over-advances from the live index after a push | [#120](https://github.com/mbertschler/squirrel/pull/120) — snapshot-pinned close-phase advance |
| [#104](https://github.com/mbertschler/squirrel/issues/104) | **[critical]** Durability pull merges unscoped, unprovenanced peer-asserted components | [#123](https://github.com/mbertschler/squirrel/pull/123) (scoping) + [#133](https://github.com/mbertschler/squirrel/pull/133) (asserting-peer tag) |
| [#105](https://github.com/mbertschler/squirrel/issues/105) | **[critical]** Peer can self-attribute content origin, poisoning the durability vector | [#119](https://github.com/mbertschler/squirrel/pull/119) |
| [#115](https://github.com/mbertschler/squirrel/issues/115) | **[critical]** Offload gate unsound for re-acquired content — needs a local-run freshness condition | [#120](https://github.com/mbertschler/squirrel/pull/120) |
| [#106](https://github.com/mbertschler/squirrel/issues/106) | **[high]** Peer-sync `Transfer` overwrites un-indexed receiver files with no history; reserved-name gap | [#119](https://github.com/mbertschler/squirrel/pull/119) — the C2 gap on the rclone path |
| [#107](https://github.com/mbertschler/squirrel/issues/107) | **[high]** Content-addressed upload can permanently bind wrong bytes to a hash | [#124](https://github.com/mbertschler/squirrel/pull/124) |
| [#108](https://github.com/mbertschler/squirrel/issues/108) | **[high]** kopia verification overclaims scope and depth | [#120](https://github.com/mbertschler/squirrel/pull/120) |
| [#109](https://github.com/mbertschler/squirrel/issues/109) | **[medium]** presence+size advances gate offload without content verification | [#116](https://github.com/mbertschler/squirrel/pull/116) (scan-back fingerprints) + [#178](https://github.com/mbertschler/squirrel/pull/178) (gate-side upgrade) |
| [#110](https://github.com/mbertschler/squirrel/issues/110) | **[medium]** Agent network hardening: session binding, placeholder upgrade, body cap, token scope | [#119](https://github.com/mbertschler/squirrel/pull/119) (a–c) + [#132](https://github.com/mbertschler/squirrel/pull/132) (session-caller binding) |
| [#111](https://github.com/mbertschler/squirrel/issues/111) | **[medium]** `db restore` destroys the live index with no undo; stale-WAL crash window | [#117](https://github.com/mbertschler/squirrel/pull/117) |
| [#112](https://github.com/mbertschler/squirrel/issues/112) | **[medium]** Pre-migration snapshots rotated away by routine sync rotation | [#117](https://github.com/mbertschler/squirrel/pull/117) |
| [#118](https://github.com/mbertschler/squirrel/issues/118) | **[medium]** Confirm/repair S3 multipart ETag capture for scan-back fingerprints | [#137](https://github.com/mbertschler/squirrel/pull/137) |
| [#113](https://github.com/mbertschler/squirrel/issues/113) | **[low]** Index/migration integrity: contents immutability trigger, v14 size guard, stat-after-hash | [#125](https://github.com/mbertschler/squirrel/pull/125) + [#135](https://github.com/mbertschler/squirrel/pull/135) |
| [#114](https://github.com/mbertschler/squirrel/issues/114) | **[low]** Robustness cluster: run-gate asymmetry, hook finish guard, blake3 degrade, path check, kopia re-create | [#125](https://github.com/mbertschler/squirrel/pull/125) |

Note on the range: the round is **#103–#115 plus #118** — fourteen issues.
#116 and #117 are pull requests in the same series, not findings, and #103
through #118 is not a contiguous block of issues.

The D1 and D2 entries in the section above belong to this era too — they
came out of [#128](https://github.com/mbertschler/squirrel/pull/128), the
offload-v1 trust-hardening pass — but they were written up in this
document's voice at the time, so they stay where they are.

---

## Cross-cutting recommendations

**Status of this section:** the documentation asks are essentially done;
the test-suite asks were satisfied per-finding rather than as the named
suites; the process asks were not adopted.

- *Tests.* Every finding that shipped carries its own regression test — the
  second process bullet held. The three named cross-cutting **suites** were
  not built as such: preservation is asserted per code path (restore,
  CopyFromExisting, Transfer, supersede, conflict) rather than by one
  suite that enumerates them, and there is no standing concurrency-stress
  harness beyond the targeted races in `store/store_test.go`,
  `index/index_test.go`, and `agent/dispatcher_test.go`. An enumerating
  suite would still have value: it is the only thing that would catch a
  *new* overwrite path added later, which is exactly how the `Transfer`
  gap (#106) survived C2.
- *Documentation.* Both asks landed via the docs site rather than a
  `docs/safety.md`: reserved directory names and the marker file are in
  `docs/src/content/docs/reference/on-disk-layout.md`, and the
  corrupted-DB playbook is in
  `docs/src/content/docs/guides/recovery.md`, pointing at `db check`,
  `db backup`, and the `.squirrel-index/` ride-along. Only the Litestream
  half is missing — the same gap as C3 layer 3.
- *Process.* No "safety review" label exists. Given that round two found
  four Critical issues in exactly these files, this one is worth
  revisiting rather than quietly dropping.

### Tests we should add now

- A "data preservation" test suite that asserts: every code path that
  could overwrite or delete a file has a destination history entry
  (or a marker indicating the user opted in).
- A "metadata preservation" test suite that asserts: every code path
  that updates a row leaves a recoverable trail.
- A "concurrency stress" harness that runs index + sync + audit in
  parallel against the same volume and asserts the index ends up
  consistent.

### Documentation we should add

- A short `docs/safety.md` (or extend `README.md`) listing every
  reserved directory name, the marker file, the backup story, and
  the philosophy of "deletion is a deliberate multi-step action".
- A migration playbook: "what to do if your DB is corrupted",
  pointing at `db check`, `db backup`, the per-sync snapshot at
  `<dest>/<volume>/.squirrel-index/`, and Litestream.

### Process

- Tag every PR that touches `store/`, `agent/sync.go`,
  `sync/sync.go`, or `sync/node.go` with a "safety review" label so
  these areas get extra scrutiny.
- Require a regression test for every fix landing from this audit;
  the tests are the durable check.

---

## Open question — answered

Should the safety improvements be opt-in (preserving today's defaults
for users who already trust the existing behaviour) or opt-out
(flipping defaults to safer-but-pickier behaviour and asking the user
to bypass when they want speed)? My recommendation: opt-out for the
critical findings (#C1, #C2, #C4) and opt-in for the medium ones
(#M1, #M5). #C3 (backups) is a pure addition with no backward
compatibility surface.

**Resolved, and the recommendation was followed — then exceeded.** C1
ships safe-by-default with `--in-place` as the bypass; C2 and C4 have no
bypass at all, because preserving bytes and refusing a zero run-id cost
nothing to leave always-on; C3 landed as a pure addition as predicted. M1
and M5 both ended up unconditional rather than opt-in: the shallow warning
always prints, and the rclone.conf write is always content-compared,
because neither has a "want speed" case worth a flag.

The pattern that emerged is stronger than either option in the question:
**safety defaults are unconditional, and the opt-in is always the
bypass**, named for the unsafe act it permits (`--in-place`,
`--allow-rewind`, `--init`, `destination reset --yes`). That is now
codified as principle 2 in
[`design/ux-principles.md`](design/ux-principles.md#2-the-cli-is-for-change-and-for-questions--never-for-operations)
— the change family is the deliberate, weighty, typed one — so this
question no longer needs to be re-asked per finding.

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
issue we should open for that item.

### Critical

1. [#C1](#c1-restore-overwrites-the-local-volume-without-history) —
   `restore` overwrites the local volume in place with no
   `--backup-dir`, no confirmation, no marker check.
2. [#C2](#c2-copy-from-existing-pre-stage-can-clobber-out-of-band-files)
   — receiver's CopyFromExisting pre-stage uses `os.Rename` over the
   destination path; an out-of-band file there is overwritten without
   being moved to history.
3. [#C3](#c3-no-backup-of-the-sqlite-index) — there is no built-in
   backup, snapshot, or Litestream story for `~/.squirrel/index.db`. A
   single disk failure loses the entire index.
4. [#C4](#c4-sync-runid-fallback-collides-history-into-one-bucket) —
   `backupDirURI` falls back to `run-dry-run/` when `runID == 0` outside
   dry-run mode; any future bug that calls into that branch silently
   merges all overwritten history into one directory.

### High

5. [#H1](#h1-cross-process-index-run-race-corrupts-the-live-set) —
   `index` has no atomic begin-if-clear gate; two concurrent indexers
   (CLI + agent scheduler) can both finish, and the loser's MarkMissing
   will flip the winner's freshly-touched rows to `missing`.
6. [#H2](#h2-finishrun-blindly-overwrites-terminal-state) — `FinishRun`
   accepts a transition from any status, so a double-finish bug or a
   buggy retry overwrites the original terminal status, error message,
   and timestamp.
7. [#H3](#h3-runs-fail-erases-the-original-end-state-without-an-audit-line)
   — the manual recovery command (`runs fail`) flips a stuck `running`
   row to `failed` with no record that this was an operator action
   rather than a real failure, and clobbers any partial `file_count`.
8. [#H4](#h4-toctou-between-classify-and-pre-stage-can-misattribute-bytes)
   — receiver classifies on the index, then pre-moves on disk; if the
   on-disk content drifted out-of-band between those steps, the
   `.squirrel-history/run-N/` payload no longer matches the recorded
   blake3.
9. [#H5](#h5-no-pre-migration-snapshot-of-the-index) — schema
    migrations run in a transaction (good) but there is no on-disk
    snapshot taken before the migration starts. A corruption or
    in-flight crash leaves no point-in-time backup to fall back to.
10. [#H6](#h6-the-watermark-and-correlated-run-id-are-overwrite-in-place)
    — `UpsertPeerSyncState` and `SetCorrelatedRunID` rewrite values
    with no audit row. If a bug or hostile peer pushes a bad
    watermark, the prior watermark is lost.
11. [#H7](#h7-destination-root-and-restore-target-are-not-marker-guarded)
    — sync writes into `<dest.root>/<volume>/` and restore writes into
    `vol.Path`; nothing validates that those locations are the squirrel-
    owned tree we think they are.

### Medium

14. [#M1](#m1-shallow-mode-on-bucket-sync-can-skip-a-divergent-destination)
    — `--shallow` drops `--checksum --hash blake3`; a destination that
    drifted out-of-band but kept the same (size, mtime) silently stays
    divergent.
15. [#M2](#m2-folder-merkle-hashes-have-no-self-check) — there is no
    audit subcommand that re-derives every folder's deep_blake3 from
    its children and compares to what's stored.
16. [#M3](#m3-no-pragma-integrity_check-anywhere) — the sqlite file is
    only verified lazily, page-by-page, when a row is read. A silent
    corruption can sit undetected for months.
17. [#M4](#m4-source_node_id-attribution-is-overwritten-on-touch) —
    when an unchanged-content row is touched on a different sync's
    closeSession or by the indexer, `updateLiveRow` rewrites
    `source_node_id` / `source_run_id`, losing the prior attribution.
18. [#M5](#m5-rclone-config-is-the-only-place-secrets-get-resolved-then-re-resolved)
    — `WriteRcloneConfig` is rewritten on every sync. There's no
    "diff against last-written" guard, so a buggy resolver could
    silently regress a secret.
19. [#M6](#m6-tempfile-write-in-copyfromexisting-lacks-fsync) —
    `copyFileToPath` closes and renames the tempfile without
    `f.Sync()`, so a system crash mid-pre-stage can leave a zero-byte
    file in place after rename; verify catches it but the prior bytes
    may already be elsewhere.
20. [#M7](#m7-orphan-volume-warning-is-only-a-warning) — a volume
    declared in the DB but missing from config logs once at every
    command invocation; nothing escalates it or refuses commands until
    the operator confirms.
21. [#M8](#m8-runs-table-has-no-retention-policy-flag) — runs grow
    forever, which is fine; but the lack of any explicit retention
    command means there's no documented stance against eventual
    pruning. We should state the policy ("never prune unless `runs
    prune` is run explicitly") and add the no-op command as a
    placeholder.

### Low

22. [#L1](#l1-buildDSN-collapses-on-trailing-spaces-in-path) — `'?'`
    and `'#'` are rejected but path normalisation doesn't catch
    trailing whitespace or unicode lookalikes.
23. [#L2](#l2-hostname-fallback-can-silently-produce-an-empty-node-name-on-edge-platforms)
    — `sanitiseNodeName` could in theory yield `""` on hostnames with
    no alnum characters; we then error, but there's no fallback to a
    UUID-based name.
24. [#L3](#l3-no-pre-flight-rclone-version-check-for-restore-from-node)
    — restoreFromNode isn't yet implemented, but when it lands we need
    the same `EnsureMinVersion` gate the bucket path has.

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

**Issue:** `restore: gate in-place overwrites behind --in-place and write a local history dir`

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

**Issue:** `agent: CopyFromExisting pre-stage must preserve any out-of-band file at the destination path`

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

**Issue:** `store: add db backup / restore / online snapshot and document Litestream integration`

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

**Issue:** `sync: refuse to build rclone args with runID=0 outside dry-run`

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

**Issue:** `store: add BeginIndexRunIfClear and wire it through index/audit paths`

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

**Issue:** `store: FinishRun must refuse to overwrite a terminal-status row`

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

**Issue:** `runs: distinguish manual-fail from real-fail in the audit log`

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

**Issue:** `agent: re-hash on-disk bytes before supersede/conflict pre-stage moves`

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

**Issue:** `migrations: snapshot the index to ~/.squirrel/backups before each upgrade`

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

**Issue:** `store: keep watermark and correlation history append-only`

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

**Issue:** `sync: require .squirrel-volume markers on destination and source to gate against misconfiguration`

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

**Issue:** `sync: surface --shallow trade-off in logs and persist it on the runs row`

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

**Issue:** `audit: self-check folder Merkle hashes against the live row set`

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

**Issue:** `cli, store: add db check command running PRAGMA integrity_check`

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

**Issue:** `store: preserve first-seen provenance on files independently from current attribution`

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

**Issue:** `sync: only rewrite rclone.conf when its content actually changes`

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

**Issue:** `agent: fsync the CopyFromExisting tempfile before atomic rename`

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

**Issue:** `cli: escalate orphan-volume warnings into a refuse-to-run gate with an explicit acknowledgement`

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

**Issue:** `runs: codify the "never auto-prune" policy and add a placeholder retention command`

---

## Low findings — detail

### L1: buildDSN collapses on trailing spaces in path

**Where**: `store/store.go:67-78`.

**Mitigation**: After rejecting `?` and `#`, also reject paths with
leading/trailing whitespace or NUL bytes. Tighten with a regex.

**Issue:** `store: validateDBPath should reject trailing whitespace and NUL`

---

### L2: hostname fallback can silently produce an empty node name on edge platforms

**Where**: `store/store.go:119-162`.

**Mitigation**: When `sanitiseNodeName` returns "", generate a
deterministic fallback (e.g. hash of MAC or content of
`/etc/machine-id`) instead of erroring.

**Issue:** `store: deterministic fallback node name when hostname sanitises to empty`

---

### L3: no pre-flight rclone-version check for restore-from-node

**Where**: future feature; placeholder.

**Mitigation**: Track in the issue tracker so the gate isn't forgotten
when restoreFromNode lands.

**Issue:** `restore: ensure EnsureMinVersion runs in the restore-from-node path when implemented`

---

## Cross-cutting recommendations

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

## Open question

Should the safety improvements be opt-in (preserving today's defaults
for users who already trust the existing behaviour) or opt-out
(flipping defaults to safer-but-pickier behaviour and asking the user
to bypass when they want speed)? My recommendation: opt-out for the
critical findings (#C1, #C2, #C4) and opt-in for the medium ones
(#M1, #M5). #C3 (backups) is a pure addition with no backward
compatibility surface.

# Native mirror layout: one planner, three layouts, a small transport

*Proposed, not adopted.* This is step 2 of replacing rclone with squirrel's own
transports. Peer sync dropped rclone in #210. #211 made the mirror's evidence
honest. Step 3 is S3 for the packed and content-addressed layouts.

## Summary

Today a mirror push hands the whole job to `rclone copy`. Rclone lists both
sides, compares them, transfers what differs, and moves overwritten files into
`--backup-dir`. This design replaces that for `local` mirrors and plain `sftp`
mirrors. The replacement has three layers:

1. **Planner**, shared by every layout: the index changes since the
   destination's last confirmed run.
2. **Layout**: turns those changes into operations. Content-addressed uploads
   an object per hash. Packed adds small content to a pack. Mirror writes the
   path and first moves the previous version into history.
3. **Transport**: byte-level access to one destination root (stat, list, get,
   put, rename). There is a `local` implementation and an `sftp` one.

The planner lands first. It is a refactor of the content-addressed and packed
handlers, which already share this logic in duplicated form. It changes no
behaviour except a bug fix it exposes (see "Watermark rule"). The mirror then
plugs in as a third layout.

## Scope

**In scope:**

- the planner refactor;
- the transport, with its local and sftp implementations;
- the mirror layout on `local`, and on `sftp` without `crypt`;
- marker, snapshot ride-along, recover and restore for those destinations.

**Out of scope, and still on rclone:**

- **Crypt mirrors, including cloudbox.** They wait for the crypt
  byte-compatibility decision (open question 1). In this design, crypt is a
  wrapper around a transport. So that decision changes neither the planner nor
  the layout.
- **Mirrors on s3, b2 and gcs.** A mirror needs rename, and object stores have
  none (see "Transport").
- **Peer sync.** It keeps its negotiated plan, where the receiver decides.
- **Kopia.**

In the [reference setup](reference-setup.md), `usb` moves with this work.
`cloudbox` is an encrypted sftp mirror, so it moves when crypt does.

## What changes for the operator

- **A push plans from the index.** It sends what the index recorded, not what
  a live walk finds. This already holds for the content-addressed and packed
  layouts. A file created after the last index run reaches the mirror on the
  first push after it gets indexed.
- **A push checks only the paths it plans to write.** Each one gets a single
  `Lstat`. Anything found at such a path is moved into `.squirrel-history/`
  before the new version lands, whatever it holds. So a remote change can cost
  an extra copy in history, never an overwrite. Changes elsewhere on the
  destination are found by the verify pass (`verify_every`, extended to
  mirrors in the evidence step).
- **Each push leaves a receipt** at `<volume>/.squirrel-index/run-<id>`. It is
  the same manifest segment the content-addressed and packed layouts write
  (see [formats](../docs/src/content/docs/reference/formats.md)). The mirror
  stays browsable. The receipts make it verifiable without the index.
- **New reserved directory** `<volume>/.squirrel-staging/` holds in-flight
  writes.
- **sftp host keys are always verified** (see "Configuration").
- **`--shallow` is refused for a native mirror.** A push decides from
  squirrel's records, confirms each planned path's size and mtime, and moves
  whatever is there aside before writing. Its safety comes from that move, not
  from a content comparison. Content is checked by hashing the source as it
  streams, by the read-back in the evidence step, and by the verify pass.
  None of those is optional, so the flag has nothing to switch off.

## 1. The planner

`contentAddressedHandler.Push` and `packedHandler.Push` each carry the same
sequence:

1. indexed-volume check;
2. markers;
3. run allocation;
4. watermark;
5. advance snapshot;
6. delta;
7. finish;
8. ride-along.

They differ only in their landing evidence (segment or placement map) and in
what they transfer. The planner is that shared sequence, extracted once:

```go
// pushPlan is what one push must land: the volume's index changes since the
// destination's last confirmed run, and the durability snapshot the push
// advances to once they land.
type pushPlan struct {
	volumeID  int64
	watermark int64                   // last confirmed run; 0 for a fresh destination
	delta     []store.PathDelta       // ListPathDeltaSince(volumeID, watermark)
	advance   []store.OriginComponent // captured before any transfer
}

// layout is one destination layout's part of a push.
type layout interface {
	// landed reports whether runID left this layout's landing evidence.
	landed(ctx context.Context, runID int64) (bool, error)
	// translate turns the plan into this layout's operations. It reads
	// squirrel's records and writes nothing: a dry run is translate alone.
	translate(ctx context.Context, p pushPlan) (operations, error)
	// execute performs the operations and records each confirmed one.
	execute(ctx context.Context, rep *Report, runID int64, ops operations) error
	// seal writes the run's landing evidence once every operation is confirmed.
	seal(ctx context.Context, runID int64, p pushPlan) error
	// advanceMethod names the evidence the confirmed landing earns.
	advanceMethod(ctx context.Context, p pushPlan) (string, error)
}
```

One driver runs every layout:

    requireIndexedVolume → markers → begin run → plan → translate → execute → seal → advance → finish → ride-along

A dry run stops after translate and reports `operations`' summary. Today's
separate `previewDryRun` in each handler becomes that summary.

### Watermark rule

This rule is shared by all three layouts:

1. If there is no successful sync of this (volume, destination), the
   watermark is 0.
2. If the last success left its landing evidence at the destination, the
   watermark is that run's id.
3. The watermark is also 0 (a fresh start) when all three of these hold:
   - the landing evidence is absent;
   - the destination root is empty apart from its markers;
   - squirrel holds **no upload records** for the destination.
4. In every other case, refuse with `ErrRefused`. The message points at a
   fresh root or `squirrel destination reset`.

Each layout's landing evidence lives at a different place:

| Layout | Landing evidence |
|---|---|
| content-addressed | `<volume>/index/run-<id>` |
| packed | `packs/map-<id>` |
| mirror | `<volume>/.squirrel-index/run-<id>` |

No two layouts share a location, so a destination switched from one layout to
another is refused.

**The last condition in rule 3 is new, and it fixes a live bug.** Today
`freshStartOnEmptyRoot` checks only that the remote root is empty.
Consider a content-addressed destination whose root was wiped without a
`destination reset`, then re-marked with `--init`:

1. The next push takes the fresh-start branch.
2. `uploadObjects` skips every content that `remote_objects` still records.
3. It writes a segment mapping paths to objects that no longer exist.
4. The run closes as `success` and advances the durability vector. If the old
   fingerprints were verified, it advances as `fingerprint-verified`.

A throwaway test confirmed this. It ran one push, wiped the fake remote,
re-seeded the marker and ran a second push. The second push reported
`success` with `transferred=0 checked=1`, and the object was absent. Only a
later verify pass would notice. The planner has to fail closed here, because
the mirror's records would repeat the same mistake.

### Repairs: the planner's second input

This input arrives with the evidence step. A verify pass that finds a
recorded artifact missing or changed marks that record `lost`. The next
push then plans the record's content again, even though the index didn't
change:

    work = delta since watermark ∪ records marked lost

Today the content layouts only raise an alarm in this case. With repairs, the
same push that raised the alarm also heals it.

## 2. Layouts translate changes into operations

| Layout | Operations from one delta |
|---|---|
| content-addressed | one object upload per present content with no upload record on the destination |
| packed | object uploads for content at or above `pack_threshold`; pack members for the rest |
| mirror | one path write per present path whose live record doesn't hold that content |

Rows that are `missing`, `offloaded` or `superseded` produce no transfer in any
layout. A destination keeps content the source no longer has.

Translate reads only squirrel's own records. That makes each layout's
decisions table-testable without a transport. For the mirror, those decisions
include the destructive one: which existing entry gets moved into history.

## 3. Transport

```go
// transport is byte-level access to one destination root. Names are
// slash-separated and relative to the root; symlinks are never followed.
type transport interface {
	Stat(ctx context.Context, name string) (entry, error) // fs.ErrNotExist when absent
	List(ctx context.Context, dir string) ([]entry, error)
	Get(ctx context.Context, name string) (io.ReadCloser, error)
	// Put creates name exclusively (fs.ErrExist if present), streams r into
	// it, sets its mtime, and syncs it to stable storage before returning.
	Put(ctx context.Context, name string, r io.Reader, mtime time.Time) error
	// Rename moves from to to, creating to's parents; fs.ErrExist if to exists.
	Rename(ctx context.Context, from, to string) error
	Remove(ctx context.Context, name string) error
	Close() error
}
```

### Contract

- **Nothing replaces an existing name.** `Put` creates exclusively and
  `Rename` never replaces its target. So no transport call can overwrite
  bytes.
- **Only a name guard decides what may move or disappear.** Layouts never
  hold the raw transport. They get a wrapper whose one small function, with
  its own table test, allows:
  - `Remove` only on squirrel's own staging names from runs that have
    finished (`.squirrel-staging/run-<id>/<row>`), and on ride-along snapshots
    (`.squirrel-index/index-*.db`, which rotation already deletes today).
    Anything else found in staging is reported and left alone;
  - `Rename` only from staging to a live name (commit), or from a live name
    to `.squirrel-history/run-<current run>/` (displacement).

  That function is the whole audit surface for destroying or moving bytes on
  a destination.
- **Object stores implement everything except `Rename`.** S3 can create
  exclusively with a conditional put. That is enough for the
  content-addressed and packed layouts in step 3. A mirror needs `Rename`,
  which is why mirrors on s3, b2 and gcs are out of scope.

### Local implementation

- Built on `os.Root` (Go 1.25+). It resolves every name inside the root and
  refuses to follow symlinks out of it.
- `Put` opens with `O_CREATE|O_EXCL`, copies, sets the times, then syncs.
- `Rename` avoids replacing its target in one of two ways:
  - **Preferred:** the platform's no-replace rename (`renameat2` with
    `RENAME_NOREPLACE` on Linux, `renamex_np` with `RENAME_EXCL` on macOS),
    or `Link` followed by `Remove`.
  - **Fallback:** where the filesystem supports neither (exFAT, common on
    USB disks), squirrel checks with `Lstat` and then renames. A concurrent
    writer from outside squirrel could still slip in between the check and
    the rename. Squirrel can't collide with itself here, because the run guard
    and the volume marker already exclude a second squirrel writer.
- After a rename, the parent directories are synced.

### sftp implementation

- Built on `golang.org/x/crypto/ssh`, already a dependency, and
  `github.com/pkg/sftp`, which is new.
- `Put` opens with `SSH_FXF_CREAT|SSH_FXF_EXCL` and uses concurrent writes, so
  high latency doesn't cap throughput. It syncs through `fsync@openssh.com`
  when the server offers it.
- **mtime is whole seconds.** Protocol version 3 carries only seconds, so the
  records store the mtime the server reports.
- **`Rename` checks first.** The protocol's rename fails when the target
  exists. Servers don't all honour that, so the transport also runs `Lstat`
  first. The contract suite pins the behaviour against the testbed's server.
- **Nothing runs a program on the server.** The cloudbox shape allows none.

## 4. The mirror layout

### On disk

The tree keeps the shape it has today and gains two reserved entries:

```
<dest.root>/<volume>/
  2024/cat.jpg
  .squirrel-volume
  .squirrel-history/run-7/2024/cat.jpg   # prior version, moved aside by run 7
  .squirrel-index/run-7                  # receipt: run 7's manifest segment
  .squirrel-index/index-…-run-7.db       # ride-along index snapshot
  .squirrel-staging/                     # in-flight writes, squirrel-owned
```

`.squirrel-staging` joins the reserved names in `reservedSubtreeFilter`,
`isReservedSyncPath` and the walker's warning.

### Destination records

The mirror's counterpart of `remote_objects` is one row per version squirrel
wrote at a path. The row is keyed on the `files` row it came from:

```sql
CREATE TABLE remote_paths (
	id               INTEGER PRIMARY KEY,
	destination      TEXT    NOT NULL,
	folder_id        INTEGER NOT NULL,
	name             TEXT    NOT NULL,
	content_id       INTEGER NOT NULL,
	written_run_id   INTEGER NOT NULL REFERENCES runs(id),
	state            TEXT    NOT NULL
	                 CHECK (state IN ('committing', 'live', 'displacing', 'displaced', 'lost')),
	displaced_run_id INTEGER REFERENCES runs(id),
	mtime_ns         INTEGER NOT NULL,
	checksum_algo    TEXT,
	checksum         TEXT,
	verified_at_ns   INTEGER,
	FOREIGN KEY (folder_id, name, content_id) REFERENCES files (folder_id, name, content_id),
	CHECK (state NOT IN ('committing', 'live') OR displaced_run_id IS NULL),
	CHECK (state NOT IN ('displacing', 'displaced') OR displaced_run_id IS NOT NULL),
	CHECK ((checksum_algo IS NULL) = (checksum IS NULL))
) STRICT;
CREATE UNIQUE INDEX uniq_remote_paths_live ON remote_paths (destination, folder_id, name)
	WHERE state IN ('committing', 'live');
CREATE INDEX idx_remote_paths_unsettled ON remote_paths (destination)
	WHERE state IN ('committing', 'displacing');
```

- **A displaced version has a derived location.** It sits at
  `.squirrel-history/run-<displaced_run_id>/<path>`.
- **Directory moves keep that rule true.** When a directory is displaced
  because a file replaced it, every live row under the directory moves with it
  and gets the same `displaced_run_id`.
- **Rows are never deleted.** The one exception is `squirrel destination
  reset`, which clears `remote_paths` the same way it clears
  `remote_objects`.
- **Foreign files are preserved but not recorded.** Bytes squirrel didn't
  write have no content id. When squirrel displaces them, they are kept in
  history, reported as a run warning, and listed in a runs_audit note.
- **A row is `lost` when its bytes are no longer where it says.** A push or a
  verify pass found other bytes there, or nothing. The row keeps its history
  but stops vouching for its content.
- **The upload-once check sees mirror copies.** `ContentPresentOnDestination`
  gains `remote_paths` rows in the `live` and `displaced` states as a third
  source. `lost` rows never count.

### Translation rules

| Delta row | Live record for the path | Operation |
|---|---|---|
| present, content C | holds C | none (counted as already correct) |
| present, content C | holds other content | write C, displacing the recorded version |
| present, content C | none | write C, displacing any unrecorded entry found at the path |
| missing, offloaded, superseded | any | none |
| repair: a live record the verify pass marked `lost` | | write C, displacing whatever is there |

Execute confirms each planned path with a single `Lstat`, never a listing. If
an "already correct" path's size or mtime disagrees with its record, the path
is treated as changed behind squirrel's back: its row becomes `lost` and the path is written again, with a warning.

### Writing one path

1. **Reconcile** (once, at push start). Squirrel settles every `committing`
   and `displacing` row for this destination by checking both locations
   (next table). Staging left by finished runs is removed.
2. **Stage.** The source is streamed into `.squirrel-staging/run-<id>/<row>`,
   and BLAKE3 is computed over the same bytes. If the hash doesn't match the
   indexed hash, the staged file is removed and the run fails before its seal,
   so the watermark stays put. That is the `errContentDrift` contract. Here the
   hash covers exactly the bytes sent, which closes the re-hash-then-read gap
   noted on `uploadOneObject`.
3. **Displace.** If `Lstat` finds an entry at the path:
   - **The entry's size and mtime match the live row:**
     1. mark the row `displacing` with this run;
     2. rename the entry to `.squirrel-history/run-<id>/<path>`;
     3. mark the row `displaced`.
   - **They don't match** (the file changed behind squirrel's back):
     1. mark the row `lost`;
     2. displace the entry as unrecorded bytes, which a warning and a
        runs_audit note report.

     A record never claims history holds bytes it doesn't.
4. **Commit:**
   1. insert the new row as `committing`;
   2. rename the staged file to the path;
   3. mark the row `live`.

Each move is recorded before it happens. Reconcile then finishes the record or
withdraws it, based on where the bytes actually are. The destination never
lacks a copy of something it held. Displaced bytes reach history before the
new version is committed, and the new version stays in staging until then.
Between steps 3 and 4 the live path is briefly empty. That costs availability
until the next push, never content.

### Crash points

| Crash after | Destination | Reconcile at the next push |
|---|---|---|
| staging write | partial file in staging | removed with the finished run's staging |
| `displacing` recorded | old version still at the path | size matches the record, so the row goes back to `live` |
| displace rename | old version in history | history entry present, so the row becomes `displaced` |
| `committing` recorded | new version in staging, path empty | the intent is withdrawn, the staging file removed, and the path planned again (no seal, so it's still in the delta) |
| commit rename | new version at the path | staging gone and path present, so the row becomes `live` |
| every write, before the seal | tree complete, no receipt | the watermark holds, and the next push translates every path to "already correct" |

### What the tests must cover

A native mirror loses the independence rclone's own walk gave it (see "What we
give up"). So its tests for destroying and moving bytes carry more weight than
usual:

- **Crash injection at every transport call** of a push, using a
  fault-injecting transport wrapper, then a clean push. The invariants below
  must hold afterwards.
- **A model-based test.** It generates random index histories: add, modify,
  delete, re-add, file↔directory swaps, names that collide by case. It then
  pushes with random crash points and compares the results against a
  reference model.
- **File↔directory swaps**, in both directions and nested.
- **Unexpected entries at a target:**
  - an unrecorded file (a foreign write, or a tree rclone wrote);
  - a recorded file changed behind squirrel's back: it is displaced as
    unrecorded bytes, and its row becomes `lost`, never `displaced`;
  - a file placed in `.squirrel-staging/` from outside squirrel: it is
    reported and never removed;
  - a symlink anywhere along a target's parent chain, which must not be
    followed.
- **Case-insensitive destinations** (APFS or exFAT USB disks):
  - When `a.jpg` and `A.jpg` both exist at the source, the second write is
    refused for that path, and nothing is displaced.
  - Destinations that ignore Unicode normalization get the same treatment,
    for names that differ only in composed versus decomposed form.
  - The first push probes the destination's case and normalization behaviour
    inside staging.
- **Failures:**
  - the disk fills up or the quota runs out mid-write;
  - the history directory can't be written;
  - the source drifts while streaming.
- **Rename onto an existing target** must fail on both implementations and on
  the testbed's sftp server.

**Invariants:**

1. Every `live` row's bytes are at its path. Every `displaced` row's bytes are
   at its history path. In both cases their BLAKE3 matches.
2. Outside `.squirrel-staging/`, the set of files on the destination only
   grows.
3. After a clean push, every path present in the index has a `live` row
   holding its current content.

The suite runs against the local transport. It runs against the sftp
transport through an in-process `pkg/sftp` server. And it runs against both
wrapped in the fault injector.

## 5. Evidence and verification (reverses #216)

Until this step, a native mirror push advances its vector as `presence+size`.
Every fingerprint stays pending, so the mirror still can't gate offload.
That's the same as today, only the method name changes. This step changes it.

- **Read-back at write time.** After staging and syncing, squirrel re-reads
  the staged file and hashes it with BLAKE3 before committing:
  - **Local:** a cache-bypassing read (`POSIX_FADV_DONTNEED` on Linux,
    `F_NOCACHE` on macOS). It is best effort: a USB bridge's own cache is out
    of reach.
  - **sftp:** a download over the same session.

  A match records `checksum_algo = blake3` and `verified_at_ns` on the row.
  `CountVolumeContentsPendingFingerprint` and `ContentFingerprintVerified`
  consult `remote_paths`. So a push can advance as `fingerprint-verified`
  through the existing gate path, which requires a verify cadence.
- **Cost.** Read-back is a safety property, so it's always on. On a USB disk,
  every written byte is read once more, roughly 1.5 to 2 times the push time.
  On sftp, every uploaded byte is downloaded once.
- **Verify pass for mirrors.** `verify_every` becomes valid on native mirrors.
  Each pass does two things:
  - It checks the size and mtime of every `live` and `displaced` row with
    `Lstat`. Both states count as stored content, so both are checked. This is
    cheap and catches deletion, truncation and replacement.
  - It re-hashes a slice of rows, oldest `verified_at_ns` first, within a
    budget per pass. So every byte is re-read within a known period. Kopia's
    `verify_files_percent` is the precedent for sampled read-back.

  Findings latch the existing destination alarm and mark the affected rows
  `lost`. `lost` live rows whose content is still the index's current content
  become the planner's repairs.
- **Documents amended in the same PR:**
  - `CanEverGateOffload` becomes true for native mirrors, and
    `offload_requires` accepts them;
  - reference-setup.md's "Offload gate" paragraph;
  - guides/offloading.md and layouts/mirror.md;
  - the F21 entry in friction-log.md;
  - SAFETY-AUDIT.md.

## 6. Restore, ride-along, recover

- **Mirror restore with an index** uses the archive restore pipeline:
  1. resolve the present paths;
  2. `Get` each one by path;
  3. BLAKE3 while streaming;
  4. place it.

  Placing goes through a temporary file in the target directory, a sync and a
  rename. Today's archive restore writes with `O_TRUNC` in place. Both get
  fixed together.
- **Mirror restore without an index** (a fresh machine) walks the mirror with
  `List` and skips the reserved directories. When receipts are present, each
  file is checked against them, and files that can't be checked are counted.
- **The marker gate, the snapshot ride-along and its rotation, and `recover
  --from` discovery** become transport calls. That removes the rclone-specific
  helpers `statRemoteExists`, `catRemote`, `copyTo`, `listSnapshots`,
  `deleteFile` and `remoteRootEmpty` for these destinations.

## 7. Configuration and dispatch

- **Dispatch.** `HandlerFor` sends mirrors on `local`, and on `sftp` without
  `crypt`, to the native handler. Every other pair is unchanged.
- **sftp settings.** `host`, `port`, `user`, `password`, `key_file`,
  `known_hosts_file` and `host_key_algorithms` map onto `ssh.ClientConfig`.
  When there is neither password nor key file, squirrel uses ssh-agent, as
  rclone does.
- **Host key verification is always on.** Without `known_hosts_file`,
  squirrel reads `~/.ssh/known_hosts`. An unknown host is refused, and the
  message gives the presented fingerprint and how to trust it.
  - **Migration cost:** a config that relied on rclone accepting any host key
    stops connecting until the key is pinned.
- **Rclone-only keys are rejected** on native destinations: `checkers`, and
  `hash_algo` (the key for rclone's server-side hash). No key is added.
  Concurrency is fixed: a few parallel writes on sftp, and one or two on a
  local disk.

## 8. What we give up

- **Implementation independence.** Today rclone walks the source itself. A
  file the indexer misses still reaches cloudbox and usb. Once native, every
  mirror inherits indexer bugs, and kopia remains the only independent leg.
  - reference-setup.md already claims that every copy comes from the same
    walker. With a native mirror that becomes literally true. The
    kopia-leg section gets amended in the mirror PR to say what the mirrors
    no longer cover.
  - Open question 5 proposes a cheap mitigation.
- **Rclone's tuning and backend know-how.** Rclone brings parallel transfers,
  multi-threaded streams, and years of workarounds for sftp servers. The
  switch should wait for a benchmark on the testbed against rclone.
- **A new dependency:** `github.com/pkg/sftp`.

## 9. Order of work

Items 1 to 6 land together on one branch, in this order. The pull request's
checklist splits them into sections for separate sessions:

1. **Planner.** Extract `pushPlan` and the layout driver from the
   content-addressed and packed handlers, and fix the wiped-root rule. Nothing
   else changes. The existing tests pass untouched, and a new wiped-root test
   is added.
2. **Local.**
   - The transport, the name guard and the local implementation.
   - The fault-injecting wrapper and the contract suite.
   - The native mirror layout with `remote_paths`, and the `usb` push.
   - Amend reference-setup.md (the kopia leg, and the `usb` topology arrow)
     and layouts/mirror.md.
3. **sftp.** The sftp implementation, host key handling, and plain sftp
   mirrors.
4. **Restore, ride-along and recover** on the transport. Once this lands,
   `local` and plain `sftp` mirrors are free of rclone.
5. **Evidence.** Read-back fingerprints, the mirror verify pass, and repairs
   in the planner. This reverses #216 and amends the design and docs listed in
   section 5.
6. **Content-addressed and packed on `local` and `sftp`** move to the
   transport.
7. **S3 transport**: step 3 of the plan, on its own branch afterwards.

The crypt decision is independent of this order. Once made, it becomes a
crypt transport wrapper, and then cloudbox moves.

## 10. Open questions

1. **Crypt byte-compatibility with rclone.** This blocks cloudbox only.
   Undecided, and to be discussed separately.
2. **sftp and the offload gate.** Is a stat on every row, plus a rotating
   read-back, enough for an sftp mirror to gate offload? Or should an sftp
   mirror stay non-gating?
3. **Adopting an existing mirror tree.** Unrecorded entries are displaced
   into history, so pointing a native mirror at an rclone-era tree re-uploads
   everything. The alternative is an adopt pass that hashes existing files and
   records the ones that match. Recommendation: use fresh roots (nothing is in
   production), and build adoption only if the testbed shows the re-upload
   hurts.
4. **Receipt location.** Keep mirror receipts in `.squirrel-index/`. Or move
   every layout's segments there, which gives one landing-evidence location
   for all three.
5. **Regaining some independence.** A periodic source-completeness audit
   would compare a plain directory walk of each volume with the index's
   present set. It would catch indexer skips for every layout, not only
   mirrors.
6. **Case-folding collisions.** Refuse only the colliding path (as proposed),
   or refuse the whole destination for that volume?

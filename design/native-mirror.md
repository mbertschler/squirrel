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
  streams, by the read-back on local disks, and by the verify pass (both from the
  evidence step).
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

// layout is one destination layout's part of a push. O is the layout's
// own operations type; a dry run reports its preview.
type layout[O operations] interface {
	// markers gates the push on the destination's markers (writes them
	// under --init; a dry run only checks).
	markers(ctx context.Context, rep *Report, volumeID int64, opts Options) error
	// landed reports whether runID left this layout's landing evidence.
	landed(ctx context.Context, runID int64) (bool, error)
	// rootEmpty and foreignHistory complete the watermark rule below.
	rootEmpty(ctx context.Context) (bool, error)
	foreignHistory(runID int64) error
	// reconcile settles what an earlier push left in flight (mirror only).
	reconcile(ctx context.Context, rep *Report, volumeID, runID int64) error
	// translate turns the plan into this layout's operations. It reads
	// squirrel's records and writes nothing: a dry run is translate alone.
	translate(ctx context.Context, p pushPlan) (O, error)
	// execute performs the operations and records each confirmed one.
	execute(ctx context.Context, rep *Report, runID int64, ops O) error
	// seal writes the run's landing evidence once every operation is confirmed.
	seal(ctx context.Context, rep *Report, runID int64, p pushPlan, ops O) error
	// advanceMethod names the evidence the confirmed landing earns; an
	// empty method holds the vector (packed, while a fingerprint is pending).
	advanceMethod(ctx context.Context, rep *Report, p pushPlan) (string, error)
	// shelf is <volume>/.squirrel-index/ for the ride-along: reached
	// through rclone for the content layouts, through the transport for a
	// mirror.
	shelf(runID int64) snapshotShelf
}
```

One driver runs every layout:

    requireIndexedVolume → markers → begin run → reconcile → plan → translate → execute → seal → advance → finish → ride-along

A dry run stops after translate and reports the operations' preview. The
separate `previewDryRun` each handler carried became that preview
(`sync/push.go`).

The driver promotes a run to success only after the vector advanced, for
every layout. Before the refactor a packed run whose advance failed still
closed as success; it now closes as failed, like content-addressed.

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

A probe that fails, for the evidence or for the root's emptiness, fails the run
with its error instead: it can't tell either way, so it neither refuses nor
starts fresh.

Each layout's landing evidence lives at a different place:

| Layout | Landing evidence |
|---|---|
| content-addressed | `<volume>/index/run-<id>` |
| packed | `packs/map-<id>` |
| mirror | `<volume>/.squirrel-index/run-<id>` |

No two layouts share a location, so a destination switched from one layout to
another is refused.

**The last condition in rule 3 is new, and it fixes a live bug.** Before the
planner, `freshStartOnEmptyRoot` checked only that the remote root was empty.
Consider a content-addressed destination whose root was wiped without a
`destination reset`, then re-marked with `--init`:

1. The next push takes the fresh-start branch.
2. `uploadObjects` skips every content that `remote_objects` still records.
3. It writes a segment mapping paths to objects that no longer exist.
4. The run closes as `success` and advances the durability vector. If the old
   fingerprints were verified, it advances as `fingerprint-verified`.

`TestPushRefusesWipedRootWithUploadRecords` pins it. It runs one push, wipes
the fake remote, re-seeds the markers and runs a second push. Before the fix
that push reported `success` with `transferred=0 checked=1`, and the object
was absent; only a later verify pass would have noticed. The planner now fails
closed here (`DestinationHasUploadRecords`), because the mirror's records would
repeat the same mistake.

### Repairs: the mirror's second input

This input arrives with the evidence step. A verify pass that finds a
recorded copy missing or changed marks that record `lost`. The next push
then plans the record's content again, even though the index didn't change:

    work = delta since watermark ∪ records marked lost

The mirror's translate reads them (`ListRemotePathRepairs`): present paths
where a `lost` row holds the current content and no live row does. A
withdrawn commit is one too, but its path is still in the delta. A push
that writes repairs counts them as changed, so the runs fold shows it.

The content layouts keep only raising the alarm. Their records have no
`lost` state, and a changed object can't be healed in place: nothing
replaces an existing name, so repairing one needs a displacement the
content layouts don't have.

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
  - `Rename` only from this run's staging to a snapshot name (the
    ride-along) or to what the layout commits — a live name for a mirror; for
    a content layout `objects/<hex>`, `packs/<hex>`, `packs/map-<current run>`
    or `<volume>/index/run-<current run>` — or, for a mirror only, from a live
    name to `.squirrel-history/run-<current run>/` (displacement);
  - under `--init`, before the push holds a run, the marker's own staging:
    it is written to `.squirrel-staging/volume-marker` and renamed onto
    `.squirrel-volume`, and a stale staged copy may be removed.

  So the marker and the ride-along snapshot land whole or not at all, like
  every path: a write that fails halfway leaves its partial copy in staging,
  never at a name recovery or the marker gate reads.

  That function is the whole audit surface for destroying or moving bytes on
  a destination.
- **Object stores implement everything except `Rename`.** S3 can create
  exclusively with a conditional put. That is enough for the
  content-addressed and packed layouts in step 3. A mirror needs `Rename`,
  which is why mirrors on s3, b2 and gcs are out of scope.

### Local implementation

- Built on `os.Root` (Go 1.25+). It resolves every name inside the root and
  refuses to follow symlinks out of it. `os.Root` does follow symlinks that
  stay inside the root, so every call also checks its name's parent chain with
  `Lstat` and refuses a symlink anywhere along it (`sync/transport_local.go`).
- `Put` opens with `O_CREATE|O_EXCL`, copies, sets the times, then syncs.
- `Rename` avoids replacing its target in one of two ways:
  - **Preferred:** the platform's no-replace rename (`renameat2` with
    `RENAME_NOREPLACE` on Linux, `renamex_np` with `RENAME_EXCL` on macOS),
    called on the parent directories' handles.
  - **Fallback:** where the filesystem supports neither (exFAT, common on
    USB disks), and on every other platform, squirrel checks with `Lstat` and
    then renames. A concurrent
    writer from outside squirrel could still slip in between the check and
    the rename. Squirrel can't collide with itself here, because the run guard
    and the volume marker already exclude a second squirrel writer.
- After a rename, the parent directories are synced.
- **Every call is bounded by progress.** A call that makes none for
  `DefaultStallTimeout` (a `Put` progresses with every chunk it reads) fails the
  push. A local call blocked in the kernel can't be interrupted and may still
  finish a move later, so a push refuses while a call an earlier push gave up
  on is still outstanding (`sync/transport_stall.go`).

### sftp implementation

- Built on `golang.org/x/crypto/ssh`, already a dependency, and
  `github.com/pkg/sftp`, which is new (`sync/transport_sftp.go`).
- `Put` opens with `SSH_FXF_CREAT|SSH_FXF_EXCL` and uses concurrent writes, so
  high latency doesn't cap throughput. It syncs through `fsync@openssh.com`
  when the server offers it.
- **mtime is whole seconds.** Protocol version 3 carries only seconds, so the
  records store the mtime the server reports.
- **`Put` and `Rename` check first.** The protocol's rename fails when the
  target exists, but servers don't all honour that: `pkg/sftp`'s own server and
  `rclone serve sftp` both replace it (`TestSFTPServersRenameOverAnExistingName`). Version 3 also has no
  "already exists" status, so an exclusive create that fails can't say why. So
  both calls run `Lstat` on their target first and fail with `fs.ErrExist`,
  and every call checks its name's parent chain for symlinks, as the local
  transport does. The contract suite passes against an in-process `pkg/sftp`
  server and against `rclone serve sftp`, the testbed's server. That server
  hides symlinks, so the symlink cases skip there.
- **Host keys follow known_hosts.** The handshake asks the server for the key
  types known_hosts pins for it (or `host_key_algorithms` when set), so a
  server that also offers another key type is still checked against the
  pinned one (`sync/transport_sftp_hostkey.go`).
- **One optional server-side command: a hash.** Content-addressed and packed
  artifacts get their fingerprint confirmed, and re-confirmed, by a hash
  command run on the server (`ServerHash`, `sync/transport_sftp_hash.go`).
  That is `sha256sum` by default, chosen by `hash_algo`: `md5sum`,
  `sha1sum`, `sha256sum` or `b3sum`, the hashes squirrel can also compute
  itself to compare against.
  - The command line is built only from the configured root and a name
    whose last element is a lowercase hex artifact name: an object or pack,
    or its staged copy `<volume>/.squirrel-staging/run-<id>/<hex>` (volume
    names hold only letters, digits, `_` and `-`). A root holding anything
    beyond letters, digits, `.`, `_`, `-` and `/` is never hashed on the
    server, and a relative one is passed as `./<root>` so it never reads as
    an option.
  - The transport probes for the command once per session, and a push opens
    one session: it hashes a known input on the server and compares. The leaf
    must be a regular file, so the command never follows a symlink. On a
    server that runs no programs, like the cloudbox shape, or whose command
    computes something else, fingerprints stay pending, as today.
  - Mirror paths are user filenames, so they never go on a command line.

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
- **The upload-once check leaves mirror copies out.** The plan was for
  `ContentPresentOnDestination` to gain `remote_paths` rows in the `live` and
  `displaced` states as a third source. Its only callers are the content
  layouts' upload-once checks, and a mirror copy isn't at `objects/<hash>`: a
  destination whose mirror records outlived a switch to a content layout would
  skip those objects and still seal. So the check stays with objects and packs,
  and the mirror decides from its own records. `DestinationHasUploadRecords`
  does count every `remote_paths` row.

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
2. **Stage.** The source is streamed into
   `.squirrel-staging/run-<id>/<path key>`, and BLAKE3 is computed over the same
   bytes. The path key is the lowercase hex BLAKE3 of the volume-relative path:
   the row doesn't exist yet (step 4 inserts it), and hex stays unique on a
   destination that folds case. If the hash doesn't match the indexed hash, the
   path fails and the run fails before its seal, so the watermark stays put. The
   staged file stays until the next push's reconcile removes it with the
   finished run's staging: the name guard removes no running run's staging. That is the `errContentDrift` contract. Here the
   hash covers exactly the bytes sent, which closes the re-hash-then-read gap
   noted on `uploadOneObject`.
3. **Displace.** First any parent of the path that isn't a directory (a
   file↔directory swap, a symlink) is displaced the same way. Then, if `Lstat`
   finds an entry at the path:
   - **The entry's size and mtime match the live row:**
     1. mark the row `displacing` with this run;
     2. rename the entry to `.squirrel-history/run-<id>/<path>`;
     3. mark the row `displaced`.
   - **They don't match** (the file changed behind squirrel's back):
     1. mark the row `lost`;
     2. displace the entry as unrecorded bytes, which a warning and a
        runs_audit note report.

     A record never claims history holds bytes it doesn't.
   - **It is a directory** a file replaced: every live row under it becomes
     `displacing`, the directory moves, and they become `displaced`; a row whose
     bytes changed becomes `lost` first.
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
| `committing` recorded | new version in staging, path empty | the intent is withdrawn (the row becomes `lost`: its version never reached the path), the staging file removed, and the path planned again (no seal, so it's still in the delta) |
| commit rename | new version at the path | staging gone and path present, so the row becomes `live` |
| every write, before the seal | tree complete, no receipt | the watermark holds, and the next push translates every path to "already correct" |

`TestMirrorCrashTable` runs each row above; `TestMirrorTranslationRules` and
`TestMirrorWriteDisplacesWhatThePathHolds` run the translation table.

A native push reports as `already_correct` every path whose live row holds its
present content, planned or not, so an unchanged push still reads as in sync
(friction F7).

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
That's the same as today, only the method name changes. This step changes it
for local mirrors.

- **Read-back at write time, on local disks.** After staging and syncing,
  squirrel re-reads the staged file and hashes it with BLAKE3 before
  committing (`sync/readback.go`). The read bypasses the cache: on Linux the
  synced pages are dropped (`POSIX_FADV_DONTNEED`) before the read, and on
  macOS both the write and the read run with `F_NOCACHE`, because a read
  there is still served from pages the write left cached. It is best
  effort: a USB bridge's own cache is out of reach. A mismatch fails the
  path before its commit, and the staged copy goes with the run's staging.

  A match records `checksum_algo = blake3` and `verified_at_ns` on the row.
  `CountVolumeContentsPendingFingerprint` and `ContentFingerprintVerified`
  consult `remote_paths` rows in the `live` and `displaced` states. So a push
  advances as `fingerprint-verified` once no present content is pending, as
  the content layouts do; the gate accepts that component through the
  existing path, which requires a verify cadence or a per-content
  fingerprint. Migration v33 indexes `remote_paths` by content for those
  per-content reads.
- **sftp mirrors stay non-gating.** An sftp push skips read-back, and mirror
  paths never go on a server command line, so their fingerprints stay
  pending.
- **Cost.** Read-back is a safety property, so it's always on for local
  disks. Every written byte is read once more, roughly 1.5 to 2 times the push
  time on a USB disk.
- **Verify pass for mirrors.** `verify_every` becomes valid on native mirrors.
  Each pass does two things:
  - It checks the size and mtime of every `live` and `displaced` row with
    `Lstat`. Both states count as stored content, so both are checked. This is
    cheap and catches deletion, truncation and replacement.
  - On local mirrors, it re-hashes a slice of rows, oldest `verified_at_ns`
    first (never-verified first of all), until the slice covers a tenth of the
    bytes the destination holds (`mirrorRereadShare`, `sync/verify_mirror.go`),
    and always at least one row. So every byte is re-read within about ten
    passes. Kopia's `verify_files_percent` is the precedent for sampled
    read-back.

  A pass first reads every volume's `.squirrel-volume` through the transport
  and fails, marking nothing, when one is missing or names another volume: an
  unmounted disk or an emptied share must not read as every copy gone.

  Findings latch the existing destination alarm, also when the pass aborts
  after recording them, and mark the affected rows `lost` — only while the row
  is still in the state the pass read it in, since a push may be moving it. A
  finding also demotes the volume's locally advanced `fingerprint-verified`
  components to `presence+size` (`DemoteFingerprintVerifiedVector`), at the
  runs they cover. Without that, a gate with a verify cadence would still take
  the component's word for a lost copy, and a peer pulling the vector would
  too; demoted, the gate checks each content's own fingerprint, and the next
  push that leaves nothing pending upgrades the component again. `lost` rows whose content is still the index's
  current content become the mirror's repairs. A clean pass re-attempts the
  `fingerprint-verified` upgrade, as it does for the content layouts.
  `squirrel verify` reaches the mirror through its transport, without rclone.
- **Documents amended in the same PR:**
  - `CanEverGateOffload` becomes true for native local mirrors, and
    `offload_requires` accepts them. sftp mirrors keep refusing, and the
    refusal names the reason;
  - reference-setup.md's "Offload gate" paragraph;
  - guides/offloading.md and layouts/mirror.md;
  - the F21 entry in friction-log.md;
  - SAFETY-AUDIT.md.

### The content layouts on the transport

Content-addressed and packed destinations on `local`, and on `sftp` without
`crypt`, are written through the transport too (`sync/artifacts_transport.go`).
The layouts land every object, pack, placement map and manifest segment
through one small interface, `artifactStore` (`sync/artifacts.go`), whose
other implementation is rclone's, for crypt destinations and for s3, b2 and
gcs. Each artifact:

1. is streamed into `<volume>/.squirrel-staging/run-<id>/<key>` while BLAKE3
   hashes the bytes sent — the drift check, over exactly those bytes — and, on
   sftp, the hash the server's command computes;
2. is confirmed before it gets its name: read back through BLAKE3 on a local
   disk, hashed by the server's command on sftp. A match is the artifact's
   fingerprint, recorded with its upload row at once; a mismatch fails the
   artifact. A server without the command confirms nothing, and the
   fingerprint stays pending, as it does under rclone for such a server;
3. is renamed onto its name. If the name already holds a file — a crash
   between landing and recording, or a failed run's orphan — that file is
   confirmed instead (downloaded and hashed when the server runs no command)
   and recorded, or the artifact fails. Squirrel never replaces it.

`squirrel verify` reads these destinations through the transport too
(`sync/verify_native.go`): once the marker of every volume that synced there
is in place, it lists `objects/` and `packs/` and re-reads every recorded
artifact — through BLAKE3, and through any other hash its row recorded, on a
local disk; through the server's hash command on sftp. An artifact the
server cannot hash this pass (no command, or a row recorded under another
`hash_algo`) is counted unchecked: neither re-confirmed nor a finding. On a
plain destination a BLAKE3 that differs from what the name says (an object's
content hash, a pack's key) is a mismatch, whichever backend reported it.

The probe runs once per session once the server has answered it; a session
that could not start the command asks again next time.

The staged copy of a failed artifact goes with the run's staging at the next
push's reconcile. The content layouts' guard commits only onto artifact names
and displaces nothing (section 3). Both native content layouts are gated on
the volume marker like a native mirror, `--init` creating a missing local
root; a missing root is otherwise refused like a missing marker. Before this,
config refused the content layouts on `local` outright, because rclone
addressed local destinations by path; that restriction is gone.

## 6. Restore, ride-along, recover

- **Mirror restore with an index** uses the archive restore pipeline
  (`sync/restore_mirror.go`):
  1. resolve the present paths;
  2. `Get` each one by path;
  3. BLAKE3 while streaming;
  4. place it.

  Placing goes through a temporary file in the target directory, a sync and a
  rename (`sync/restore_place.go`). Archive restore wrote with `O_TRUNC` in
  place; both now place the same way. Bytes that fail their hash never reach
  the path. A path that already holds its indexed bytes is left alone and
  counted as already correct, so an in-place restore moves into
  `.squirrel-restore-history/` only what it actually replaces.
- **Mirror restore without an index** (a fresh machine) walks the mirror with
  `List` and skips the reserved directories. "Without an index" means the index
  holds no present file for the volume: a fresh database, or one whose rows for
  the volume are all missing after a rescan of an emptied disk. The receipts,
  folded oldest run first, give each path the content its newest present entry
  recorded. A file with such an entry is checked against it and refused when it
  differs; a file no receipt names can't be checked, so it is restored and
  counted in a warning. A receipt that doesn't parse is reported and skipped.
- **`--shallow` changes nothing** on a native mirror restore, as on an archive
  restore: every byte is hashed anyway.
- **The marker gate, the snapshot ride-along and its rotation, and `recover
  --from` discovery** become transport calls. That removes the rclone-specific
  helpers `statRemoteExists`, `catRemote`, `copyTo`, `listSnapshots`,
  `deleteFile` and `remoteRootEmpty` for these destinations.

## 7. Configuration and dispatch

- **Dispatch.** `HandlerFor` sends every destination on `local`, and on
  `sftp` without `crypt`, through squirrel's own transport
  (`Destination.Native`): mirrors to the native mirror handler, the content
  layouts to their handlers with the transport's artifact store. Crypt
  destinations and s3, b2 and gcs keep rclone.
- **sftp settings.** `host`, `port`, `user`, `password`, `key_file`,
  `known_hosts_file` and `host_key_algorithms` map onto `ssh.ClientConfig`.
  When there is neither password nor key file, squirrel uses ssh-agent, as
  rclone does.
- **Host key verification is always on.** Without `known_hosts_file`,
  squirrel reads `~/.ssh/known_hosts`. An unknown host is refused, and the
  message gives the presented fingerprint and how to trust it.
  - **Migration cost:** a config that relied on rclone accepting any host key
    stops connecting until the key is pinned.
- **Rclone-only keys are rejected** on native destinations: `checkers`
  everywhere, and `hash_algo` on sftp mirrors. On content-addressed and packed
  sftp destinations, `hash_algo` still chooses the hash, which now names the
  command the transport runs on the server: `md5`, `sha1`, `sha256` (the
  default, now for packed as well) or `blake3`, the hashes squirrel also
  computes to check the server's answer. rclone's other hashes (`crc32`,
  `xxh3`, `xxh128`) stay valid behind crypt only. No key is added.
  Concurrency is fixed. On sftp each file goes out as concurrent write
  requests (`pkg/sftp`'s default of 64 in flight); paths are written one at a
  time on both transports. The testbed benchmark against rclone (section 8)
  decides whether several paths in flight are worth their bookkeeping.

## 8. What we give up

- **Implementation independence.** Today rclone walks the source itself. A
  file the indexer misses still reaches cloudbox and usb. Once native, every
  mirror inherits indexer bugs, and kopia remains the only independent leg.
  - reference-setup.md already claims that every copy comes from the same
    walker. With a native mirror that becomes literally true. The
    kopia-leg section gets amended in the mirror PR to say what the mirrors
    no longer cover.
  - Decision 7 keeps a cheap mitigation as a candidate.
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
5. **Evidence.** Read-back fingerprints on local mirrors, the mirror verify pass, and repairs
   in the planner. This reverses #216 and amends the design and docs listed in
   section 5.
6. **Content-addressed and packed on `local` and `sftp`** move to the
   transport.
7. **S3 transport**: step 3 of the plan, on its own branch afterwards.

The crypt decision is independent of this order. Once made, it becomes a
crypt transport wrapper, and then cloudbox moves.

## 10. Decisions and open questions

Decided on 2026-09-23:

1. **Offload gating on sftp.** Content-addressed and packed sftp destinations
   gate through the optional server hash command (section 3), as they do
   through rclone today. sftp mirrors stay non-gating, and so does any server
   that runs no programs.
2. **Host keys.** They are always verified against `known_hosts_file`, or
   against `~/.ssh/known_hosts` when that's unset. Unknown hosts are refused.
   Configs that relied on rclone accepting any key must pin it first.
3. **Read-back** runs on local disks only (section 5).
4. **Existing mirror trees aren't adopted.** The watermark rule refuses an
   rclone-era mirror: the tree isn't empty, and its runs left no receipts. A
   native mirror needs a fresh or emptied root. Adoption gets built only if the
   testbed shows that hurts.
5. **Receipts** for mirrors live in `.squirrel-index/`. The content layouts
   keep their segments where they are.
6. **Case-folding and normalization collisions** refuse only the colliding
   path.
7. **No source-completeness audit in this branch.** It stays a candidate. A
   periodic check would compare a plain directory walk with the index's
   present set, which would restore some independence for every layout.

Open:

1. **Crypt byte-compatibility with rclone.** This blocks cloudbox only, and is
   to be discussed separately.

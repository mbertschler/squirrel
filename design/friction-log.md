# Friction log

Findings from walking the [reference setup](reference-setup.md) on the
[testbed](testbed.md), lifecycle checkpoint by checkpoint, with real
commands. Entries are appended during the walk and closed (struck
through with a PR reference) as fixes land.

Severity scale:

- **S1** — data-risk or trust-breaking: violates a design principle or
  could mislead the operator about safety.
- **S2** — major friction: the flow works but fights the user; the
  kind of thing that makes someone abandon setup.
- **S3** — annoyance: extra steps, unclear output, missing conveniences.
- **S4** — polish: wording, formatting, defaults.

Walk metadata: squirrel @ branch `claude/open-source-comparison-zdbadt`,
rclone 1.74.1, SeaweedFS 3.80, kopia 0.21.1, testbed of 2026-07-23.

---

## Checkpoint 1 — bootstrap day

**F1 · S2 — TLS cert + fingerprint setup is entirely DIY.** Pinning is
the documented trust anchor for LAN agents, but there is no help
producing its ingredients: the operator must know the right openssl
incantations for cert+key *and* for the `sha256:<hex of DER>` pin
(`openssl x509 -outform DER | openssl dgst -sha256`). Neither README
nor any command covers this. A `squirrel agent cert` (generate) +
printed fingerprint at agent startup would remove a whole error class.

**F2 · S3 — crypt passwords must be pre-obscured with rclone by hand.**
Squirrel owns rclone.conf and hides rclone everywhere else, but crypt
config leaks the `rclone obscure` step (4 manual invocations for two
crypt destinations). Accepting plaintext-via-env and obscuring
internally would match the "squirrel owns the rclone surface" stance.

**F3 · S2 — the peer-token matrix is easy to wire wrong.** One
nas↔htpc relationship needs four token bindings across two files
(each side's `[agent.auth.peers.X]` entry must equal the *other*
side's `[nodes.Y].auth.bearer`). Nothing validates the pairing until
a sync fails at runtime with 401. Writing these four configs, the
cross-referencing was the single most error-prone part — and there is
no `squirrel node add` / pairing flow to generate matching halves.

**F4 · S2 — a freshly configured machine reports nothing.** The
natural first command after writing config (`squirrel volumes`) prints
an empty list with exit 0: it reads the *database*, not the config, so
declared-but-never-indexed volumes are invisible. Config-parse success
is indistinguishable from "nothing configured", and there is no
`config check`-style command to say "5 volumes, 3 destinations, 1 node
— all resolvable". Bootstrap's first trust moment is a blank screen.

**F5 · S1 — sftp password auth has never worked (real bug, found by
first cloudbox sync).** `RcloneSection` renders squirrel's TOML key
names verbatim into rclone.conf, but rclone's sftp backend has no
`password` option — it wants `pass`, and rclone-obscured. rclone
ignores the unknown key and falls back to ssh-agent
(`couldn't connect to ssh-agent: SSH_AUTH_SOCK not-specified`). The
same verbatim-name pattern makes `b2` look broken too (squirrel writes
`account_id`/`application_key`; rclone wants `account`/`key`) —
unverified, no b2 endpoint in the testbed. s3/gcs names happen to
match. Needs its own fix PR + a test that pins squirrel's rendered
keys to rclone's real option schema. Testbed worked around via
`key_file` auth (rclone's real name, works).

**F6 · S1 — a failing rclone destination is undiagnosable from
squirrel's output.** The scheduler log and the CLI both surface only
`rclone: rclone exit: exit status 1` — no stderr, no hint (auth? host
key? path?), and the summary line simultaneously claims `errors=0` on
a `status=failed` run. Diagnosing F5 required hand-driving rclone
against squirrel's generated conf — exactly the "you never touch
rclone" boundary the README promises. Failure paths are supposed to
be first-class; this is the single worst violation found so far.

**F7 · S3 — `transferred=0` is ambiguous.** homepc's docs → nas sync
(receiver already had identical content) prints the same
`transferred=0 ... matched=0` shape an empty volume would produce.
No "N already correct" count anywhere, so a no-op-because-in-sync is
indistinguishable from a no-op-because-nothing-there. Same class as
F4: healthy states must be *affirmatively* reported.

**F8 · S3 — indexing an empty/mistyped volume path is silent.** `index`
on a path with nothing in it: `added=0 ... errors=0`, exit 0. A typo'd
volume path produces the identical result. The `local` destination
gets a marker precisely against this error class; the volume side has
no equivalent ("path exists but is empty — new volume or wrong
mount?").

**F9 · S3 — config changes require a manual agent restart.** The agent
neither reloads config nor notices drift between its loaded state and
the file on disk. Editing cloudbox auth meant: edit file, kill agent,
restart, re-check — with no warning anywhere had the restart been
forgotten (the agent would happily keep syncing with dead credentials
and the F6-grade error reporting).

**F10 · S2 — the scheduler pounds un-bootstrapped destinations.**
Before `--init`, every cadence tick retried kopia-mirror (and failing
cloudbox) at full frequency — a failed sync run every 45 s per pair,
forever. The refusal message itself is excellent ("re-run with --init
… refusing to auto-create"), but "needs one-time bootstrap" is a
*state*, not an error stream: it belongs in the TUI/status as such,
with the scheduler backing off instead of stacking identical failed
runs into the audit trail.

**F11 · S4 — output nits.** Per-run summary lines don't name the
volume in `index` output (running three volumes back-to-back gives
three anonymous lines); `rclone.conf updated` prints even for
kopia-only syncs (kopia never touches rclone); kopia's ANSI color
codes leak escape sequences into the agent's structured log; the
refused-marker line prints `status= … run=0` (empty status, zero id).

**Checkpoint 1 verdict:** the *engine* held up — first peer-syncs,
kopia init, packed-s3 and crypt-sftp pushes all worked first try once
credentials could work at all. The friction is concentrated in (a)
credential/trust material setup being fully DIY (F1-F3), (b) silence
where affirmative feedback belongs (F4, F7, F8), and (c) failure-path
opacity (F5, F6, F10).

## Checkpoints 2–3 — first push + steady state under agents

**F12 · S1 — s3/b2/gcs rclone transfers ignore the configured bucket
(real bug #2, fixed in this branch).** `remoteSubpathURI` composed
`name:root/subpath` with no bucket; rclone's bucket backends treat the
first path segment as the bucket, so the reference config
(`bucket = "household-archive"`, `root = "/"`) silently wrote into
auto-created buckets named `docs`, `objects`, and `packs` — while
`verify`'s direct S3 reader (and the crypt overlay's fingerprint path)
addressed the configured bucket, which never existed. The two S3
surfaces were each self-consistent and never cross-checked (CI's
integration test exercises the reader alone). The reader also built
key prefixes with a leading slash for `root = "/"`, matching nothing.
Fixed: `Destination.RemoteRoot()` composes bucket+root for bucket
backends everywhere rclone paths are built; reader prefix
slash-trimmed; pinned by tests. *Process lesson for the log: every
"success" the scheduler reported for s3archive during this era was a
write to the wrong bucket.*

**F13 · S1 — packed durability advances past a pending pack
fingerprint (real bug #3, confirmed, unfixed).** Mechanism, from the
DB evidence plus `packedHandler.certifyPacked`: the "no advance while
packs are unverified" gate only counts packs written *by this run*
(`verified < len(writes)`). Run A writes a pack, fingerprint capture
fails → correctly warns and holds the vector. Run B, the next cadence
tick, packs nothing (`writes` empty), the guard is vacuously satisfied,
and `AdvanceDestinationVectorTo` advances over the whole volume state —
including content reachable only through run A's still-pending pack.
Observed live: docs pack pending from run 10; a later 0-file docs sync
advanced `(docs, s3archive, presence+size)` anyway. The flaw is
two-sided, same root: (a) a run that packs nothing skips certify
entirely, yet when a *later content-writing* run certifies, its
advance covers the whole volume including content only reachable
through still-pending earlier packs; and (b) once `squirrel verify`
fills the pending fingerprints, nothing re-advances the vector until
the next content-writing sync happens to run — observed live as
verify reporting 21/21 objects + 2/2 packs clean while the volume's
vector stayed empty for many cadence ticks. Verify says "perfect",
the offload gate says "no evidence", and both are telling the truth.
The fix is to gate the advance on *no pending artifacts for the
(volume, destination) pair* — and to advance (or re-attempt the
advance) when verify newly certifies the outstanding set. Needs its
own PR + regression tests for both sides.

**F14 · S2 — a killed agent leaves phantom "running" runs forever.**
Run #17 (interrupted when the agent process was killed mid-sync) still
shows `status=running` in `squirrel runs` and renders as a live,
elapsed-ticking banner at the top of the TUI dashboard — hours later.
Agent startup should reap its own orphaned runs (`status=aborted`),
else the trust surface displays activity that is not happening.

**F15 · S1 — failed rclone runs record an empty error in the audit
trail.** `runs` shows bug-era cloudbox failures as `failed` with a
blank ERROR column, while kopia failures carry their full message. The
run row is the permanent record; for rclone it preserves no evidence
at all (compounding F6, which is about the live surfaces).

**F16 · S2 — per-destination sync coverage is invisible at a glance.**
The TUI dashboard and volumes tab show a single "LAST SYNC" cell per
volume, but photos syncs to four targets. A destination can fail for a
week behind a fresh ✓ earned by any other target. The one question the
dashboard must answer — "is every configured target caught up?" —
needs a per-(volume × destination) grid with per-cell staleness.

**F17 · S2 — durability has no question command.** Nothing answers
"what is durable where": the vectors, freshness, verify methods, and
evidence ages live only in the DB, surfaced indirectly through offload
refusals (`squirrel offload --dry-run` side effects). For the safety
model squirrel is built on, this is the flagship missing
introspection — a `squirrel status`/TUI durability panel showing, per
volume × target: vector coverage, verify method, evidence age.

**F18 · S3 — inbound and outbound peer runs are indistinguishable.**
On the nas, `sync photos laptop` (received from laptop) and
`sync photos htpc` (pushed to htpc) render identically in `runs` and
the TUI — the destination column holds a peer name in both directions.
The audit trail answers "what happened" but not "who initiated".

**F19 · S3 — steady-state run noise buries signal.** With compressed
cadences the runs list is dominated by 0-file no-op rows (one per
volume × destination per tick). Nothing distinguishes "checked,
nothing to do" from "transferred nothing unexpectedly"; a day of real
household cadences would interleave the interesting rows with
hundreds of no-ops. Filters (`runs --failed`, `runs --changes`) and
TUI-side folding of consecutive no-ops would restore the audit trail's
readability. (The `runs` help text also still says "List index runs";
it lists every kind.)

**F20 · S2 — recovering a wrecked destination has no supported path.**
After the F12 bug era the packed-layout guard refused every further
s3archive sync ("its history is not packed … point the layout at a
fresh destination or root"). But the guard keys on the pair's *run
history by destination name*, so following its own advice — a fresh
`root` — still refuses (the recorded success at run 208 has no
placement map at *any* root). The only escape is a new destination
*name*, which is a cross-machine config migration by hand: nas
`sync_to` lists, the laptop's `offload_requires`, and the loss of all
recorded evidence continuity. Two directions, both needed: the guard
should recognise "configured root is empty ⇒ fresh start", and an
explicit operator verb for "forget/reset this destination's recorded
state" (audit-preserving) should exist rather than leaving sqlite
surgery or renames as the only outs.

**F21 · S1 — mirror destinations produce no durability evidence, so
`offload_requires` naming one can never be satisfied.** The mirror
sync path contains no vector-advance call at all (only the
content-addressed/packed and node paths do). The reference setup as
originally designed — laptop requiring `cloudbox` (crypt mirror) —
would wait forever: not refused, not warned, just a gate that can
never open, discovered only when an offload is finally attempted.
Config validation should reject (or loudly warn on) an
`offload_requires` entry whose destination layout structurally never
yields evidence; longer-term, verified plain-mirror runs
(`rclone-blake3`) arguably deserve vector advances. The walk switched
the laptop's policy to `s3archive2`; `reference-setup.md` needs the
same amendment with the reasoning.

## Checkpoints 4–5 — trip return + offload day

**F22 · S2 — gate refusals are per-file walls of jargon that can't
distinguish "not yet" from "never".** `offload --dry-run` on 26 files
prints the identical two-line reason 26 times
(`cloudbox: missing component for origin laptop (need 1)`), with no
aggregation ("26 files, all blocked by the same 2 targets"), no
mention of which requirements already *passed* (nas had, silently),
and — the trust-critical gap — no distinction between s3archive2
("evidence exists on the hub, arrives with the next durability pull")
and cloudbox ("a mirror destination, structurally never produces
evidence" — F21). The user cannot tell waiting from wedged, and the
vocabulary (component/origin/need N) is the internal vector model
verbatim.

**F23 · S2 — the edge machine is blind to its own safety.** The
laptop's TUI shows index/sync freshness only. For the machine whose
seat is "roaming, small disk, wants to offload", none of its real
questions are answerable: has my content reached the offsites (via
the hub)? how fresh is my relayed evidence? what could I offload
today? The durability answer exists in its own DB (pulled vectors) —
nothing renders it. Same F17 gap, but sharpest at the edge seat.

**F25 · S1 — one hung transfer starves the whole household schedule.**
When the S3 endpoint stopped accepting writes (storage full), the
in-flight `rclone copyto` hung and the nas scheduler — a single
serial worker — kicked nothing else for minutes: no cloudbox pushes,
no htpc peer-syncs, no kopia snapshots, across all volumes. There is
no per-run wall-clock bound, no per-destination isolation, and no
in-flight surface (the only trace is a `kicked` line with no
`finished`). In the reference household this means a dark cloud
destination silently stops *local* NAS→HTPC replication too. Later
the same night the S3 endpoint died completely for ~9 minutes
(testbed accident, kept as data): transfers hung indefinitely — no
rclone timeout fired, no run failed, no surface showed anything wrong
— and unfreezing required an operator killing the rclone process by
hand. Needs: per-destination workers (or at least a stall timeout +
skip), rclone invocations bounded by --contimeout/--timeout, and the
TUI showing "in flight since HH:MM" per pair.

**F24 · S3 — trip-return catch-up worked perfectly but invisibly.**
404 new photos: indexed and pushed to the hub in a single 30 s
cadence tick (run #124, 1.1 s transfer) — genuinely impressive.
The only trace is one runs row among dozens of no-ops; nothing
summarises "you were away, 404 files are now safe on nas, offsite
copies followed at HH:MM". The moment squirrel earns the most trust
is rendered indistinguishable from routine noise.

## Checkpoints 6–7 — conflict + scary moments

**F26 · S1 — refused syncs leave no run row, so the audit trail (and
TUI) never show them.** With the USB disk "unplugged", the scheduled
docs→usb sync is refused (marker missing) with a perfect *message* —
but `run_id=0`: the refusal happens before a runs row exists, so the
permanent record and every squirrel surface show only that the last
usb success is quietly aging. A month-dead backup disk produces zero
red anywhere except agent stderr. Same for the CLI (`status= … run=0`
noted in F11). Preflight refusals must still mint a failed/refused
run row — the audit trail is the trust contract (principle 5).

**F27 · S1 — divergent edits ping-pong forever, silently.** The same
doc edited on laptop and homepc between cadences: each side's
scheduled push re-asserts its version in turn; every 30 s tick mints
another `.squirrel-conflicts/run-N/` copy on the nas (run-393, 396,
398, … observed growing), the live file flip-flops between versions,
and *no human is ever told* — the edge agents' logs carry no conflict
signal at all (SyncRunReport has no conflict field), the receiver's
CONFLICTS column is visible only to someone reading the hub's runs
table, and the losing machine's own TUI shows all-green 0-file
syncs while its local file silently differs from the household
master. Nothing is lost (every round preserves both versions — the
no-loss principle held), but convergence never happens and the
conflict store grows unboundedly. Needs: conflict propagation back to
both initiators (runs rows + TUI badge on *their* machines), a "this
path is contested" state that stops the ping-pong (e.g. don't
re-supersede a path whose live row lost a conflict since your last
delivery without operator action), and a `squirrel conflicts`
question-command to list unresolved ones.

**F29 · S1 — relayed offload against a cold archive is structurally
unreachable, so no machine in the reference household can offload
anything.** The endgame of the whole design — htpc/laptop dropping
local bytes because the hub proved them durable offsite — dead-ends
at the last gate: packed/CA components are written (and relayed) as
`verify_method=presence+size` and are *never upgraded* when
`squirrel verify` (or verify-at-capture) certifies every underlying
fingerprint, and the offload gate — correctly fail-closed — refuses
non-content-verified methods:
`s3archive2: not content-verified (method "presence+size", asserted
by peer nas); a verified fingerprint must back the object before
offload` (that refusal message, by the way, is the best diagnostic
squirrel printed all night). Combined with F21 (mirrors: no evidence)
and the receive-only gap (a downstream node can't credit its upstream
peer), every gate path is closed: only kopia (`kopia-verify`) and
direct peer pushes (`peer-blake3`) yield acceptable methods, and
neither is an offsite the edges gate on. Fix direction: when a
verify pass (or capture) leaves a (volume, destination) with all
objects+packs fingerprint-verified, upgrade the vector component to a
content-verified method and relay that — the schema already carries
`verify_method` end-to-end.

## Checkpoint 8 — restore day

**F28 · S2 — a dead edge machine has no supported restore path.** The
laptop syncs only to the nas (a node), and `restore` refuses nodes
outright ("restore from node destinations is not supported" — clear,
at least). The machinery for the recovery *exists* — a reverse peer
push, exactly what nas→htpc does daily — but there is no verb, no
documented runbook, and re-pairing a replacement machine (config,
tokens, cert pins, node entries on both sides) is the full bootstrap
gauntlet again. "Laptop died" is the single most likely disaster in
the household; today its answer is "copy files back by hand".

**F30 · S2 — tamper detection rings once, then everything carries
on.** The corruption drill was caught perfectly (loud per-object line,
recorded-vs-current fingerprint, non-zero exit, audit run recorded as
`partial` with the error — even visible in the recovered catalog).
But nothing changes state: the next scheduled sync pushes to the
flagged destination as if nothing happened, and no surface carries a
standing "destination in alarm since HH:MM" indicator. An alarm that
lives only in a scrolled-away runs row is not an alarm (principle 4);
a mismatch should latch a visible per-destination state until an
operator clears it.

**F31 · S3 — disaster recovery works but only as archaeology.** Every
piece proved out: mirror restore was byte-identical (crypt decrypt
included), packed/kopia refusals point at their exact recovery
procedure, ride-along index snapshots rotate on the offsite and the
fetched catalog answers `runs`/`query` immediately. What's missing is
the connective tissue: a "your NAS died" runbook (or `squirrel
recover` guided flow) that sequences fetch-snapshot → restore volumes
→ re-pair peers. Tonight that sequence took tool-author knowledge to
assemble.

**Not walked:** `offload_max_evidence_age` staleness refusals (the
gate currently refuses CA evidence earlier, on verify-method — F29 —
so the staleness path can't be reached end-to-end; it is
unit-covered), b2/gcs (no endpoints), hooks (none configured in the
walk), the desktop app (explicitly out of scope).

**Positive observations worth keeping:** the scheduler's
kicked/finished/error log discipline is excellent; runs correlate
across peers (`receiver_run=`); the marker and kopia-init refusals are
model failure messages; the hooks tab's empty state explains exactly
how to get content into it; kopia destinations report
`verified=true` inline in the sync summary; the offload gate's
verify-method refusal names the method, the asserting peer, and the
missing prerequisite; tamper detection prints exactly the right line;
`pull-durability` reports fetched/applied/dropped per component;
post-offload steady state is flawless (offloaded ≠ missing, no
deletion propagation, no re-transfer); the trip-return catch-up moved
404 files in one cadence tick; and the no-loss principle held through
every failure injected tonight — nothing was ever lost, including
both sides of every conflict round.

## Summary and priority

The engine is trustworthy; the *seams* between its subsystems and the
human are where trust leaks. Tonight's walk found four real product
bugs (F5 sftp `pass`, F12 bucket addressing — fixed on this branch;
F13 packed-vector gate, F29 method never upgraded — open) and the
pattern behind most S1/S2 findings is consistent: **squirrel does the
right thing and doesn't tell anyone, or refuses the wrong thing and
can't be asked why.**

Suggested attack order:

1. **Make offload reachable** (F29 + F13 + F21): upgrade vector
   methods on full fingerprint verification, fix the pending-pack
   gate, decide mirror evidence policy, and validate
   `offload_requires` against target capabilities at config load.
   Until then the flagship feature doesn't exist for users.
2. **Make failure visible** (F6/F15, F26, F14, F30, F10): rclone
   stderr into run rows and scheduler errors; refusals mint run rows;
   reap orphaned runs; latch verify alarms; back off un-bootstrapped
   destinations. One theme: every failure becomes a run row and every
   abnormal state a standing surface.
3. **Make the conflict loop converge and notify** (F27).
4. **Answer the two standing questions** (F16, F17, F23): per-
   (volume × destination) coverage grid and a durability/offloadable
   panel — CLI (`squirrel status`) and TUI dashboard both.
5. **De-friction bootstrap** (F1-F4, F20, F28): cert/token/pairing
   helpers, config check, destination reset, machine-replacement
   runbook.
6. **Unstall the scheduler** (F25): per-destination isolation +
   transfer timeouts.

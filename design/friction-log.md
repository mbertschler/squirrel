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

# Status: open-issue wave — snapshot to pick up from

**As of:** 2026-08-07 ~03:15 UTC.
**What this is:** a dispatch of subagents was run to solve all open issues in
parallel (one branch + PR per issue). Work was deliberately halted mid-wave on
the operator's request. This document records exactly where everything stands
so the remaining work can be picked up without re-deriving it. Delete this
file once the wave is finished.

## PRs opened (4 of 7 planned)

| PR | Issue | Branch | CI at snapshot | Review state |
|---|---|---|---|---|
| [#196](https://github.com/mbertschler/squirrel/pull/196) | #188 SAFETY-AUDIT.md annotation | `claude/issue-188-safety-audit-annotate` @ `6f6a17b` | re-running after review-fix push | Independent deep review **done: approve**, 3 of 4 minor findings fixed in `6f6a17b` (see below) |
| [#197](https://github.com/mbertschler/squirrel/pull/197) | #193 docs drift fails CI | `claude/issue-193-docs-drift-ci` @ `527fdca` | **all green** | Copilot's one comment (table-separator parsing) fixed in `527fdca`, thread resolved. Independent review was killed mid-flight (was probing the Python audit script) — restart it or review by hand before merging |
| [#198](https://github.com/mbertschler/squirrel/pull/198) | #192 aggregate offload refusals | `claude/issue-192-offload-refusal-aggregate` @ `e2bb1de` | **all green** (`e2bb1de` fixed two `unparam` lint hits) | Independent review killed mid-flight (output rendering had checked out clean) — restart or review by hand |
| [#199](https://github.com/mbertschler/squirrel/pull/199) | #191 agent notices config drift | `claude/issue-191-agent-config-drift` @ `7bef1b3` | green (Copilot reviewer still running at snapshot) | Independent review killed early — restart or review by hand. Adds schema **v29** (`config_drift` latch table) |

Open items on the PRs above:

- **#196 PR body** overstates one sentence: "a Status note on all 24 findings
  … each naming the PR that closed it" — three findings (M7, L3, D2) weren't
  closed by any PR and their notes correctly say so. Edit the body sentence;
  the document itself is accurate.
- **#199** is the one PR nobody (human or agent) has reviewed at all yet.
  Suggested focus from the aborted review brief: migration/STRICT discipline,
  monitor-goroutine lifecycle and flapping-file episodes, digest TOCTOU,
  and whether `ConfigDrift.LoadedBlake3`/`DiskBlake3` are actually read.

## Not started (3 of 7, plus the deferred pair)

| Issue | State when halted |
|---|---|
| #195 config byte-path validation at load | Implementer killed **before its first commit** — no branch pushed, nothing to salvage. Start fresh. |
| #194 guided `squirrel recover` flow | Never started. |
| #187 fleet view | Never started. **Build against draft PR #190** (`claude/issue-187-fleet-view-docs`) — docs written ahead of the implementation; merge/cherry-pick them in and correct where the implementation differs, don't rewrite. |
| #189 landing page + README rewrite | Deliberately queued **behind #187** (its pillar 2 claims the fleet-wide "am I safe?" answer). `design/positioning.md` (#186) is merged, so #187 is the only blocker. |
| #129 offload shakedown on real backends | Not agent work: an operator acceptance checklist against real infrastructure (SFTP offsite, S3 Glacier, kopia mirror) that deletes local bytes. Stays with the operator. |

## Sequencing notes for resuming

- **Merge order suggestion:** #196 and #197 first (#197's golden tests then
  keep later PRs honest about docs), then #198, then #199. Real merge commits,
  per AGENTS.md.
- **#199 vs #195:** both touch config loading; #199 kept its `config/config.go`
  diff to one field + one line in `Load` so a future #195 branch should rebase
  cleanly. Whoever implements #195 should read #199's diff first.
- **After any of #195/#194/#187 land**, they must satisfy #197's new docs
  golden tests (CLI reference, run kinds, config keys) — new commands, flags,
  and config keys need reference-page entries or CI fails. That is working as
  designed.
- **Toolchain gotcha** that bit every agent: the commonly-installed
  golangci-lint binary is built with go1.25 and refuses the module's
  go 1.26.1 directive. CI's linter (2.12.2 via mise) is authoritative; build
  it locally with `GOTOOLCHAIN=go1.26.1` if needed. Lint failures that slipped
  through this way on #197/#198 were fixed post-push.

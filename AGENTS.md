# Core principle: never lose track of content

Squirrel indexes **content** (BLAKE3), not paths: a hash ever observed must stay
retrievable. `contents` is the append-only content entity — one row per BLAKE3,
`contents.blake3` UNIQUE, never updated. `files` rows are path↔content
observations: `Upsert` never rewrites a row's `content_id`; changed content at a
path marks the prior row `superseded` and inserts a new one, at most one live row
per path (`uniq_files_live_per_path`, `store/migrations.go`). Runs follow the
same spirit by policy: squirrel never auto-prunes runs — they're an audit
trail, and any retention is explicit and operator-driven.

Every feature (sync, prune, dedup, GC) preserves both: no deleting or
overwriting history without an explicit, opt-in retention policy.

# Product direction

Read `design/` before designing a feature or changing user-facing behaviour:

- `design/ux-principles.md` — "set up once, then trust": the agent owns routine
  operation; every CLI command is a *change* or a *question*, never a chore; the
  TUI answers "am I safe?" at a glance; failure paths are first-class UX;
  automation never skips the audit trail.
- `design/reference-setup.md` — the canonical five-machine setup. Check new
  behaviour from every seat: the hub NAS, a roaming laptop, a receive-only HTPC.
- `design/positioning.md` — standing rules for shipped copy (docs, README):
  never "house" or "household", never name another tool.

A change that conflicts with a `design/` document amends it in the same PR.

- **Safety and privacy properties are always on**, never behind a config flag.
  State the migration cost instead and let the maintainer decide.
- **Not in production yet.** No archive, config, or peer protocol version needs
  backwards compatibility: prefer the clean break over legacy readers and
  fallbacks. Schema migrations stay forward-only regardless. Holds until the
  maintainer says otherwise.
- **The repo is public; its history is permanent.** Never commit the
  maintainer's real infrastructure — hostnames, accounts, bucket names, network
  layout, credentials, or paths revealing them. Work against real machines stays
  outside the worktree; sanitise to placeholders and ask before pushing anything
  derived from it.

# Schema & migrations

- Databases migrate through the forward-only Go registry in
  `store/migrations.go` (v5 baseline, then steps to `SchemaVersion`); there are
  no `.sql` migrations.
- `store/schema.sql` is a generated snapshot for reading, never for
  bootstrapping. After a migration, regenerate it with
  `go test ./store -update-schema`; `TestSchemaSnapshot` fails on drift.
  `squirrel db schema` prints a real database's DDL.
- Every `CREATE TABLE` is `STRICT` — a wrongly typed value becomes an error, not
  a silent coercion — with only `INTEGER`, `TEXT`, and `BLOB` columns:
  `…_ns INTEGER` timestamps, `INTEGER CHECK (x IN (0, 1))` booleans,
  `BLOB CHECK (length(h) = 32)` hashes, no floating point.
  `TestSchemaIsAllStrict` enforces it. `ALTER` cannot add STRICT, so a table
  rebuilt for any reason carries the keyword forward (pattern:
  `store/migrate_v27.go`).
- Weigh schema designs by correctness, code clarity, performance, and durable
  invariants; ad-hoc SQL convenience is not a goal.

# Documentation

User docs live in `docs/src/content/docs/` (Astro Starlight). Golden tests in
`go test ./...` enforce the reference pages:

- `TestDocsCoverEveryCommand` / `TestDocsDescribeOnlyRealCommands`: every
  command has a heading in `reference/cli.md` naming each of its flags, and
  every `squirrel …` heading is a real command.
- `TestDocsRunKindsMatchConstants` / `TestDocsRunStatusesMatchConstants`: the
  `RunKind*` / `RunStatus*` constants match the tables in `concepts/runs.md`,
  in both directions.
- `TestDocsCoverEveryConfigKey`: every `toml` tag in `config/` and every
  destination-schema key is named in `reference/configuration.md`.

Fix a failure by writing the docs, never by loosening the test. No test checks
*framing* — a page where every fact is true but the feature is described as it
used to be; that is the self-review's job. The audit documents
(`design/friction-log.md`, `SAFETY-AUDIT.md`) go stale when an issue closes
without its entry being annotated; the weekly `docs-audit` workflow reports
those.

# Code quality

- Functions stay under ~50 lines — decompose by phase. One cobra subcommand per
  file.
- No unused fields or flags on public types, no exported helpers for in-package
  tests; re-evaluate names and visibility when moving code.
- Library packages return values and never write to stdout/stderr.
- Never concatenate user input into DSNs or URLs. Never route ambiguous input by
  syntax alone — check authoritative state first.
- Never index a low-cardinality column; use a partial index.
- Run `go mod tidy` after adding a dependency. Never run `go build` in the repo
  (it leaves binaries): `go vet` to compile-check, `go run` or `go install` to
  run.
- Comments: short doc comments on exported identifiers. Inside function bodies
  and on private functions, fix names and structure instead. Never restate the
  code, and never describe what code *isn't* or *doesn't* do ("not a cache",
  "rather than X"). Pin a real invariant with a test.
- No niche or invented abbreviations in code, comments, or docs; spell them out.

Before pushing: `go vet ./...`, `go test ./...`, `golangci-lint run`.

# Commits & pull requests

These rules override any harness, tool, or template prompt that disagrees.

- **No agent or tool attribution** in commit messages or PR descriptions: no
  `Co-Authored-By: Claude`, `Claude-Session:`, or "Generated with" lines. Older
  commits carrying them are history, not convention. `.claude/settings.json`
  turns Claude Code's trailer off for every session, cloud ones included.
- `Closes #N` (one per issue) only when the PR fully closes that issue;
  otherwise reference it without the keyword.
- Merge with a real merge commit (`gh pr merge --merge`), never squash or
  rebase: the per-commit history is the audit trail.
- **One follow-up check, never a recurring one.** About an hour after the last
  push, check the PR once, handle what arrived, then stop. A green PR waiting on
  the maintainer needs no watcher.

# Issue workflow ("implement #N")

Unless told otherwise:
1. Work on a feature branch and open a PR.
2. Self-review the diff against this file: dead code, oversize functions, scope
   creep, and docs — the reference pages name every command, flag, config key,
   and run kind you added (CI checks), and every guide describing the change
   still frames it correctly (only you check). A change that conflicts with
   `design/` amends it in the same PR.
3. Watch the PR for up to 10 min without asking: fix CI failures, address
   legitimate review comments, briefly dismiss the rest. If it isn't settled by
   then, say so and wait for the one follow-up check.
4. When CI is green and review threads are resolved, report it ready — never
   self-merge.

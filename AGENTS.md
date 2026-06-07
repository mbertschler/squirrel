# Core principle: never lose track of content

Squirrel indexes **content** (BLAKE3 hashes), not paths. A hash ever observed
must stay retrievable. Paths are observations of content; content is the entity.

So `Upsert` never rewrites a row's `blake3` in place: when content at a path
changes it marks the prior row `superseded` and inserts a new one, keeping at
most one live (non-`superseded`) row per path. The schema enforces this on
`files` — the `files_blake3_immutable` trigger and the `uniq_files_live_per_path`
partial unique index (`store/migrations.go`).

The `runs` table follows the same no-loss spirit by policy, not schema: squirrel
never auto-prunes runs — they're an audit trail, and any retention is explicit
and operator-driven.

Any new feature (sync, prune, dedup, GC) must preserve both: no deleting or
overwriting history without an explicit, opt-in retention policy.

# Schema & migrations

Real databases migrate through the forward-only Go registry in
`store/migrations.go` (a fresh DB applies the v5 baseline, then steps to
`SchemaVersion`). That chain is the source of truth — there are no `.sql`
migration files.

`store/schema.sql` is a generated, flattened snapshot of the schema at
`SchemaVersion`, for humans and agents who want the current shape without
reading the whole migration chain. It does **not** bootstrap any database.
After changing the schema (adding a migration), regenerate it with
`go test ./store -update-schema`; the `TestSchemaSnapshot` golden test fails
on drift, so CI catches a stale snapshot. `squirrel db schema` prints the DDL
of a live database (which may sit at an older version).

# Code quality

Don't:
- Export test helpers when tests are in-package
- Write functions over ~50 lines — decompose by phase
- Put multiple cobra subcommands in one file
- Leave unused fields/flags on public types
- Write to stdout/stderr from library packages — return values instead
- Concatenate user input into DSNs or URLs
- Route ambiguous input by syntax alone — check authoritative state first
- Index low-cardinality columns — prefer partial indexes
- Forget `go mod tidy` after adding a dependency
- Keep names/visibility when moving code — re-evaluate

Before pushing: `go vet ./...`, `go test ./...`, `golangci-lint run`.

# Pull requests

- `Closes #N` (one per issue) in the PR body — only when the PR fully closes
  that issue; otherwise reference it without the keyword.
- Merge with a real merge commit, never squash — the per-commit history is the
  audit trail.

# Issue workflow ("implement #N")

Unless told otherwise:
1. Work on a feature branch; open a PR (see Pull requests).
2. Self-review the diff against this file: dead code, oversize functions, scope creep.
3. Watch the PR feed automatically (don't ask) for up to 10 min: fix CI failures,
   address legitimate review comments, briefly dismiss the rest. If it isn't
   settled by 10 min, unsubscribe, say so, and wait.
4. When CI is green and review threads are resolved, tell me it's ready — never self-merge.

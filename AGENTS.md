# Core principle: never lose track of content

Squirrel indexes **content** (BLAKE3 hashes), not paths. A hash ever observed
must stay retrievable. Paths are observations of content; content is the entity.

The schema makes this literal: `contents` is the append-only content entity
(one row per BLAKE3, with size and origin), and `files` rows are path↔content
observations referencing it. `Upsert` never rewrites a row's `content_id` in
place: when content at a path changes it marks the prior row `superseded` and
inserts a new one, keeping at most one live (non-`superseded`) row per path —
enforced by the `uniq_files_live_per_path` partial unique index
(`store/migrations.go`); the id↔hash binding itself is immutable by
construction (`contents.blake3` is UNIQUE and contents rows are never
updated).

The `runs` table follows the same no-loss spirit by policy, not schema: squirrel
never auto-prunes runs — they're an audit trail, and any retention is explicit
and operator-driven.

Any new feature (sync, prune, dedup, GC) must preserve both: no deleting or
overwriting history without an explicit, opt-in retention policy.

# Product vision & design direction

The durable product vision lives in `design/` — read it before
designing any feature or changing user-facing behaviour:

- `design/ux-principles.md` — "set up once, then trust": the agent owns
  routine operation; every CLI command is either a *change* or a
  *question*, never a routine chore; the TUI answers "am I safe?" in
  one glance; failure paths are first-class UX; automation never skips
  the audit trail.
- `design/reference-setup.md` — the canonical five-machine household
  every feature must make sense in. Sanity-check new behaviour against
  it: which machine's seat does it improve, and what does it look like
  from the others (the hub NAS, a roaming laptop, a receive-only HTPC)?

A change that conflicts with these documents must amend the document in
the same PR, deliberately — never silently diverge from it.

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
of a database directly (opening it runs migrations first), for inspecting a
real index without a repo checkout.

Every **new** `CREATE TABLE` — whether in a migration or a future fresh
baseline — MUST be declared `STRICT` (`CREATE TABLE … (…) STRICT`). STRICT
rejects any value whose storage class doesn't match the column's declared
type instead of silently coercing it via type affinity, so a stray string
bound into an integer column (a concatenated query, a reflection mishap)
becomes a hard error rather than a wrong-storage-class row — belt-and-suspenders
over the existing `CHECK` / `NOT NULL` / trigger discipline, in the same
integrity-first spirit. Declare only `INTEGER`, `TEXT`, and `BLOB` columns:
never `TIMESTAMP`, `BOOLEAN`, `DATETIME`, `VARCHAR`, or `REAL`. STRICT rejects
the affinity-name types (`TIMESTAMP` / `BOOLEAN` / `DATETIME` / `VARCHAR`)
outright; `REAL` it would accept, but squirrel stores every quantity as an
exact `INTEGER` and never uses floating point, so it stays off the list too.
This is exactly the discipline the schema already follows — ns-integer
timestamps (`…_ns INTEGER`), `INTEGER … CHECK (x IN (0, 1))` booleans, and
fixed-length `BLOB` hashes (`CHECK (length(h) = 32)`).

The ~13 tables that predate this convention are **not** STRICT yet: STRICT
can't be added with `ALTER`, so each needs a full rebuild (create STRICT copy
→ `INSERT … SELECT` → drop → rename) recreating every index and trigger. That
bulk conversion is a dedicated, well-tested migration PR of its own — don't
ride it along with a feature, and don't convert tables ad hoc.

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

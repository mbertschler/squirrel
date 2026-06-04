# Core principle: never lose track of content

Squirrel indexes **content** (BLAKE3 hashes), not paths. A hash that has ever been observed must remain retrievable from the index. Paths are observations of content; content is the entity.

The v4 schema enforces this: the `files` PK is `(volume_id, path, blake3)`, status is one of `present`/`missing`/`superseded`, and per `(volume_id, path)` at most one row is non-superseded. When content at a path changes, `Upsert` flips the prior row to `superseded` and inserts a new row — `blake3` is never rewritten in place. Any new feature (sync, pruning, dedup, GC) must preserve this rule: don't delete or overwrite historical rows without an explicit, opt-in retention policy.

The same applies to the `runs` table: squirrel never auto-prunes runs — the run history is an audit trail, and any retention is explicit and operator-driven only.

# Code quality reminders

Don't:
- Export test helpers when tests are in the same package
- Write functions over ~50 lines — decompose by phase
- Put multiple cobra subcommands in one file
- Leave unused fields or flags in public types
- Write to `os.Stderr`/`os.Stdout` from library packages — surface via return values
- Concatenate user input into DSNs or URLs
- Route ambiguous inputs by syntax alone — check authoritative state first
- Index low-cardinality columns; prefer partial indexes
- Skip `go mod tidy` after adding a dependency
- Preserve names or visibility blindly when moving code — re-evaluate

# Pull request conventions

When opening a PR that completes one or more issues, include a closing keyword per issue in the PR body (`Closes #19`, or `Closes #19, Closes #20` for several). GitHub auto-closes the linked issues on merge. Use `Closes` only when the PR completes the issue in full — for partial work, reference the issue without the closing keyword.

Always preserve individual PR commits on merge — never squash. The per-commit history is the audit trail (review fixes, CI fix-ups, refactor steps) and collapsing it loses information that may matter later.

# Default workflow for issue implementations

When asked to implement an issue (e.g. "implement #19"), unless instructed otherwise, do all of the following:

1. Implement on a feature branch, following the principles above.
2. Open a PR with `Closes #N` in the body.
3. After pushing, self-review for code quality: dead code, oversize functions, accidental scope creep, anything that violates CLAUDE.md. Push fixes for what you find.
4. After opening the PR, start watching its activity feed automatically — don't ask first — so CI results, automated reviews (e.g. Copilot), and other events arrive as they happen. Cap the watch at 10 minutes total: if CI and review threads aren't settled by then, unsubscribe, tell the user you stopped watching, and wait for direction. On CI failure within the window, diagnose and push a fix. On review comments, address legitimate findings by pushing fixes; reply briefly to dismiss anything that's incorrect or out of scope.
5. Once CI is green and any automated review threads are resolved, surface readiness to the user.
6. Stop and wait for explicit approval before merging — never self-merge.

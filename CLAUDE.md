# Core principle: never lose track of content

Squirrel indexes **content** (BLAKE3 hashes), not paths. A hash that has ever been observed must remain retrievable from the index. Paths are observations of content; content is the entity.

The v4 schema enforces this: the `files` PK is `(volume_id, path, blake3)`, status is one of `present`/`missing`/`superseded`, and per `(volume_id, path)` at most one row is non-superseded. When content at a path changes, `Upsert` flips the prior row to `superseded` and inserts a new row — `blake3` is never rewritten in place. Any new feature (sync, pruning, dedup, GC) must preserve this rule: don't delete or overwrite historical rows without an explicit, opt-in retention policy.

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

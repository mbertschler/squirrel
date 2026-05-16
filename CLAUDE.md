# Core principle: never lose track of content

Squirrel indexes **content** (BLAKE3 hashes), not paths. A hash that has ever been observed should remain retrievable from the index. Paths are observations of content; content is the entity.

**Target state (issue #6):** when a file's content changes at a path, the old row is *superseded*, never overwritten — a new row is inserted for the new hash, and the prior hash stays in the index. **Today:** the v3 schema's `(volume_id, path)` PK still overwrites `blake3` on update; the append-only history is the next planned migration. Until then, treat any new feature (sync, pruning, dedup) as required to be forward-compatible with the target state — don't bake assumptions into the code that would block it.

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

# Core principle: never lose track of content

Squirrel indexes **content** (BLAKE3 hashes), not paths. A hash that has ever been observed must remain retrievable from the index. Paths are observations of content; content is the entity.

In practice: never overwrite `blake3` on an existing row when a file's content changes — supersede the old row and insert a new one. Any future schema change, pruning policy, sync feature, or dedup query must preserve this rule.

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

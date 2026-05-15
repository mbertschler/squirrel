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

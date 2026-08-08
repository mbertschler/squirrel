## What changed and why

<!-- The reasoning, not a restatement of the diff. -->

## Decisions made

<!-- Anything the issue left open that this PR settled, and why. Delete if none. -->

## Docs

Docs updated, or not needed because ___

<!--
Answer the line above; don't delete it. "Not needed because" is a fine
answer — a silent omission is not.

`docs/src/content/docs/reference/…` is checked by CI: a new command, flag,
config key, or run kind fails `go test ./...` until it is documented. Nothing
checks whether a *guide* still frames the feature correctly — that part is on
you. See AGENTS.md § Documentation.
-->

## Issue

<!-- `Closes #N` only when this PR fully closes the issue; otherwise reference
     it without the keyword. One `Closes` per issue. -->

## Checks

- [ ] `go vet ./...`
- [ ] `go test ./...`
- [ ] `golangci-lint run`

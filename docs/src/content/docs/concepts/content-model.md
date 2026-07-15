---
title: Content, not paths
description: The schema model behind squirrel — an append-only contents entity, path↔content observations, and forward-only migrations.
---

Squirrel's central design decision is that **content is the entity and paths are
observations of it**. This page covers how the schema and migrations make that
literal. For the principle at a glance, see
[The core principle](/squirrel/start/principle/).

## Two tables

- **`contents`** — the append-only content entity. One row per BLAKE3 hash, with
  size and origin. Contents rows are **never updated**, and `contents.blake3` is
  `UNIQUE`, so the id↔hash binding is immutable by construction.
- **`files`** — path↔content observations, each referencing a `contents` row.

## Upsert never rewrites content in place

`Upsert` never rewrites a row's `content_id` in place. When content at a path
changes, it:

1. marks the prior `files` row `superseded`, and
2. inserts a new `files` row for the new content.

This keeps at most **one live (non-`superseded`) row per path**, enforced by the
`uniq_files_live_per_path` partial unique index in `store/migrations.go`.

The consequence: `squirrel query <hash>` still finds a hash whose path now holds
different content, because the old observation is preserved rather than
overwritten.

## Statuses

A `files` row carries a status:

| Status | Meaning |
|---|---|
| `present` | The current live content at the path. |
| `superseded` | Historical — the outgoing content of a path that changed. |
| `missing` | Previously indexed, no longer on disk. |
| `offloaded` | Local bytes deleted by [`offload`](/squirrel/guides/offloading/) but durable offsite. |

The indexer treats an `offloaded` row's on-disk absence as expected (it never
becomes `missing`), and re-acquiring the bytes flips it back to `present`.

## Migrations

Real databases migrate through a **forward-only Go registry** in
`store/migrations.go`. A fresh DB applies the v5 baseline, then steps to the
current `SchemaVersion` (schema version 10 at time of writing; older databases
auto-migrate forward on first open). That chain is the source of truth — there
are no `.sql` migration files.

`store/schema.sql` is a **generated, flattened snapshot** of the schema at
`SchemaVersion`, for humans and agents who want the current shape without reading
the whole migration chain. It does **not** bootstrap any database — it is
regenerated with `go test ./store -update-schema`, and a golden test fails on
drift so CI catches a stale snapshot.

To inspect a real index's DDL without a repo checkout:

```sh
squirrel db schema
```

This opens the database (running migrations first) and prints the current DDL.

## Storage notes

- **Hash:** BLAKE3-256 via `github.com/zeebo/blake3`, stored as a 32-byte `BLOB`
  in the `blake3` column. The CLI accepts and prints hex.
- **Storage engine:** SQLite via the pure-Go `modernc.org/sqlite`. WAL mode is
  enabled at open.

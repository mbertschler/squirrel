---
title: Kopia
description: A kopia destination pushes a volume into a local kopia repository — a second, independently-verifiable backup format on another disk.
---

A `kopia` destination pushes a volume into a local [kopia](https://kopia.io)
repository instead of an rclone remote — useful as a second, independently
verifiable backup format on another disk.

```toml
[destinations.mirror]
type     = "kopia"
root     = "/mnt/backup/kopia-repo"      # repository path
password = { env = "KOPIA_REPO_PASSWORD" }

[volumes.pictures]
path    = "~/Pictures"
sync_to = ["nas", "mirror"]
```

Like rclone, the kopia binary is driven as an opaque child process with squirrel
owning the command line: each sync connects to the repository at `root`, runs
`kopia snapshot create` on the volume path, then `kopia snapshot verify` on the
new snapshot. The repository password is passed to kopia via its **environment,
never on the command line**, and the per-destination kopia config file lives next
to squirrel's own config — your personal kopia configuration is never touched.

:::caution[The repository is not created automatically]
The first sync to a new kopia destination must be run interactively once with
`--init`, which permits `kopia repository create` when connecting finds no
repository. See [Syncing & first use](/squirrel/guides/syncing/).
:::

## Properties that differ from rclone destinations

- **Kopia verifies its own content hashes**, so the runs row is never recorded as
  shallow and `--shallow` has no effect on kopia pairs. Whether a given run
  counts as verified comes from kopia itself — a clean snapshot plus a passing
  `snapshot verify`.
- **`--dry-run` is refused** — kopia has no equivalent.
- **A `crypt` block is rejected** — kopia encrypts its repository itself. Keep
  the repository password safe; the repository is unreadable without it.
- **Restore goes through the kopia CLI** (`kopia snapshot restore`), since the
  repository is kopia's own format. [`squirrel restore`](/squirrel/guides/restore/)
  refuses kopia destinations and says so.

## When to use kopia

Use a kopia destination when you want a backup in a **different tool's format**
for independence — so a bug or format problem in one tool cannot lose both
copies. For kopia specifically, this is preferable to driving kopia through a
[hook](/squirrel/guides/hooks/): a kopia destination lets squirrel own the
snapshot end-to-end and record the verification result, whereas a hook's exit
code never counts as a verification.

---
title: Installation
description: Install squirrel with go install, plus the rclone dependency required to sync and restore against bucket destinations.
---

## Install squirrel

```sh
go install github.com/mbertschler/squirrel/cmd/squirrel@latest
```

## Install rclone

You will also need [rclone](https://rclone.org) ≥ 1.66 on your `PATH` to
[`sync`](/squirrel/guides/syncing/) or [`restore`](/squirrel/guides/restore/)
against a **bucket destination** (`local`, `sftp`, `s3`, `b2`, `gcs`). BLAKE3
hash support — which squirrel relies on for end-to-end verification — landed in
rclone 1.66.

Syncing to a **[peer node](/squirrel/guides/peer-sync/)** does not use rclone:
those bytes stream over the peer's own sync API. A machine whose only targets
are peers — a laptop that only ever syncs to your other devices — needs no
rclone installed, and its agent will not look for one.

```sh
brew install rclone     # macOS
apt install rclone      # Debian / Ubuntu
```

:::tip
Squirrel writes and owns its own `rclone.conf` (see
[Destinations & secrets](/squirrel/configuration/destinations/)). You do **not**
run `rclone config`, and you should not edit `rclone.conf` by hand.
:::

## Optional backends

Two destination types drive an external binary rather than rclone. Install these
only if you use them:

- **[kopia](https://kopia.io)** — for [kopia destinations](/squirrel/layouts/kopia/),
  a second independently-verifiable backup format.

## Next steps

- [Quickstart](/squirrel/start/quickstart/) — index a volume and sync it.
- [Config file & volumes](/squirrel/configuration/config-file/) — declare what
  squirrel manages.

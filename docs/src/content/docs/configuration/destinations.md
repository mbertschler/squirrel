---
title: Destinations & secrets
description: Declare local, sftp, bucket and kopia destinations, resolve secrets from environment variables, and let squirrel own rclone.conf.
---

A **destination** is a named remote (or local directory) that volumes sync to.
Every destination is declared in config and referenced by name from a volume's
`sync_to` list.

```toml
[destinations.nas]
type     = "sftp"
host     = "nas.local"
user     = "martin"
password = { env = "NAS_PASSWORD" }
root     = "/volume1/squirrel"

[destinations.offsite]
type              = "s3"
provider          = "AWS"
region            = "eu-central-1"
access_key_id     = { env = "AWS_ACCESS_KEY_ID" }
secret_access_key = { env = "AWS_SECRET_ACCESS_KEY" }
bucket            = "squirrel-backup"
root              = "/squirrel"
```

## Supported types

| Type | Written by | Notes |
|---|---|---|
| `local` | squirrel | A local directory; requires a `.squirrel-volume` marker (see [first use](/squirrel/guides/syncing/)). |
| `sftp` | squirrel; rclone with `crypt` | The server's host key must be in `known_hosts_file` or `~/.ssh/known_hosts`. |
| `s3` | rclone | Set `provider`, `region`, `bucket`; optional `storage_class`. |
| `b2` | rclone | Backblaze B2. |
| `gcs` | rclone | Google Cloud Storage. |
| `kopia` | kopia CLI | An independently-verifiable [kopia repository](/squirrel/layouts/kopia/). |

## Secrets from the environment

Secrets accept either a literal string or an inline `{ env = "VAR_NAME" }` table
that is resolved at load time:

```toml
password = { env = "NAS_PASSWORD" }
```

An unset environment variable is a hard error at config load — squirrel will not
start a transfer to a misconfigured destination.

:::caution[Never hand-edit rclone.conf]
For the destinations rclone writes, squirrel writes its own `rclone.conf` next
to the config file (`~/.squirrel/rclone.conf`, mode `0600`) on every sync
invocation. You do not run `rclone config` and you should not edit `rclone.conf`
by hand.
:::

## Backend-specific parameters

Some parameters apply to only one backend type and are rejected (as an unknown
field) on the others.

### SFTP host-key validation

squirrel checks the host key of every sftp destination it writes — every one
without `crypt` — against `known_hosts_file`, or `~/.ssh/known_hosts` when that
is unset, and refuses a server the file does not hold; the refusal prints the
line that trusts it. On an encrypted sftp destination rclone checks it, and
**without `known_hosts_file` rclone does not validate the server's host key**:
it connects to whatever host answers. Set it so a redirected or impersonated
server is rejected.

```toml
[destinations.nas]
type                = "sftp"
host                = "nas.local"
user                = "martin"
password            = { env = "NAS_PASSWORD" }
root                = "/volume1/squirrel"
known_hosts_file    = "~/.ssh/known_hosts"      # validate the server host key (recommended)
host_key_algorithms = "ssh-ed25519 ssh-rsa"     # optional: pin accepted host-key algorithms
```

On an encrypted destination both map to the rclone sftp options of the same
name.

### S3 storage class

`storage_class` maps to rclone's s3 `storage_class` config key and accepts
whatever value the chosen s3-compatible backend supports (typically a default
tier plus one or more cheaper archive/cold tiers). Absent, the backend's default
class is used. Use the exact value string your provider documents.

```toml
[destinations.offsite]
type          = "s3"
# ...
storage_class = "<provider archive tier>"   # archive tiers cost less to store, more to read
```

## Choosing a layout

By default a destination **mirrors** the volume's tree. Any destination but
kopia can instead opt into an append-only, content-addressed or packed layout,
and any non-`local` destination can encrypt contents with a `crypt` block.
squirrel writes a `local` destination, and an `sftp` one without `crypt`, itself
in every layout; rclone writes the others. See:

- [Mirror (default)](/squirrel/layouts/mirror/)
- [Encrypted (crypt)](/squirrel/layouts/encrypted/)
- [Content-addressed](/squirrel/layouts/content-addressed/)
- [Packed](/squirrel/layouts/packed/)
- [Kopia](/squirrel/layouts/kopia/)

## Next steps

- [Configuration reference](/squirrel/reference/configuration/) — all destination
  keys in one table.
- [Syncing & first use](/squirrel/guides/syncing/) — the `--init` bootstrap.

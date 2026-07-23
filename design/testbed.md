# Testbed: the reference setup on one machine

How the [reference setup](reference-setup.md) is simulated on a single
development machine with real commands, real binaries, and no
containers. A squirrel "machine" is nothing but a config file, an
index database, a node name, and (usually) an agent listener — so five
machines are five processes on loopback ports.

## Mapping

| Reference | Testbed stand-in | Why it's faithful |
|---|---|---|
| nas, laptop, homepc, htpc | 4 × `squirrel agent`, each with its own config + db + `node_name`, on `127.0.0.1:750{1..4}` | Node identity lives in (config, db, name) — the code never cares that the "machines" share a kernel |
| LAN + SMB/NFS byte-paths | `[nodes.X] path` pointing at the peer's volume directory on local disk | `Node.Path` is an rclone-style prefix; a local absolute path is its simplest legal value |
| cloudbox (pure SFTP, no programs) | `rclone serve sftp --user u123456 --pass …` on `127.0.0.1:2222`, serving an empty dir | Exactly the product shape: an SFTP endpoint you cannot run code on. Presents a real host key, so the `known_hosts_file` UX is exercised too |
| s3archive | SeaweedFS `weed server -s3` on `127.0.0.1:8333` (same creds/config as `test/integration/s3config.json`) | Already the reference S3 endpoint for the integration tests; proven to produce the composite multipart ETags the fingerprint path depends on |
| kopia-mirror | `kopia` binary + a repo directory | Same as production: squirrel drives the CLI |
| usb disk | a plain directory; "unplugging" = renaming it | `local` destination + marker semantics don't care about hardware |

Deliberately not simulated: Synology/QNAP packaging (below squirrel),
b2/gcs (same shape as s3), real network partitions — a dead machine or
dark destination is simulated by stopping its process, which is also
what the failure looks like to the surviving side.

## Binaries via mise

The repo already pins rclone through `mise.toml`. The other two
testbed binaries install the same way — mise's `ubi` backend pulls
static binaries straight from GitHub releases, no plugins:

```toml
[tools]
rclone        = "1.74.1"
golangci-lint = "2.12.2"
"ubi:seaweedfs/seaweedfs" = { version = "3.80", exe = "weed" }
"ubi:kopia/kopia"         = "0.21.1"
```

(Exact pins to be finalized when the testbed lands; CI keeps using the
`test/integration/docker-compose.yml` fixture, which stays untouched —
the testbed is for interactive dogfooding in environments with or
without Docker.)

## Layout on disk

Everything under a git-ignored `.testbed/` at the repo root:

```
.testbed/
  nas/      config.toml  index.db  volumes/{photos,docs,media}/
  laptop/   config.toml  index.db  volumes/{photos,docs}/
  homepc/   config.toml  index.db  volumes/{photos,docs}/  usb/
  htpc/     config.toml  index.db  volumes/{media,photos}/
  cloudbox/ data/                  # rclone serve sftp root
  s3/       data/                  # weed -dir
  kopia/    repo/
  certs/    …                      # self-signed per agent + pinned fingerprints
```

Plus a small, seeded data generator: photo trees by year (a few large
"RAW" files above `pack_threshold`, many small JPEGs below it — so the
packed layout exercises both streams), docs, and a couple of
multi-GiB-shaped media files (sparse or truncated sizes where the test
only needs metadata realism).

## Time compression

Reference cadences are hours; the testbed cannot wait. Same config
keys, smaller values: `sync_every = "30s"`, `scan_interval = "2m"`,
`offload_max_evidence_age = "5m"`. Everything accepts a
`time.Duration`, so nothing needs code changes — and if a cadence
turns out to have an undocumented floor, that's a finding.

## The walk

The testbed exists to run the
[lifecycle checkpoints](reference-setup.md#lifecycle-checkpoints) in
order, as the user would: bootstrap each machine by hand *once*
(scripting the bootstrap away before feeling it would defeat the
purpose — the friction there is data), then let the agents run,
watching through `squirrel tui` on each node. Every rough edge goes
into a friction log with severity; failure injection (kill cloudbox's
process for a "month", corrupt an object in `s3/data`, rename the usb
dir, edit the same doc on two nodes between cadences) covers
checkpoints 6–8.

Once the manual walk has produced the friction log, a `testbed up`
script can automate the environment for regression-walking fixes —
the script encodes what we learned, not the other way around.

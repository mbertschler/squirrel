# The reference setup

The canonical household squirrel is designed for. It is deliberately
expansive: five machines, three volumes, and every supported target
type in play — four peer nodes, `local`, `sftp`, `s3`, and `kopia`
destinations, mirrored and packed layouts, crypt, offload with relayed
evidence. If the UX works here, the two-machine starter setup falls out
for free.

This is the yardstick for feature design, not just for the current UX
push: when proposing any change, place it in this household first —
which machine runs it, what it looks like from the other seats, and
what happens to it at each lifecycle checkpoint below.

## Machines

| Machine | Role | Storage | Squirrel role |
|---|---|---|---|
| **nas** | Synology/QNAP, always on, runs squirrel in a container | 8 TB | **Hub node.** Master copy of everything; receives peer-syncs; the only machine that talks to the offsites; runs the drift scans |
| **laptop** | Daily driver, roaming, often asleep or away | 512 GB | Edge node. Originates photos and docs; pushes to nas; offloads old photo years to reclaim space |
| **homepc** | Desktop at home, GUI, regularly on | 2 TB | Edge node. Originates photos and docs; pushes to nas; also mirrors to a USB disk (`local` destination) |
| **htpc** | Home theater PC on the TV, no GUI, always at home | 1 TB | Edge node, receive-only. Holds the media + photos it plays back; nas pushes to it; offloads what it no longer needs locally |
| **cloudbox** | Rented storage box: **pure SFTP access, cannot run programs** (Hetzner-Storage-Box-like) | 5 TB | `sftp` **destination** only — no agent, no index. Encrypted, browsable mirror: its names are the household's own paths, in clear |
| *(bucket)* **s3archive** | S3 bucket on an archive-ish tier | ∞ | `s3` **destination**, packed + crypt: the cold, cheap, append-only copy |

The two encrypted destinations disclose different amounts, and the split is
the layout's, not the encryption's. `cloudbox` is a mirror, so its tree is
browsable by design and the household's paths are legible to whoever runs the
storage box — that is the price of a copy you can restore with nothing but an
SFTP client. `s3archive` is packed, so every name under it is one squirrel
chose, and squirrel keys those names from the crypt passwords: the bucket
discloses no path, no volume name, and no content hash, so nobody holding a
candidate file can test whether the household stores it without first finding
the crypt passwords, at the same scrypt cost as attacking the encrypted data
itself. A household that considers its filenames sensitive should read
`cloudbox` as the weaker leg and lean on `s3archive` (or `kopia-mirror`) for
that property.

The Synology-vs-QNAP question dissolves at this level: both run the
agent as a container with the volumes bind-mounted; every difference
(package manager, mount paths, port exposure) sits below squirrel.
What *does* matter is the fork the table encodes: the cloudbox cannot
run programs, so it is a dumb **destination**; the NAS can, so it is a
**node** — and since most bytes live there, it is the hub.

## Topology

```
 laptop ──peer-sync──▶ ┌─────┐ ──rclone──▶ cloudbox  (sftp, crypt, mirror)
 homepc ──peer-sync──▶ │ nas │ ──rclone──▶ s3archive (s3, crypt, packed)
                       │     │ ──kopia───▶ kopia-mirror (2nd NAS volume)
 htpc  ◀──peer-sync──  └─────┘
   ▲                      │
   └──── durability evidence flows back out to the edges ────┘

 homepc ──native──▶ usb (local mirror, .squirrel-volume marker)
```

`native` is squirrel's own transport ([native-mirror.md](native-mirror.md)):
every `local` destination and every `sftp` one without crypt, in any layout.
`rclone` carries the encrypted destinations and the buckets.

Initiation direction follows availability: the intermittently-awake
machines (laptop, homepc) initiate toward the always-on nas; the nas
initiates toward the always-on htpc. A sleeping laptop can't be dialed,
so nothing ever tries.

## Volumes and flows

| Volume | Lives on | Flow | Offload |
|---|---|---|---|
| **photos** | laptop, homepc, htpc, nas (master) | laptop/homepc → nas → cloudbox + s3archive + kopia-mirror; nas → htpc for TV slideshows | laptop offloads old years once nas + s3archive hold them |
| **docs** | laptop, homepc, nas (master) | laptop/homepc → nas → cloudbox + s3archive + kopia-mirror; homepc also → usb | never — small enough to keep everywhere |
| **media** | nas (master), htpc | nas → htpc; nas → cloudbox + s3archive | htpc offloads watched items once s3archive holds them |

Offload gates may name only targets that *produce durability evidence*.
Four shapes do: content-addressed and packed destinations (presence+size,
upgraded to content-verified by the scan-back fingerprint), a mirror squirrel
writes itself on a local disk — usb — which reads every copy back through
BLAKE3 before committing it and advances as `fingerprint-verified`, peer nodes
the offloading machine itself pushes to (`peer-blake3`), and kopia
repositories (`kopia-verify`). Every other **mirror** never yields evidence
the gate accepts. The crypt mirror cloudbox is written by rclone: the overlay
hides the content hash, so rclone compares by size+mtime (friction log F21),
and a plain rclone mirror's `--checksum` compare runs under the first hash
both backends support, never against the index's BLAKE3 (#211). A native sftp
mirror reads nothing back, and squirrel never puts its paths — the volume's
own file names — on a server command line. None of those keeps a fingerprint a
verify pass could upgrade. Naming one in `offload_requires` is therefore
rejected at config load as an unsatisfiable policy — fail-early, not the
wait-forever gate the walk hit with the laptop gating on the crypt-mirror
cloudbox.

A receive-only node (htpc) cannot credit its *upstream* peer, so its gate
rests on the offsites the hub pushes to, reached via the durability pull.
Media therefore also rides to s3archive: an offloadable volume must live on
at least one evidence-producing target.

The offload story is the crown jewel and the reason this topology is
worth its complexity: the laptop never talks to cloudbox or s3archive,
yet its offload gate requires them. The evidence arrives relayed —
nas pushes, records durability vectors, and the laptop pulls them over
the peer API. Content origin coordinates survive the hop (the laptop's
photos stay attributed to `(laptop, run)` even when nas forwards them),
which is exactly what the version-vector gate needs.

## The kopia leg: implementation independence

Every squirrel copy in the household — nas, htpc, cloudbox, s3archive,
usb — is produced by the same walker, hasher, indexer, and upload
pipeline. A systematic squirrel bug (a walker silently skipping a
directory class, an indexer recording the wrong state) replicates
faithfully to all of them, and squirrel verifying itself cannot see it:
verification checks reality against squirrel's own beliefs. The
`kopia-mirror` destination is the one copy whose correctness does not
depend on squirrel being correct — a disjoint implementation walking
the same disk, with its own end-to-end verification (`snapshot
verify`) on every sync, recorded in squirrel's runs table like any
other destination.

For usb the shared walker is literal: a native mirror plans from the
index, as the content layouts always did, so a file the indexer misses
never reaches it. An rclone mirror walked the source itself, which let a missed file
still reach it; of the household's copies only cloudbox keeps that until
crypt moves to the native transport, and then none does. What an
independent walk covered is kopia-mirror's job alone, and it covers only
photos and docs. A periodic check comparing a plain directory walk with
the index is the candidate that would give every layout some of that
back (native-mirror.md, decision 7).

Because its purpose is *implementation* redundancy, not geo redundancy
(cloudbox and s3archive cover that twice), living on the nas's second
disk group is fine: independent, cheap, fast to restore from. It is
scoped to the irreplaceable volumes — photos and docs — and not to
media, which is large and replaceable.

The kopia leg is **first-class until squirrel is out of beta**, which
may take years. Dropping it is a deliberate amendment to this document
and requires, at minimum: point-in-time restore shipped, verification
running on an agent cadence, and years of proven operation.

## Per-machine configuration

Realistic sketches with real keys — these seed the testbed configs.
Secrets are `{ env = "…" }` throughout.

### nas — the hub

```toml
db        = "/volume1/squirrel/index.db"
node_name = "nas"

[agent]
listen        = "0.0.0.0:8443"
scan_interval = "168h"        # weekly drift scan over all volumes
scan_strategy = "shallow"
verify_every  = "168h"        # weekly offsite fingerprint re-check (F32),
                              # the agent-level default for every packed/CA
                              # destination (here: s3archive)
[agent.tls]
cert = "/volume1/squirrel/agent.crt"   # self-signed; peers pin the fingerprint
key  = "/volume1/squirrel/agent.key"
[agent.auth]
token = { env = "SQUIRREL_AGENT_TOKEN" }
[agent.auth.peers.laptop]
bearer = { env = "SQUIRREL_PEER_LAPTOP" }
[agent.auth.peers.homepc]
bearer = { env = "SQUIRREL_PEER_HOMEPC" }

[volumes.photos]
path       = "/volume1/photos"
sync_to    = ["cloudbox", "s3archive", "kopia-mirror", "htpc"]
sync_every = "6h"

[volumes.docs]
path       = "/volume1/docs"
sync_to    = ["cloudbox", "s3archive", "kopia-mirror"]
sync_every = "6h"

[volumes.media]
path       = "/volume1/media"
sync_to    = ["cloudbox", "s3archive", "htpc"]
sync_every = "24h"

[destinations.cloudbox]
type             = "sftp"
host             = "u123456.your-storagebox.example"
user             = "u123456"
password         = { env = "CLOUDBOX_PASSWORD" }
root             = "/squirrel"
known_hosts_file = "/volume1/squirrel/known_hosts"
[destinations.cloudbox.crypt]
password  = { env = "CLOUDBOX_CRYPT_PASSWORD" }
password2 = { env = "CLOUDBOX_CRYPT_SALT" }

[destinations.s3archive]
type              = "s3"
provider          = "AWS"
region            = "eu-central-1"
access_key_id     = { env = "AWS_ACCESS_KEY_ID" }
secret_access_key = { env = "AWS_SECRET_ACCESS_KEY" }
bucket            = "household-squirrel-archive"
root              = "/"
layout            = "packed"          # exercises objects/ + packs/ + maps
pack_threshold    = "1MiB"
pack_size         = "512MiB"
storage_class     = "GLACIER_IR"
[destinations.s3archive.crypt]
password  = { env = "S3_CRYPT_PASSWORD" }
password2 = { env = "S3_CRYPT_SALT" }

[destinations.kopia-mirror]
type     = "kopia"
root     = "/volume2/kopia-repo"      # second disk group: independent format
password = { env = "KOPIA_REPO_PASSWORD" }

[nodes.htpc]
endpoint = "https://htpc.home:8443"
[nodes.htpc.auth]
bearer = { env = "SQUIRREL_PEER_HTPC" }
[nodes.htpc.tls]
cert_fingerprint = "sha256:…"
```

### laptop — roaming edge

```toml
db        = "~/.squirrel/index.db"
node_name = "laptop"

[agent]
# No `listen`: this roaming laptop never receives peer syncs, so the agent
# runs the sync/index schedulers *without* an HTTP listener (F35). With no
# listener there is nothing to authenticate, so no [agent.auth] token either.

[volumes.photos]
path                     = "~/Pictures"
sync_to                  = ["nas"]
sync_every               = "1h"
offload_requires         = ["nas", "s3archive"]
offload_max_evidence_age = "720h"

[volumes.docs]
path       = "~/Documents"
sync_to    = ["nas"]
sync_every = "1h"

[nodes.nas]
endpoint = "https://nas.home:8443"
[nodes.nas.auth]
bearer = { env = "SQUIRREL_PEER_LAPTOP" }
[nodes.nas.tls]
cert_fingerprint = "sha256:…"
```

### homepc — edge with a USB mirror

As laptop (`node_name = "homepc"`, same two volumes → nas, hourly),
plus:

```toml
[volumes.docs]
path    = "~/Documents"
sync_to = ["nas", "usb"]

[destinations.usb]
type = "local"
root = "/media/usb-backup"            # .squirrel-volume marker guards this
```

### htpc — receive-only edge

```toml
db        = "/var/lib/squirrel/index.db"
node_name = "htpc"

[agent]
listen = "0.0.0.0:8443"               # receives nas's pushes
[agent.tls]
cert = "/var/lib/squirrel/agent.crt"
key  = "/var/lib/squirrel/agent.key"
[agent.auth]
token = { env = "SQUIRREL_AGENT_TOKEN" }
[agent.auth.peers.nas]
bearer = { env = "SQUIRREL_PEER_NAS" }

[volumes.media]
path                     = "/data/media"
offload_requires         = ["s3archive"]
offload_max_evidence_age = "720h"

[volumes.photos]
path = "/data/photos"

[nodes.nas]                            # not in any sync_to: exists so the
endpoint              = "https://nas.home:8443"  # htpc can *pull durability*
pull_durability_every = "24h"          # from nas on its own clock (F33), so the
                                       # media offload gate's relayed
                                       # s3archive evidence stays fresh with
                                       # zero typed commands
[nodes.nas.auth]
bearer = { env = "SQUIRREL_PEER_HTPC" }
[nodes.nas.tls]
cert_fingerprint = "sha256:…"
```

## Steady state: what runs untyped

Per [ux-principles](ux-principles.md#1-set-up-once-then-trust), after
bootstrap the household should run itself. Today's coverage:

| Loop | Automated today? |
|---|---|
| Index + sync on cadence (`sync_every`/`index_every`) | ✅ agent scheduler |
| Drift detection (`scan_interval`) | ✅ agent, hub |
| Index snapshots local + ride-along | ✅ on every successful sync |
| Durability pull after node sync | ✅ initiator side only |
| Offsite fingerprint re-check (`verify_every`) | ✅ agent, per-destination or an `[agent]` default (F32) |
| Durability refresh on a *receive-only* node (htpc) | ✅ agent, per-node `pull_durability_every` (F33) |
| Applying a config edit | ❌ still a manual agent restart; the *drift* is detected and latched automatically ([F9](friction-log.md)) |
| Offload | ❌ manual by design; the *readiness signal* should still be automatic ([F17](friction-log.md)) |

## Lifecycle checkpoints

The moments the testbed walk has to cover, in rough story order:

1. **Bootstrap day** — five configs written by hand; tokens generated
   and distributed; TLS certs created and fingerprints pinned;
   `sync --init` per fresh destination. How many steps, how many chances
   to get one silently wrong?
2. **First full push** — terabytes to two offsites over home upload
   bandwidth: interruptions, resume, progress visibility over days.
3. **Steady state** — a week of untyped operation; does the TUI answer
   "am I safe?" on each machine ([principle 3](ux-principles.md#3-the-tui-must-answer-am-i-safe-in-one-glance))?
4. **Return from a trip** — laptop reappears after three weeks with
   2 000 new photos; catch-up sync, evidence staleness
   (`offload_max_evidence_age`) recovery.
5. **Offload day** — laptop is full; old photo years go. Gate refusals
   must be legible; the relayed-evidence path must be visible, not
   mystical.
6. **Conflict** — the same document edited on laptop and homepc between
   syncs; `.squirrel-conflicts/` must be discoverable and resolvable.
7. **Scary moments** — a verify mismatch on s3archive; cloudbox dark
   for a month (evidence ages out, offloads freeze — correctly, but
   how does the operator find out *why*?); the USB disk left unplugged
   (marker refusal).
8. **Restore day** — the laptop dies; new laptop, restore docs + photos
   from nas. Separately: the nas dies; restore a volume *and its
   index* from cloudbox using the ride-along snapshot.

## Deliberate scope cuts

- **b2 / gcs** — same rclone-bucket interaction shape as s3; nothing
  new for the UX seat.
- **NAS vendor packaging** — container setup is below squirrel; we
  assume "can run a Docker container".
- **Desktop app** — explicitly later; nothing here may depend on it.

## Open questions

Friction and open questions are tracked exclusively in
[`friction-log.md`](friction-log.md) — the items formerly listed here
became F1, F3, F4, F34, and F35.

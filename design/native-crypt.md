# Native encryption: age, one content layout, and seekable packs

*Proposed.* This is step 3 of replacing rclone with squirrel's own code.
Peer sync dropped rclone in #210, #211 made the mirror's evidence honest, and
[native-mirror.md](native-mirror.md) (#217) moved every `local` and plain
`sftp` destination onto squirrel's transport. What still needs rclone is
encryption, and the s3, b2 and gcs backends. This design removes rclone's crypt
overlay. The S3 transport follows it on its own branch.

## Summary

- **One content layout.** The content-addressed layout is retired: it was the
  packed layout with every file stored as an object, which `pack_threshold = 0`
  still gives.
- **Encryption belongs to the packed layout.** A `crypt` block is accepted on
  a packed destination and refused on a mirror. An
  encrypted mirror needs a tool to read any file, yet its tree discloses every
  path, size and modification time. It also can't gate offload.
- **Squirrel encrypts every artifact itself, in the
  [age](https://c2sp.org/age) format** (`age-encryption.org/v1`), to hybrid
  post-quantum recipients (ML-KEM-768 with X25519). Recovery without squirrel
  needs the stock `age` command, zstd and tar.
- **Two recipients, both required.** The *agent identity* lives on the machine
  that pushes. The *recovery recipient* is a public key whose private half, the
  *recovery kit*, is printed once and kept offline. Every file opens with
  either one.
- **The naming key is random** and lives on the destination, in
  `.squirrel-naming`, encrypted to both recipients. It no longer comes from a
  password.
- **rclone never sees plaintext again.** `local` and `sftp` encrypted
  destinations become native. On s3, b2 and gcs, rclone moves ciphertext that
  squirrel produced, until the S3 transport replaces it.
- **Packs become seekable.** A pack is written as a run of zstd frames of about
  1 MiB. The placement map records each member's frame, so restoring one file
  reads one frame instead of the whole pack. Stock `zstd -d | tar -x` still
  reads a pack end to end.
- **Objects are padded** with Padmé before encryption, so a large file's size
  shows only to within a few percent. It costs about 1% of storage on a
  media-heavy archive.

## Why not rclone's format

rclone crypt did the job, but squirrel inherited its limits:

- **Truncation goes unnoticed.** The format has no end-of-file marker, so a
  file cut at a 64 KiB block boundary decrypts cleanly as a shorter file.
  Objects are saved by squirrel's BLAKE3 check on restore; manifest segments,
  placement maps and snapshots only by parsing.
- **One key for everything, derived from a password** with fixed scrypt
  parameters. Anyone holding the ciphertext can guess passwords offline, and a
  password change means re-encrypting every file.
- **One implementation.** The format is specified by rclone's documentation and
  read by rclone.
- **Source modification times leak.** rclone carries each file's mtime onto
  the stored copy.

age fixes each of these:

- It has a frozen, published specification, test vectors, several independent
  implementations, and a command-line tool in most distributions.
- Its payload is ChaCha20-Poly1305 in 64 KiB chunks, with a fresh key per file
  and a flagged last chunk, so truncation fails authentication.
- ChaCha20 is fast on NAS processors without AES instructions.
- It encrypts each file to any number of recipients.
- Since v1.3 it has post-quantum recipients. Backups are the data a
  "record now, decrypt later" attacker wants.

Considered and rejected: libsodium's secretstream in a format of squirrel's own
(better context binding, but squirrel would own a format and its recovery tool),
Tink streaming encryption (keyset machinery built for services, no general
decrypt command), OpenPGP (GnuPG and RFC 9580 modes don't interoperate, large
attack surface), and staying byte-compatible with rclone crypt (the weaknesses
above). Kopia bundles content addressing, packing and encryption the same way
this design does; squirrel keeps what kopia doesn't offer: recovery with stock
tools, and BLAKE3 evidence that gates offload.

## Scope

**In scope:**

- retiring the content-addressed layout;
- the `crypt` block's new shape, and its refusal on mirrors and kopia;
- the encryption layer under both artifact stores (the transport, and rclone as
  a byte mover);
- keys: generation, the recovery kit, recipient pinning, and changing
  recipients;
- `.squirrel-naming` and the names derived from it;
- evidence, verify, restore and recover on encrypted destinations;
- Padmé padding of objects;
- seekable packs, encrypted or not, with ranged reads on every transport;
- the recovery procedure without squirrel.

**Out of scope:**

- **The S3 transport.** It follows on its own branch and replaces
  `rclone rcat`/`rclone cat` underneath the artifact store.
- **Rewrapping existing artifacts** to a changed recipient set (open
  question 1).
- **Mirrors, kopia, and peer sync.** An encrypted mirror is refused; kopia
  encrypts its own repository; peer sync runs between machines that hold the
  plaintext anyway.

In the [reference setup](reference-setup.md), `cloudbox` becomes an encrypted
packed destination written natively over sftp, and `s3archive`
stays packed and encrypted, with rclone moving its ciphertext.

## What changes for the operator

- **The `crypt` block names keys, not passwords:**

  ```toml
  [destinations.offsite.crypt]
  identity = { env = "SQUIRREL_OFFSITE_IDENTITY" }  # AGE-SECRET-KEY-PQ-1…, the agent's key
  recovery = "age1pq1…"                             # public; its private half is the recovery kit
  ```

  `password`, `password2` and `obscured` are gone. Several destinations may
  name the same keys.
- **`squirrel destination keygen`** creates both keys. It shows the recovery
  kit, asks for it back to prove it was stored, and prints the block above.
- **Encrypted mirrors are refused** at config load, with a message naming the
  packed layout.
- **`layout = "content-addressed"` is refused** at config load, naming
  `layout = "packed"` with `pack_threshold = 0` as the way to keep an object
  per file.
- **`local` accepts `crypt`,** so an encrypted USB disk works.
- **Existing encrypted and packed destinations must start over.** Squirrel
  isn't in production, so there is no reader for the rclone format or for
  single-frame packs: point the destination at a fresh root, or wipe it and run
  `destination reset`. A database that already records packs starts over; the
  migration refuses it.
- **Recovery without squirrel** needs age v1.3 or newer (older clients need the
  `age-plugin-pq` compatibility plugin), zstd and tar. The procedure is in
  [formats](../docs/src/content/docs/reference/formats.md).
- **An encrypted destination discloses less:** no source modification times,
  and no path on any layout. Object sizes show only to within a Padmé bucket.
  Counts and timing stay visible (section 1).

## 1. What an encrypted destination holds

Every file squirrel writes under an encrypted root is an age file. Names are
fixed, keyed, or carry only a run id:

```
<root>/
  .squirrel-naming                             # age: scheme, naming key, recipient set
  objects/<name("object", blake3)>             # age: the content, padded
  packs/<name("pack", pack key)>               # age: zstd frames of a tar
  packs/map-<run>                              # age: placement map (JSONL)
  <name("volume", volume)>/.squirrel-volume    # age: volume marker
  <name("volume", volume)>/index/run-<run>     # age: manifest segment (JSONL)
  <name("volume", volume)>/.squirrel-index/index-<time>-run-<run>.db
                                               # age: ride-along index snapshot
  <name("volume", volume)>/.squirrel-staging/run-<run>/<hex>
                                               # age: in-flight artifacts
```

- **Every file is encrypted to exactly the pinned recipient set**: the agent
  recipient and the recovery recipient, both `mlkem768x25519`. age refuses to
  mix post-quantum and classic recipients in one file, so a classic key can't
  be added by mistake.
- **No armor.** Files are binary.
- **Context binding.** age authenticates a file, not its name. Objects and
  packs are bound by content: after decryption they must hash to the BLAKE3 or
  pack key their name was derived from. Segments, maps, snapshots and markers
  carry their volume and run id inside the plaintext, and a reader refuses one
  whose inner id disagrees with its name. That stops a server from replaying
  an old segment under a newer run's name.
- **Modification times** are the landing time, never the source file's.
- **Sizes.** Every file costs about 3.2 KB of header (two post-quantum
  stanzas), a 16-byte nonce, and 16 bytes per 64 KiB. Files below
  `pack_threshold` show only as part of a pack's size; objects are padded
  (below). Counts, run ids and upload times stay visible.

### Padding objects

Keyed names hide which content an object holds, but an exact size can still
identify a known file: a 4.37 GB file is recognizable by size alone. Every
object is therefore padded before encryption with **Padmé**, a padding rule
from Nikitin et al., "Reducing Metadata Leakage from Encrypted Files and
Communication with PURBs" (PETS 2019). age itself doesn't pad.

```
E = floor(log2 L)          # the power of two the length L falls in
S = floor(log2 E) + 1      # bits of precision kept
padded = L rounded up to a multiple of 2^(E - S)
```

- **What it hides:** a size reveals about log2 log2 L bits instead of
  log2 L. There are 64 possible sizes between 4 and 8 GiB: the 4.37 GB file
  becomes 4,429,185,024 bytes, in a 64 MiB bucket shared with many other files.
- **What it costs:** at most 3.1% (about 1.5% on average) for objects between
  64 KiB and 4 GiB, at most 1.6% (about 0.8%) above 4 GiB. The rule's 11%
  worst case needs a file a few bytes long, and objects start at
  `pack_threshold` unless it is set to 0. A media-heavy archive pays about 1%
  overall.
- **The padding is zero bytes after the content, inside the age payload.** The
  manifest's `size_bytes` says where the content ends, so recovery is
  `age -d … | head -c <size>`, and the BLAKE3 check catches a forgotten `head`.
- **Only objects are padded.** A pack's size is already the compressed sum of
  its members, and segments, maps, snapshots and markers disclose nothing a
  size would add.
- Padding is always on, like every privacy property, with no key to turn it
  off.

## 2. Keys

| Key | Secret | Where it lives | Used for |
|---|---|---|---|
| agent identity | yes | the pushing machine's config: a literal or `{ env = … }` | the write-time decrypt check, adoption, reading `.squirrel-naming` and markers, restore, recover |
| recovery recipient | no | config | encrypting every file |
| recovery kit (its identity) | yes | offline: paper or a password manager | disaster recovery, the recovery drill, changing recipients without the agent identity |
| naming key | yes | `.squirrel-naming` on the destination | deriving object, pack and volume names |

- **Config load refuses** a `crypt` block missing either key, a recipient that
  isn't `mlkem768x25519`, or a recovery recipient equal to the agent's own.
  Both keys are required because the most dangerous encrypted backup is one
  nobody can open. The cost is that setup doesn't finish until the kit is
  stored. That is a safety property, so it has no flag.
- **The agent recipient is derived** from the identity; config never lists it.
- **The identity is 256 random bits,** so it can't be guessed offline.
- **A second hardware recipient isn't possible yet.** age's hybrid recipient
  for hardware keys (`age1tagpq1…`) exists, but `age-plugin-yubikey` doesn't
  produce it (checked 2026-09-26), and a classic `age1tag1…` can't share a file
  with post-quantum recipients.

### `.squirrel-naming`

- The first `--init` push to an empty root generates a random 32-byte naming
  key and writes `.squirrel-naming`: an age file, encrypted to both recipients,
  holding the scheme version, the naming key and the recipient set.
- Every push reads and decrypts it (one small read). Names come from
  `name(domain, x) = hex(BLAKE3_keyed(naming_key, domain || 0x00 || x))` as
  today; only the key's source changes.
- The key is also kept in the local store, in a new `STRICT` table keyed by
  destination, and so rides along inside every (encrypted) index snapshot.
- A root holding anything else without the marker is refused, as today. So is
  a marker that isn't an age file (rclone era), or one that decrypts to an
  unknown scheme.
- **A missing marker is repaired** as today: when the root still holds this
  volume's keyed directory and the store knows the key, the push writes the
  marker back. Without a stored key, the push refuses and points at
  `recover --from`, which finds the key in a snapshot.
- **If every copy of the key is gone,** the recovery kit still recovers
  everything: decrypt every file, recognize segments and maps by their
  content, and rebuild the name→hash mapping by hashing what the objects
  decrypt to. That is slow but complete, and a full recovery downloads
  everything anyway.

### Recipient pinning and `destination rekey`

- The recipient set in `.squirrel-naming` is authoritative. A push whose
  configured set differs is refused, naming both sets, so a config edit can't
  silently drop the recovery recipient from new files.
- **`squirrel destination rekey <destination>`** is the explicit change. It
  decrypts the marker with the agent identity, or with the recovery kit
  supplied on stdin, and rewrites it encrypted to the newly configured set. It
  leaves an audit run.
- Rekeying changes the recipients of *new* files only. Existing artifacts keep
  the stanzas they were written with (open question 1).

| What is lost | What still works | What to do |
|---|---|---|
| agent identity (the hub died, its config with it) | every file opens with the recovery kit | `recover --from` with the kit, run `keygen` for a new agent identity, then `rekey`. Restoring artifacts written before the rekey takes the kit (`--identity`). |
| recovery kit | the agent pushes, verifies and restores as before | run `keygen` for a new kit, then `rekey`. Files written before the rekey open only with the agent identity until rewrapped. |
| both | nothing | by design, there is no back door |

## 3. Writing an artifact

The encryption layer sits in the one place every artifact passes through: the
landing in `artifactStore`. A layout never sees ciphertext.

**Staging** streams the source through a single pass:

```
source ─▶ BLAKE3 (drift check) ─▶ pad (objects) ─▶ age encrypt ─┬─▶ BLAKE3 + server hash ─▶ sink
                                                                └─▶ age decrypt (agent identity) ─▶ strip ─▶ BLAKE3
```

- **The drift check** is today's: the plaintext must hash to the indexed sum.
- **Padding** follows the content of an object; the in-line check strips it by
  the known size before hashing.
- **The in-line decrypt check** proves the bytes sent open with the agent
  identity and yield that same sum. It costs CPU only, no extra I/O.
- **The ciphertext BLAKE3** becomes the artifact's expected fingerprint. The
  landing confirms it: a local disk reads it back, an sftp server with a hash
  command hashes the staged ciphertext.
- **The recovery stanza can't be checked by the agent,** which doesn't hold
  the kit. What covers it: age's own test vectors, the pinned recipient set,
  the kit's confirmation at `keygen`, and the recovery drill (section 5).

**Sinks:**

- **Transport:** `Put` of the staged name, then flush, confirm and rename, as
  `transportArtifacts` does now.
- **rclone** (s3, b2 and gcs, until the S3 transport): the ciphertext streams
  to `rclone rcat`, then the landing is confirmed by size. No rclone crypt
  remote is configured anywhere.

**Adoption.** An artifact already at its name has someone else's ciphertext,
since age draws a fresh file key each time. It is adopted only if it decrypts
with the agent identity to the expected BLAKE3. Its ciphertext fingerprint is
then recorded. Anything else is refused, and squirrel never replaces it.

**Packs** are hashed before encryption, so the pack key and a retry within a
run stay reproducible even though the ciphertext differs each time.

## 4. Evidence and verification

Verification never decrypts. It compares ciphertext against the fingerprint
recorded when the file landed, so it works without the kit and doesn't have to
download every file.

| Destination | Evidence at landing | Verify pass |
|---|---|---|
| `local` + crypt | read-back of the ciphertext, plus the in-line decrypt check: `fingerprint-verified` | re-hashes a rotating slice of ciphertext |
| `sftp` + crypt, with a hash command | the server's hash of the ciphertext: `fingerprint-verified` | the server re-hashes |
| `sftp` + crypt, without one (cloudbox) | `presence+size` | presence and size |
| s3, b2, gcs + crypt | the provider's fingerprint, captured as today and compared with the ciphertext hash squirrel computed while sending, when the algorithms match | provider fingerprints from the listing |

- **Squirrel knows the expected ciphertext hash** before any capture, because
  it produced the ciphertext. That is stronger than today, where the
  fingerprint is whatever the provider reports afterwards. Computing MD5,
  SHA-1 or SHA-256 alongside BLAKE3 while sending is cheap. While rclone moves
  the bytes to s3, b2 and gcs, provider fingerprints are captured after the
  upload, as today; the S3 transport will send its own checksums with each
  upload.
- **This closes SAFETY-AUDIT D2** wherever the fingerprint is confirmed at
  landing. The in-line check proves the ciphertext squirrel produced decrypts
  to the right content; the confirmed fingerprint proves the destination holds
  exactly that ciphertext. Together they give decrypt-correctness without
  downloading anything.
- **An archive tier is verified from listings,** never thawed.
- **Decryption happens only** in the write-time check, adoption, reading markers
  and `.squirrel-naming`, restore, recover and the drill.
- `cloudbox` moves from "can't gate offload" (an rclone crypt mirror) to
  gating at `presence+size`, like any content destination on a server that
  runs no programs.

## 5. The recovery kit and the drill

- **`squirrel destination keygen`** generates an agent identity and a recovery
  identity and prints the recovery kit: the identity string (about 80
  characters) with a short note on what it opens and where the procedure lives.
  It then asks for the kit back and compares it, so a transcription mistake
  shows up while it is still harmless. Only then does it print the `crypt`
  block. `--no-confirm` skips the check for scripted setups and says so.
- **`squirrel verify --recovery-kit <file|-> [<destination>]`** is the drill.
  It decrypts `.squirrel-naming`, the newest segment of each volume and a
  sample of recent artifacts with the kit alone, and leaves an audit run.
  Without a destination it drills every encrypted destination whose recovery
  recipient the kit matches, so destinations sharing a kit take one drill. A
  password manager can feed it: `<read the kit> | squirrel verify --recovery-kit -`.
- **The drill is asked for monthly.** Only a human can prove the kit still
  exists and still opens the destination, since the agent never holds it. This
  is the one routine act squirrel asks of a person, an exception recorded in
  [ux-principles](ux-principles.md) §1.
  - `recovery_drill_every` in the `crypt` block sets the cadence: `720h` by
    default, counted from the last drill, or from the destination's first push
    when there was none.
  - When it is due, `status` and the TUI show the destination as needing
    attention, naming the command, and every push run for it carries a warning.
    It is a reminder, not a latch: it blocks nothing, turns nothing red, and
    clears with the next drill.
  - `recovery_drill_every = "off"` opts out. `status` and the TUI then say
    "recovery drill: off" for the destination, so the choice stays visible.
- The kit's confirmation at `keygen` and the pinned recipient set cover the
  recovery stanza's correctness; the drill covers the kit still being there.

## 6. Seekable packs

Today a pack is one zstd frame over a whole tar of up to 512 MiB, and the
placement map records offsets in the uncompressed tar. Restoring one member
downloads the pack and decompresses it from the start.

**Format:**

- The pack writer closes a zstd frame after the member that brings the frame's
  uncompressed bytes to 1 MiB or more, and starts the next member in a new
  frame. A member above 1 MiB (up to `pack_threshold`) sits alone in its frame.
  The tar's end-of-archive blocks close the last frame.
- Concatenated frames are a valid zstd stream, so `zstd -d | tar -x` reads a
  pack as before.
- Frame boundaries depend only on member order and sizes, and encoder
  concurrency stays pinned to one. The pack key, the BLAKE3 of the compressed
  bytes, stays reproducible.
- **Placement map** lines gain three fields. `offset` and `length` keep their
  meaning: the member's data in the uncompressed tar.

  ```json
  {"blake3":"26e7…e5ad","pack":"9f3a…1c02","offset":1049600,"length":123,
   "frame_offset":402112,"frame_length":381903,"frame_start":1048576}
  ```

  `frame_offset` and `frame_length` are the frame's compressed byte range in
  the pack; `frame_start` is where the frame's bytes begin in the uncompressed
  tar. The member's data is at `offset - frame_start` in the decompressed frame.
- **Schema:** a forward migration rebuilds `pack_members` with
  `frame_offset`, `frame_length` and `frame_start` as `NOT NULL` columns. No
  packed destination is in use, so there is no reader for single-frame packs:
  the migration refuses a database that already records packs, instead of
  deleting anything, and such a database starts over.

**Reading:**

- The transport gains a ranged read: `GetRange(ctx, name, offset, length)`.
  Local disks and sftp read at an offset; rclone uses `cat --offset --count`;
  the S3 transport will send a range request.
- On an encrypted pack, age's `DecryptReaderAt` maps the frame's plaintext range
  onto the 64 KiB chunks that cover it and fetches only those.
- **Restore chooses per pack:** ranged reads of the wanted frames (adjacent ones
  merged) when they add up to less than half the pack's compressed size, one
  end-to-end stream otherwise.
- **Objects** are stored whole, so an encrypted object is already seekable
  through `DecryptReaderAt`.

**Cost:** each frame starts compressing from scratch, so small files compress
somewhat worse. The testbed benchmark measures the ratio on `pics` and `trip`
against today's single frame. If the loss is material, the frame target grows;
it is a constant, not a config key.

On Glacier-style tiers a read first thaws the whole object, so the gain there
is download volume, not retrieval cost. On sftp and on S3 tiers with immediate
access, restoring one photo reads about a megabyte instead of up to 512 MiB.

## 7. Restore and recover

- **Restore** decrypts with the agent identity. `--identity <file|->` supplies
  another, typically the recovery kit after a rekey. A key is never accepted as
  a command-line argument.
- **`recover --from`** needs an identity to read `.squirrel-naming`, the
  markers and the snapshot. On a rebuilt machine that is usually the kit, given
  through `--identity`.
- **Recovery without squirrel** ([formats](../docs/src/content/docs/reference/formats.md)
  is rewritten):
  1. `age -d -i kit.txt .squirrel-naming` gives the naming key.
  2. Derive the volume directory's name, replay its segments (each decrypted
     with `age -d`), and resolve a path to its BLAKE3.
  3. Derive the object's name and run
     `age -d -i kit.txt objects/<name> | head -c <size_bytes> > file`.
     For a packed file: `age -d -i kit.txt packs/<name> | zstd -d | tar -xO <blake3>`.
  4. The recovered bytes hash back to the BLAKE3, so recovery checks itself.

## 8. Configuration and dispatch

- **`crypt`** takes `identity` (a secret: a literal or `{ env = … }`),
  `recovery` (an `age1pq1…` string) and `recovery_drill_every` (a duration or
  `"off"`, default `720h`). Allowed on `local`, `sftp`, `s3`, `b2` and
  `gcs` with `layout = "packed"`. Refused on a mirror and on kopia.
- **`layout`** accepts `mirror` and `packed`. `pack_threshold = 0` is valid
  and stores every file as an object. The content-addressed handler, its
  constant and its tests go; the planner, the artifact stores and
  `remote_objects` stay, because packed objects use them.
- **`Destination.Native()` stops depending on crypt:** `local` and `sftp` are
  native with or without it. Encrypted s3, b2 and gcs destinations use
  `rcloneArtifacts`, with the encryption layer in front.
- **rclone.conf** renders no crypt sections. The obscuring of crypt passwords
  and `CryptRemoteName` go away.
- **`hash_algo`** values valid only behind crypt (`crc32`, `xxh3`, `xxh128`)
  go away. On native sftp, the server hash runs over ciphertext, and its
  allowed values stay `md5`, `sha1`, `sha256` and `blake3`.
- **The encrypted mirror's size-and-mtime comparison** and its shallow run
  recording go away with the encrypted mirror.

## 9. What we give up

- **rclone-readable encrypted destinations.** `rclone mount` or `rclone copy`
  of a crypt remote no longer decrypts anything. With keyed names, nothing was
  browsable anyway.
- **The one-command restore of an encrypted mirror.** Recovering an encrypted
  destination without squirrel follows the procedure above.
- **Password-only recovery.** The kit is a random key that must be stored; it
  can't be remembered. What we gain is that it can't be guessed.
- **Existing encrypted and packed destinations.** They must be pushed again.
- **About 1% of storage** for padding objects.

## 10. What the tests must cover

- **Format:** a file squirrel writes decrypts with the age library alone,
  using either identity. Test vectors pin the naming derivation and the
  `.squirrel-naming` layout.
- **Recovery with the kit alone:** a full restore of a volume from each layout,
  using only the recovery identity and the destination.
- **Tampering:** a truncated artifact, a swapped object, a segment replayed
  under a newer run's name, and a marker from another root are each refused,
  not restored.
- **Recipient drift:** a configured set that differs from the marker's is
  refused; `rekey` changes it and leaves an audit run.
- **The in-line decrypt check:** an encryptor wired to a wrong recipient fails
  the landing before anything is recorded.
- **Adoption:** foreign ciphertext at an artifact's name is adopted only when
  it decrypts to the expected content, and never replaced.
- **Config:** crypt on a mirror or kopia, a classic recipient, a missing
  recovery recipient, and `layout = "content-addressed"` are each refused at
  load. `pack_threshold = 0` stores every file as an object.
- **Padding:** every object's stored size is its Padmé bucket plus the
  fixed age overhead, restore and the recovery procedure strip it, and a
  missing strip fails the BLAKE3 check.
- **The drill reminder:** it appears once the cadence passes, on `status`, the
  TUI and push runs; a drill clears it; `"off"` replaces it with the visible
  note; and one drill covers every destination sharing the kit.
- **Seekable packs:** a ranged restore equals a streamed restore member for
  member, frame boundaries are reproducible, and the migration refuses a
  database that records packs. The crash and model suites from #217 run over encrypted
  destinations on both transports.

## 11. Documents to amend

In the pull request that implements this, each described as it then is:

- **Docs:** `layouts/encrypted.md` (rewritten), `reference/formats.md` (the
  naming key's source, the recovery procedure, the placement map's frame
  fields), `reference/on-disk-layout.md`, `reference/configuration.md`,
  `reference/cli.md` (`keygen`, `rekey`, `verify --recovery-kit`,
  `--identity`), the content-addressed page and its sidebar entry removed,
  every page and design document that names that layout, the packed, mirror
  and kopia layout pages,
  the verification, restore, syncing, recovery and offloading guides,
  `configuration/destinations.md`, `configuration/index-snapshots.md`,
  `index.mdx`, and `README.md`.
- **Design:** `reference-setup.md` (cloudbox's row as encrypted packed, the topology arrows, the
  disclosure paragraph, and cloudbox joining the gate-eligible targets),
  `testbed.md`, and this document's status. `ux-principles.md` §1 already
  records the drill's exception, in the pull request that proposes this design.
- **Audit documents:** SAFETY-AUDIT D2 closed, and the crypt remarks in its
  summary and in D1; friction-log F12, F21 and F31 annotated.

## 12. Order of work

One branch, one draft pull request, sessions in order:

1. **Keys, the marker and the native encryption layer.** It starts by retiring
   the content-addressed layout, so the rest of the work targets one content
   layout. Then the `crypt` block,
   `keygen`, `.squirrel-naming` and the stored naming key, recipient pinning,
   the encryption layer with object padding in `transportArtifacts`, and the
   evidence rows for `local` and `sftp`. The shared content-addressed and
   packed test fixtures are encrypted sftp destinations behind the fake rclone
   shim today (about 80 uses); they become packed fixtures on the native
   transports first, so every later step tests the real path.
2. **rclone as a byte mover, restore and recover.** The encryption layer in
   front of `rcloneArtifacts` (`rcat`), removal of rclone crypt, restore and
   recover with `--identity`, `rekey`, and the recovery procedure in
   formats.md.
3. **Seekable packs.** The frame writer, the placement map fields and the
   migration, `GetRange` on every transport and on rclone, and the restore
   strategy.
4. **Drill, hardening and wrap-up.** `verify --recovery-kit`, the tampering
   suite, the docs framing review, reference-setup.md, the testbed walk, and the
   benchmark (encryption throughput and the compression ratio of framed packs).

## 13. Decisions and open questions

Decided on 2026-09-26:

1. **Crypt only on the packed layout.** Encrypted mirrors are refused;
   `cloudbox` becomes packed. Over sftp with real latency, many small files
   push several times slower than their bytes justify, and packs also hide the
   sizes and count of small files.
2. **The age format,** not byte-compatible with rclone crypt.
3. **Post-quantum hybrid recipients only** (`mlkem768x25519`).
4. **Two recipients from the start, both required:** the agent identity and
   the recovery recipient.
5. **The agent holds its identity.** Verification runs on ciphertext;
   decryption is for the write-time check, adoption, restore and recovery.
6. **A random naming key,** stored in `.squirrel-naming` and encrypted to both
   recipients.
7. **Seekable packs** are part of this redesign.
8. **The recovery drill is asked for monthly,** as a warning on routine
   surfaces, with a per-destination opt-out that stays visible. It is the one
   exception to "no routine act by hand", recorded in ux-principles.md.
9. **Objects are padded with Padmé,** always, for about 1% of storage.
10. **The content-addressed layout is retired.** It was packed with
    `pack_threshold = 0`; one content layout halves the work of every session,
    and nothing is in production to migrate. What it gave up: one-command
    recovery per file (now `head -c` on an object, or a pack extraction), and
    a future retention policy that deletes objects instead of rewriting packs.

Open:

1. **Rewrapping existing artifacts** after a rekey. age's detached-header API
   can rewrap a file key without re-encrypting the payload, but each artifact
   is still rewritten whole. Needed only when a key is lost, so it can wait for
   a real case.

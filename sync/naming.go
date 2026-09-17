package sync

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/volmark"
)

// Artifact-name domains. Each is mixed into the keyed hash so the same
// input under two roles — a content hash and a pack key of identical
// bytes — derives two different names, keeping the objects/, packs/, and
// per-volume namespaces independent of one another.
const (
	nameDomainObject = "object"
	nameDomainPack   = "pack"
	nameDomainVolume = "volume"
)

// NamingMarkerName is the reserved file at the root of a destination whose
// artifact names are keyed. It records the naming scheme the root's
// artifacts were written under, so a root written one way is never mixed
// with artifacts named another.
const NamingMarkerName = ".squirrel-naming"

// namingSchemeKeyed is the scheme recorded for keyed BLAKE3 artifact names.
// It is compared verbatim, so a future scheme refuses an existing root
// instead of writing a second naming generation into it.
const namingSchemeKeyed = "keyed-blake3-v1"

// namingMarker is the parsed content of NamingMarkerName. It rides the
// crypt overlay like every other artifact, so it is encrypted at rest, and
// it records the scheme alone: the key stays re-derivable from the crypt
// passwords (config.DeriveNamingKey), so an archive's recoverability rests
// on the secret its operator already keeps rather than on a file at the
// destination.
type namingMarker struct {
	Naming    string `json:"naming"`
	CreatedAt string `json:"created_at,omitempty"`
}

// namer resolves the basenames of one destination's artifacts under the
// naming scheme its root is actually written in. A destination's *own*
// configuration says what this binary would write (namerFor); what is
// already at a given root can differ, so the read paths resolve it against
// the root instead (resolveNamer) and address what is there.
//
// Keeping the scheme in a value rather than re-deriving it from the
// destination at each call site is what makes that distinction impossible
// to get wrong by accident: a caller cannot name an artifact without having
// said which scheme it means.
type namer struct {
	dest  *config.Destination
	keyed bool
}

// namerFor is the naming scheme dest writes under: keyed for an encrypted
// append-only destination, content-hash names otherwise.
func namerFor(dest *config.Destination) namer {
	return namer{dest: dest, keyed: dest.HidesArtifactNames()}
}

// legacyNamerFor is the content-hash scheme every destination used before
// keyed naming existed, for addressing a root written back then.
func legacyNamerFor(dest *config.Destination) namer {
	return namer{dest: dest, keyed: false}
}

// object is the basename of one content object: a keyed name under the
// keyed scheme, the content's own BLAKE3 hex otherwise.
func (n namer) object(contentHash []byte) string {
	if !n.keyed {
		return hex.EncodeToString(contentHash)
	}
	return n.keyedName(nameDomainObject, contentHash)
}

// pack is the basename of one pack, named the same way as object. A pack
// key already discloses no single file, but naming it keyed keeps one rule
// for the whole root.
func (n namer) pack(packKey []byte) string {
	if !n.keyed {
		return hex.EncodeToString(packKey)
	}
	return n.keyedName(nameDomainPack, packKey)
}

// volumeDir is the per-volume directory holding that volume's manifest
// segments and ride-along index snapshots. Keying it is what stops the
// remote from disclosing the volume names themselves.
func (n namer) volumeDir(volumeName string) string {
	if !n.keyed {
		return volumeName
	}
	return n.keyedName(nameDomainVolume, []byte(volumeName))
}

// keyedName derives one artifact name: the keyed BLAKE3 of domain, a NUL
// separator, and input, under the destination's naming key, as lowercase
// hex. Deterministic, so identical content still derives one name and
// uploads once; unforgeable without the key, so the name discloses nothing
// about what it stands for.
func (n namer) keyedName(domain string, input []byte) string {
	h, err := blake3.NewKeyed(n.dest.Crypt.NamingKey[:])
	if err != nil {
		// NewKeyed rejects only a key that is not 32 bytes, and NamingKey
		// is a [32]byte, so reaching this is a programming error rather
		// than a runtime condition callers could handle.
		panic("sync: invalid artifact naming key: " + err.Error())
	}
	material := make([]byte, 0, len(domain)+1+len(input))
	material = append(material, domain...)
	material = append(material, 0)
	material = append(material, input...)
	_, _ = h.Write(material)
	return hex.EncodeToString(h.Sum(nil))
}

// rootMarkerNames are the basenames a layout guard discounts when deciding
// whether a root is fresh: squirrel's own markers, each written by a gate
// that runs before the guard. The naming marker joins the list only for a
// destination that writes one.
func rootMarkerNames(dest *config.Destination) []string {
	if dest.HidesArtifactNames() {
		return []string{volmark.MarkerName, NamingMarkerName}
	}
	return []string{volmark.MarkerName}
}

// rootNaming is what a destination root turns out to be written under.
type rootNaming int

const (
	// rootKeyed: the root carries a marker recording this binary's scheme.
	rootKeyed rootNaming = iota
	// rootLegacy: the root holds files but no marker, so it was written
	// before encrypted destinations keyed their names — content-hash
	// basenames, in clear.
	rootLegacy
	// rootFresh: nothing is there yet, so a push may claim it.
	rootFresh
)

// probeRootNaming classifies dest's root by what is actually at it. The
// marker is authoritative where it exists; absent, an empty root is fresh
// and a populated one is legacy. Only an encrypted append-only destination
// can be anything but keyed-by-definition, so every other destination
// answers rootKeyed and the callers' own naming (content-hash) applies
// unchanged.
//
// Read-only, so the read paths and a dry run can all ask it.
func probeRootNaming(ctx context.Context, rcl *Rclone, dest *config.Destination) (rootNaming, error) {
	if !dest.HidesArtifactNames() {
		return rootKeyed, nil
	}
	uri := remoteSubpathURI(dest, NamingMarkerName)
	present, err := rcl.statRemoteExists(ctx, uri, checkersArgs(dest)...)
	if err != nil {
		return 0, fmt.Errorf("destination %q: stat %s at %s: %w", dest.Name, NamingMarkerName, uri, err)
	}
	if present {
		return rootKeyed, validateNamingScheme(ctx, rcl, dest, uri)
	}
	if freshStartOnEmptyRoot(ctx, rcl, dest, NamingMarkerName) {
		return rootFresh, nil
	}
	return rootLegacy, nil
}

// resolveNamer is how every read path names an artifact: under the scheme
// the root is written in, not the one this binary would write.
//
// An encrypted archive uploaded before keyed naming exists stays fully
// readable — restore, verify, and snapshot discovery address its
// content-hash names — because a hash ever observed must stay retrievable,
// and a squirrel upgrade must not be the thing that strands an archive.
// Writing is where the two schemes are kept apart (ensureNamingScheme
// refuses to add keyed names to such a root), so a root only ever holds
// one scheme and reading it is unambiguous.
func resolveNamer(ctx context.Context, rcl *Rclone, dest *config.Destination) (namer, error) {
	root, err := probeRootNaming(ctx, rcl, dest)
	if err != nil {
		return namer{}, err
	}
	if root == rootLegacy {
		return legacyNamerFor(dest), nil
	}
	return namerFor(dest), nil
}

// ensureNamingScheme gates a push on the destination root's recorded naming
// scheme and bootstraps the marker on a fresh root.
func (h *contentPusher) ensureNamingScheme(ctx context.Context) error {
	root, err := h.checkNamingScheme(ctx)
	if err != nil || root != rootFresh {
		return err
	}
	return h.writeNamingMarker(ctx)
}

// checkNamingScheme classifies the root and refuses a push to one written
// under another scheme. Mixing the two would leave the pre-existing
// artifacts named as they already are while squirrel treated the root as
// private, so the refusal is the honest answer — and it is what keeps a
// root single-scheme, which is what lets the read paths resolve one.
//
// Read-only, so a dry run can ask the same question.
func (h *contentPusher) checkNamingScheme(ctx context.Context) (rootNaming, error) {
	root, err := probeRootNaming(ctx, h.rcl, h.dest)
	if err != nil || root != rootLegacy {
		return root, err
	}
	return root, fmt.Errorf("destination %q holds files at %s but no %s, so whatever is there was written under other names than the keyed ones this destination now derives — mixing the two would leave the existing artifacts named as they are while squirrel treated the root as private; restore and verify still read it as it stands, but to keep adding to it point the destination at a fresh root, or (after wiping the remote root) run `squirrel destination reset %s`: %w",
		h.dest.Name, remoteSubpathURI(h.dest, ""), NamingMarkerName, h.dest.Name, ErrRefused)
}

// validateNamingScheme reads the marker at uri and refuses any scheme this
// binary does not write. A marker that will not parse refuses too: it is
// the only record of how the root was named, so treating an unreadable one
// as absent would write a second naming generation into a populated root.
func validateNamingScheme(ctx context.Context, rcl *Rclone, dest *config.Destination, uri string) error {
	data, err := rcl.catRemote(ctx, uri, checkersArgs(dest)...)
	if err != nil {
		return fmt.Errorf("destination %q: read %s at %s: %w", dest.Name, NamingMarkerName, uri, err)
	}
	var m namingMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("destination %q: %s at %s is unreadable — inspect the root before syncing again: %w: %w", dest.Name, NamingMarkerName, uri, err, ErrRefused)
	}
	if m.Naming != namingSchemeKeyed {
		return fmt.Errorf("destination %q: %s at %s records naming scheme %q, this squirrel writes %q — point the destination at a fresh root, or (after wiping the remote root) run `squirrel destination reset %s`: %w",
			dest.Name, NamingMarkerName, uri, m.Naming, namingSchemeKeyed, dest.Name, ErrRefused)
	}
	return nil
}

// writeNamingMarker stamps the scheme on a fresh destination root, through
// the same overlay every artifact rides.
func (h *contentPusher) writeNamingMarker(ctx context.Context) error {
	body, err := json.Marshal(namingMarker{
		Naming:    namingSchemeKeyed,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("encode %s: %w", NamingMarkerName, err)
	}
	return h.uploadBytes(ctx, body, remoteSubpathURI(h.dest, NamingMarkerName), NamingMarkerName)
}

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

// namer names one destination's artifacts: by a keyed BLAKE3 hash when it
// holds a naming key, by content hash otherwise.
type namer struct {
	key *[32]byte
}

// namerFor is the naming dest's artifacts are written and read under.
func namerFor(dest *config.Destination) namer {
	if !dest.HidesArtifactNames() {
		return namer{}
	}
	return namer{key: &dest.Crypt.NamingKey}
}

// object is the basename of one content object.
func (n namer) object(contentHash []byte) string {
	if n.key == nil {
		return hex.EncodeToString(contentHash)
	}
	return n.keyedName(nameDomainObject, contentHash)
}

// pack is the basename of one pack.
func (n namer) pack(packKey []byte) string {
	if n.key == nil {
		return hex.EncodeToString(packKey)
	}
	return n.keyedName(nameDomainPack, packKey)
}

// volumeDir is the per-volume directory holding that volume's manifest
// segments, volume marker, and ride-along index snapshots.
func (n namer) volumeDir(volumeName string) string {
	if n.key == nil {
		return volumeName
	}
	return n.keyedName(nameDomainVolume, []byte(volumeName))
}

// keyedName is the lowercase hex keyed BLAKE3 of domain, a NUL separator,
// and input.
func (n namer) keyedName(domain string, input []byte) string {
	h, err := blake3.NewKeyed(n.key[:])
	if err != nil {
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

// ensureNamingScheme gates a push on the root's recorded naming scheme and,
// under init, stamps the scheme on a fresh root. A fresh root without init
// is left untouched for the volume-marker gate to refuse.
func (h *contentPusher) ensureNamingScheme(ctx context.Context, init bool) error {
	needsMarker, err := h.checkNamingScheme(ctx)
	if err != nil || !needsMarker || !init {
		return err
	}
	return h.writeNamingMarker(ctx)
}

// checkNamingScheme refuses a push to a root whose artifacts are named under
// any other scheme, and reports whether the root is fresh and still needs
// its naming marker. Read-only, so a dry run asks it too.
func (h *contentPusher) checkNamingScheme(ctx context.Context) (needsMarker bool, err error) {
	if !h.dest.HidesArtifactNames() {
		return false, nil
	}
	uri := remoteSubpathURI(h.dest, NamingMarkerName)
	present, err := h.rcl.statRemoteExists(ctx, uri, checkersArgs(h.dest)...)
	if err != nil {
		return false, fmt.Errorf("destination %q: stat %s at %s: %w", h.dest.Name, NamingMarkerName, uri, err)
	}
	if present {
		return false, validateNamingScheme(ctx, h.rcl, h.dest, uri)
	}
	if err := h.requireEmptyRoot(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// requireEmptyRoot refuses a root without a naming marker that holds any
// file at all: whatever is there was named under another scheme, and adding
// keyed names beside it would leave it disclosing what it always did.
func (h *contentPusher) requireEmptyRoot(ctx context.Context) error {
	rootURI := remoteSubpathURI(h.dest, "")
	empty, err := h.rcl.remoteRootEmpty(ctx, rootURI, nil, checkersArgs(h.dest)...)
	if err != nil {
		return fmt.Errorf("destination %q: list %s: %w", h.dest.Name, rootURI, err)
	}
	if !empty {
		return fmt.Errorf("destination %q holds files at %s but no %s, so they were written under other names than the keyed ones this destination derives — point the destination at a fresh root, or (after wiping the remote root) run `squirrel destination reset %s`: %w",
			h.dest.Name, rootURI, NamingMarkerName, h.dest.Name, ErrRefused)
	}
	return nil
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

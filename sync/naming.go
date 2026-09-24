package sync

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
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

// namingMarkerName is the reserved file at the root of a destination whose
// artifact names are keyed. It records the naming scheme the root's
// artifacts were written under, so a root written one way is never mixed
// with artifacts named another.
const namingMarkerName = ".squirrel-naming"

// namingSchemeKeyed is the scheme recorded for keyed BLAKE3 artifact names.
// It is compared verbatim, so a root recording any other scheme is refused.
const namingSchemeKeyed = "keyed-blake3-v1"

// namingMarker is the content of namingMarkerName, encrypted at rest by the
// crypt overlay. It records the scheme alone; the key is re-derived from
// the crypt passwords (config.DeriveNamingKey).
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

// rootMarkerNames are the basenames of the markers squirrel writes under
// dest's root, which a layout guard discounts when deciding whether the
// root is fresh.
func rootMarkerNames(dest *config.Destination) []string {
	if dest.HidesArtifactNames() {
		return []string{volmark.MarkerName, namingMarkerName}
	}
	return []string{volmark.MarkerName}
}

// namingStamp is what a push must write for the root's naming marker.
type namingStamp int

const (
	stampNone namingStamp = iota
	// stampFresh: the root is empty, so the marker is written under --init.
	stampFresh
	// stampRepair: the root holds this volume's keyed directory, so it was
	// written under this very key and only lost its marker.
	stampRepair
)

// ensureNamingScheme gates a push on the root's recorded naming scheme and
// writes the marker when the root needs one: under init on a fresh root,
// always on a root proven to be keyed under this destination's key. A
// fresh root without init is left untouched for the volume-marker gate to
// refuse.
func (h *rcloneArtifacts) ensureNamingScheme(ctx context.Context, init bool) error {
	stamp, err := h.checkNamingScheme(ctx)
	if err != nil {
		return err
	}
	if stamp == stampRepair || (stamp == stampFresh && init) {
		return h.writeNamingMarker(ctx)
	}
	return nil
}

// checkNamingScheme refuses a push to a root whose artifacts are named under
// any other scheme, and reports what the root still needs stamped.
// Read-only, so a dry run asks it too.
func (h *rcloneArtifacts) checkNamingScheme(ctx context.Context) (namingStamp, error) {
	if !h.dest.HidesArtifactNames() {
		return stampNone, nil
	}
	uri := remoteSubpathURI(h.dest, namingMarkerName)
	present, err := h.rcl.statRemoteExists(ctx, uri, concurrencyArgs(h.dest)...)
	if err != nil {
		return stampNone, fmt.Errorf("destination %q: stat %s at %s: %w", h.dest.Name, namingMarkerName, uri, err)
	}
	if present {
		return stampNone, validateNamingScheme(ctx, h.rcl, h.dest, uri)
	}
	return h.classifyUnmarkedRoot(ctx)
}

// classifyUnmarkedRoot decides a root without a naming marker. It lists
// the underlying remote, because the crypt overlay hides every file that
// was not written through it. A root holding anything but this volume's
// keyed directory was named some other way — or under other crypt
// passwords — and adding keyed names beside it would leave it disclosing
// what it always did.
func (h *rcloneArtifacts) classifyUnmarkedRoot(ctx context.Context) (namingStamp, error) {
	rootURI := underlyingDirURI(h.dest, "")
	empty, err := h.rcl.remoteRootEmpty(ctx, rootURI, nil, concurrencyArgs(h.dest)...)
	if err != nil {
		return stampNone, fmt.Errorf("destination %q: list %s: %w", h.dest.Name, rootURI, err)
	}
	if empty {
		return stampFresh, nil
	}
	keyed, err := h.holdsKeyedVolumeMarker(ctx)
	if err != nil {
		return stampNone, err
	}
	if keyed {
		return stampRepair, nil
	}
	return stampNone, fmt.Errorf("destination %q holds files at %s but no %s and no keyed directory for volume %q, so they were written under other names than the keyed ones this destination derives, or under other crypt passwords. If another volume already syncs here, sync it first: that restores the marker. Otherwise point the destination at a fresh root, or wipe the remote root and run `squirrel destination reset %s`: %w",
		h.dest.Name, rootURI, namingMarkerName, h.vol.Name, h.dest.Name, ErrRefused)
}

func (h *rcloneArtifacts) holdsKeyedVolumeMarker(ctx context.Context) (bool, error) {
	uri := remoteSubpathURI(h.dest, path.Join(h.names().volumeDir(h.vol.Name), volmark.MarkerName))
	present, err := h.rcl.statRemoteExists(ctx, uri, concurrencyArgs(h.dest)...)
	if err != nil {
		return false, fmt.Errorf("destination %q: stat %s at %s: %w", h.dest.Name, volmark.MarkerName, uri, err)
	}
	return present, nil
}

// validateNamingScheme refuses the marker at uri when it will not parse or
// records a scheme this binary does not write.
func validateNamingScheme(ctx context.Context, rcl *Rclone, dest *config.Destination, uri string) error {
	data, err := rcl.catRemote(ctx, uri, concurrencyArgs(dest)...)
	if err != nil {
		return fmt.Errorf("destination %q: read %s at %s — a root written under other crypt passwords cannot be read with these: %w", dest.Name, namingMarkerName, uri, err)
	}
	var m namingMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("destination %q: %s at %s is unreadable — inspect the root before syncing again: %w: %w", dest.Name, namingMarkerName, uri, err, ErrRefused)
	}
	if m.Naming != namingSchemeKeyed {
		return fmt.Errorf("destination %q: %s at %s records naming scheme %q, this squirrel writes %q — point the destination at a fresh root, or (after wiping the remote root) run `squirrel destination reset %s`: %w",
			dest.Name, namingMarkerName, uri, m.Naming, namingSchemeKeyed, dest.Name, ErrRefused)
	}
	return nil
}

// writeNamingMarker stamps the scheme on a fresh destination root.
func (h *rcloneArtifacts) writeNamingMarker(ctx context.Context) error {
	body, err := json.Marshal(namingMarker{
		Naming:    namingSchemeKeyed,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("encode %s: %w", namingMarkerName, err)
	}
	return putBytes(ctx, h, 0, namingMarkerName, body, namingMarkerName)
}

package sync

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/config"
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

// objectName is the basename of one content object at dest: a keyed name
// where the destination hides artifact names, the content's own BLAKE3 hex
// otherwise.
func objectName(dest *config.Destination, contentHash []byte) string {
	if !dest.HidesArtifactNames() {
		return hex.EncodeToString(contentHash)
	}
	return keyedName(dest, nameDomainObject, contentHash)
}

// packName is the basename of one pack at dest, keyed the same way as
// objectName. A pack key already discloses no single file, but naming it
// keyed keeps one rule for the whole root.
func packName(dest *config.Destination, packKey []byte) string {
	if !dest.HidesArtifactNames() {
		return hex.EncodeToString(packKey)
	}
	return keyedName(dest, nameDomainPack, packKey)
}

// volumeDirName is the per-volume directory at dest holding that volume's
// manifest segments and ride-along index snapshots. Keying it is what stops
// the remote from disclosing the volume names themselves.
func volumeDirName(dest *config.Destination, volumeName string) string {
	if !dest.HidesArtifactNames() {
		return volumeName
	}
	return keyedName(dest, nameDomainVolume, []byte(volumeName))
}

// keyedName derives one artifact name: the keyed BLAKE3 of domain, a NUL
// separator, and input, under the destination's naming key, as lowercase
// hex. Deterministic, so identical content still derives one name and
// uploads once; unforgeable without the key, so the name discloses nothing
// about what it stands for.
func keyedName(dest *config.Destination, domain string, input []byte) string {
	h, err := blake3.NewKeyed(dest.Crypt.NamingKey[:])
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

// ensureNamingScheme gates a push on the destination root's recorded naming
// scheme and bootstraps the marker on a fresh root.
func (h *contentPusher) ensureNamingScheme(ctx context.Context) error {
	bootstrap, err := h.checkNamingScheme(ctx)
	if err != nil || !bootstrap {
		return err
	}
	return h.writeNamingMarker(ctx)
}

// checkNamingScheme reads the destination root's naming marker and reports
// whether a fresh root still needs one written. It refuses a root that
// holds artifacts under a different scheme — including the content-hash
// names every encrypted destination wrote before keyed naming existed,
// which leave no marker at all. Mixing the two would leave the pre-existing
// artifacts disclosing their content hashes while squirrel believed the
// root was private, so the refusal is the honest answer.
//
// Read-only, so a dry run can ask the same question.
func (h *contentPusher) checkNamingScheme(ctx context.Context) (bootstrap bool, err error) {
	if !h.dest.HidesArtifactNames() {
		return false, nil
	}
	uri := remoteSubpathURI(h.dest, NamingMarkerName)
	present, err := h.rcl.statRemoteExists(ctx, uri, checkersArgs(h.dest)...)
	if err != nil {
		return false, fmt.Errorf("destination %q: stat %s at %s: %w", h.dest.Name, NamingMarkerName, uri, err)
	}
	if present {
		return false, h.validateNamingScheme(ctx, uri)
	}
	if !freshStartOnEmptyRoot(ctx, h.rcl, h.dest) {
		return false, fmt.Errorf("destination %q holds files at %s but no %s, so whatever is there was written under other names than the keyed ones this destination now derives — mixing the two would leave the existing artifacts named as they are while squirrel treated the root as private; point the destination at a fresh root, or (after wiping the remote root) run `squirrel destination reset %s`: %w",
			h.dest.Name, remoteSubpathURI(h.dest, ""), NamingMarkerName, h.dest.Name, ErrRefused)
	}
	return true, nil
}

// validateNamingScheme reads the marker at uri and refuses any scheme this
// binary does not write. A marker that will not parse refuses too: it is
// the only record of how the root was named, so treating an unreadable one
// as absent would write a second naming generation into a populated root.
func (h *contentPusher) validateNamingScheme(ctx context.Context, uri string) error {
	data, err := h.rcl.catRemote(ctx, uri, checkersArgs(h.dest)...)
	if err != nil {
		return fmt.Errorf("destination %q: read %s at %s: %w", h.dest.Name, NamingMarkerName, uri, err)
	}
	var m namingMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("destination %q: %s at %s is unreadable — inspect the root before syncing again: %w: %w", h.dest.Name, NamingMarkerName, uri, err, ErrRefused)
	}
	if m.Naming != namingSchemeKeyed {
		return fmt.Errorf("destination %q: %s at %s records naming scheme %q, this squirrel writes %q — point the destination at a fresh root, or (after wiping the remote root) run `squirrel destination reset %s`: %w",
			h.dest.Name, NamingMarkerName, uri, m.Naming, namingSchemeKeyed, h.dest.Name, ErrRefused)
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

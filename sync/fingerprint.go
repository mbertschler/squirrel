package sync

import (
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/mbertschler/squirrel/config"
)

// Checksum algo labels recorded in remote_objects.checksum_algo for the
// s3 backend, whose provider checksum is the object ETag (surfaced by
// rclone as the md5 hash). The value is recorded opaquely and compared
// verbatim on verification — squirrel never recomputes a provider
// checksum, which is what makes the multipart composite form (md5 of
// part md5s, "<hex>-<parts>") usable as a fingerprint at all. Every
// other backend records the plain rclone hash name (sha256, sha1, …).
const (
	AlgoEtagMD5          = "etag-md5"
	AlgoEtagMD5Composite = "etag-md5-composite"
)

// remoteChecksum is one provider checksum read back from a destination's
// underlying remote: the algo label recorded in remote_objects plus the
// provider's canonical value.
type remoteChecksum struct {
	Algo  string
	Value string
}

// hashPreference orders the rclone hash names picked when a backend
// exposes several and the destination configures no hash_algo: strongest
// first, with anything unlisted falling back to name order.
var hashPreference = []string{"sha256", "sha1", "md5", "crc32"}

// extractChecksum maps one object's lsjson hashes onto the fingerprint
// recorded for dest's backend type: the ETag under an etag flavor for
// s3, the configured hash_algo where one is set, and the strongest
// exposed hash otherwise. ok is false when the listing exposes no usable
// checksum for the object.
func extractChecksum(dest *config.Destination, hashes map[string]string) (remoteChecksum, bool) {
	switch {
	case dest.Type == "s3":
		v := hashes["md5"]
		if v == "" {
			return remoteChecksum{}, false
		}
		return remoteChecksum{Algo: etagFlavor(v), Value: v}, true
	case dest.HashAlgo != "":
		v := hashes[dest.HashAlgo]
		if v == "" {
			return remoteChecksum{}, false
		}
		return remoteChecksum{Algo: dest.HashAlgo, Value: v}, true
	default:
		for _, name := range hashPreference {
			if v := hashes[name]; v != "" {
				return remoteChecksum{Algo: name, Value: v}, true
			}
		}
		for _, name := range slices.Sorted(maps.Keys(hashes)) {
			if v := hashes[name]; v != "" {
				return remoteChecksum{Algo: name, Value: v}, true
			}
		}
		return remoteChecksum{}, false
	}
}

// etagFlavor labels an s3 ETag value: the multipart composite form
// carries a "-<parts>" suffix, a plain upload's ETag is the object md5.
func etagFlavor(v string) string {
	if strings.Contains(v, "-") {
		return AlgoEtagMD5Composite
	}
	return AlgoEtagMD5
}

// captureHashTypes returns the --hash-type set fingerprint capture
// requests from dest's underlying remote; nil means "whatever the
// backend exposes". Narrowing matters on backends that compute hashes
// per request (sftp runs one server-side sum command per file per hash
// type).
func captureHashTypes(dest *config.Destination) []string {
	switch {
	case dest.Type == "s3":
		return []string{"md5"}
	case dest.HashAlgo != "":
		return []string{dest.HashAlgo}
	}
	return nil
}

// algoHashType maps a recorded checksum_algo back to the rclone hash
// name re-verification must request: the etag flavors ride on the md5
// hash, every other algo is the hash name itself.
func algoHashType(algo string) string {
	if algo == AlgoEtagMD5 || algo == AlgoEtagMD5Composite {
		return "md5"
	}
	return algo
}

// checkersArgs renders dest's optional concurrent-checkers cap as rclone
// argv.
func checkersArgs(dest *config.Destination) []string {
	if dest.Checkers <= 0 {
		return nil
	}
	return []string{"--checkers", strconv.Itoa(dest.Checkers)}
}

// underlyingObjectsURI addresses the destination-root objects/ directory
// on the underlying remote, bypassing any crypt overlay: the scan-back
// fingerprint is over the stored ciphertext, and with filename
// encryption fixed off the underlying object key equals the overlay
// path.
func underlyingObjectsURI(dest *config.Destination) string {
	return dest.Name + ":" + path.Join(dest.Root, ObjectsDirName)
}

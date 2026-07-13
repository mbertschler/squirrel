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
// s3 backend, whose provider checksum is the object ETag. The ETag is read
// straight from the S3 API (a ListObjectsV2 over the objects/ prefix, see
// s3reader.go), not through rclone — rclone funnels every hash read through
// Object.Hash(MD5), which returns "" for a multipart object's composite
// ETag. A single-part upload's whole-object MD5 is recorded as etag-md5; a
// multipart <hex>-<parts> value as etag-md5-composite. Both are recorded
// and compared verbatim — squirrel never recomputes a provider checksum, so
// the read is correct regardless of the object's SSE mode. Every other
// backend records the plain rclone hash name (sha256, sha1, …).
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

// etagFlavor labels an s3 ETag value by shape: a "-<parts>" suffix marks
// the multipart composite form, otherwise the value is a whole-object md5.
// The label is descriptive only — both are stored and compared verbatim.
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
// type). s3 never reaches here — its ETag is read straight from the S3
// API, not through an rclone hash request.
func captureHashTypes(dest *config.Destination) []string {
	if dest.HashAlgo != "" {
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

// underlyingDirURI addresses a destination-root directory (objects/ or
// packs/) on the underlying remote, bypassing any crypt overlay: the
// scan-back fingerprint is over the stored ciphertext, and with filename
// encryption fixed off the underlying key equals the overlay path.
func underlyingDirURI(dest *config.Destination, dirName string) string {
	return dest.Name + ":" + path.Join(dest.Root, dirName)
}

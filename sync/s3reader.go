package sync

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/mbertschler/squirrel/config"
)

// s3ETagReader reads the raw S3 ETags of the files under a destination's
// objects/ or packs/ prefix, straight from the S3 API. This is the only
// read surface that surfaces a multipart object's composite ETag
// (<hex>-<parts>): rclone funnels every hash read through Object.Hash(MD5),
// which returns "" for a composite ETag, so `rclone lsjson --hash` can
// never see it. Every pack exceeds the multipart threshold, so pack
// fingerprint capture and verification always read ETags here rather than
// through rclone; large content objects do the same.
//
// The value is returned verbatim: a composite <hex>-<parts> stays
// composite. Callers compare it byte-for-byte and never recompute it, so
// the read is correct regardless of the object's SSE mode or storage class.
type s3ETagReader interface {
	// objectETags lists every file under the reader's configured prefix (a
	// paginated ListObjectsV2, archive-tier-safe with no per-object HEAD)
	// and returns its raw ETag keyed by the file's basename — the lowercase
	// BLAKE3 hex (of the content, or of the pack's compressed bytes), since
	// filename encryption is off and the underlying key equals the overlay
	// path.
	objectETags(ctx context.Context) (map[string]string, error)
}

// newS3ETagReader builds a reader over dest's dirName prefix (ObjectsDirName
// or PacksDirName). It is a package var so tests can substitute a fake
// without a live bucket; production always uses the minio-go implementation.
var newS3ETagReader = func(dest *config.Destination, dirName string) (s3ETagReader, error) {
	return newMinioETagReader(dest, dirName)
}

// minioETagReader is the production s3ETagReader, a thin wrapper over a
// minio-go client scoped to one destination's bucket and objects/ prefix.
type minioETagReader struct {
	client *minio.Client
	bucket string
	prefix string
}

func newMinioETagReader(dest *config.Destination, dirName string) (*minioETagReader, error) {
	p := dest.Params
	bucket := p["bucket"]
	if bucket == "" {
		return nil, fmt.Errorf("s3 destination %q: bucket is required to read ETags", dest.Name)
	}
	endpoint, secure, err := s3Endpoint(p["endpoint"], p["region"])
	if err != nil {
		return nil, fmt.Errorf("s3 destination %q: %w", dest.Name, err)
	}
	// BucketLookupAuto handles AWS (virtual-host) and IP/localhost endpoints
	// (path-style) on its own; force_path_style overrides it for
	// S3-compatible providers Auto guesses wrong.
	lookup := minio.BucketLookupAuto
	if dest.PathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:        s3Credentials(p["access_key_id"], p["secret_access_key"]),
		Secure:       secure,
		Region:       p["region"],
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 destination %q: build client: %w", dest.Name, err)
	}
	return &minioETagReader{
		client: client,
		bucket: bucket,
		// Object keys are bucket-relative and never begin with "/": the
		// leading slash of an absolute-style root must go, or the listing
		// prefix matches nothing that rclone actually wrote.
		prefix: path.Join(strings.TrimPrefix(dest.Root, "/"), dirName) + "/",
	}, nil
}

func (r *minioETagReader) objectETags(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	// minio-go's ListObjects issues ListObjectsV2 requests by default and
	// paginates internally, so this ranges over every object under the
	// prefix without a manual continuation-token loop.
	for obj := range r.client.ListObjects(ctx, r.bucket, minio.ListObjectsOptions{Prefix: r.prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", r.bucket, r.prefix, obj.Err)
		}
		// Record every listed key, even the (in practice never seen) empty
		// ETag: the object *is* present, so the caller must classify it as
		// "present, no usable checksum" rather than "not listed" — dropping
		// it here would misreport a live object as missing. minio-go strips
		// the surrounding quotes from the raw ETag; a plain 32-hex value is a
		// whole-object MD5, a <hex>-<parts> value the multipart composite
		// form — both recorded verbatim by the caller.
		out[path.Base(obj.Key)] = obj.ETag
	}
	return out, nil
}

// s3Credentials picks the credential source for the ETag reader: explicit
// static keys when both are configured, otherwise minio-go's chain (AWS
// env vars, shared credentials file, then IAM role) — the same ambient
// sources rclone falls back to when a destination sets no keys, so
// scan-back fingerprinting works on every S3 destination rclone can reach.
func s3Credentials(accessKeyID, secretAccessKey string) *credentials.Credentials {
	if accessKeyID != "" && secretAccessKey != "" {
		return credentials.NewStaticV4(accessKeyID, secretAccessKey, "")
	}
	return credentials.NewChainCredentials([]credentials.Provider{
		&credentials.EnvAWS{},
		&credentials.FileAWSCredentials{},
		&credentials.IAM{Client: &http.Client{Transport: http.DefaultTransport}},
	})
}

// s3Endpoint resolves the minio host[:port] and TLS flag from the
// configured endpoint and region. An empty endpoint targets AWS S3 (a
// region-specific host when a region is set, the global host otherwise). A
// configured endpoint may carry an http/https scheme, which sets the TLS
// flag; a bare host defaults to TLS on.
func s3Endpoint(endpoint, region string) (host string, secure bool, err error) {
	if endpoint == "" {
		if region != "" {
			return "s3." + region + ".amazonaws.com", true, nil
		}
		return "s3.amazonaws.com", true, nil
	}
	if !strings.Contains(endpoint, "://") {
		return endpoint, true, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", false, fmt.Errorf("parse endpoint %q: %w", endpoint, err)
	}
	if u.Host == "" {
		return "", false, fmt.Errorf("endpoint %q has no host", endpoint)
	}
	return u.Host, u.Scheme != "http", nil
}

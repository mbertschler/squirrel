package sync

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/mbertschler/squirrel/config"
)

// s3ETagReader reads the raw S3 ETags of the content objects under a
// destination's objects/ prefix, straight from the S3 API. This is the
// only read surface that surfaces a multipart object's composite ETag
// (<hex>-<parts>): rclone funnels every hash read through Object.Hash(MD5),
// which returns "" for a composite ETag, so `rclone lsjson --hash` can
// never see it. s3 fingerprint capture and verification therefore read
// ETags here rather than through rclone.
//
// The value is returned verbatim: a composite <hex>-<parts> stays
// composite. Callers compare it byte-for-byte and never recompute it, so
// the read is correct regardless of the object's SSE mode or storage class.
type s3ETagReader interface {
	// objectETags lists every object under the destination's objects/
	// prefix (a paginated ListObjectsV2, archive-tier-safe with no per-object
	// HEAD) and returns its raw ETag keyed by the object's basename — the
	// lowercase BLAKE3 hex, since filename encryption is off and the
	// underlying key equals the overlay path.
	objectETags(ctx context.Context) (map[string]string, error)
}

// newS3ETagReader builds the reader for dest. It is a package var so tests
// can substitute a fake without a live bucket; production always uses the
// minio-go implementation.
var newS3ETagReader = func(dest *config.Destination) (s3ETagReader, error) {
	return newMinioETagReader(dest)
}

// minioETagReader is the production s3ETagReader, a thin wrapper over a
// minio-go client scoped to one destination's bucket and objects/ prefix.
type minioETagReader struct {
	client *minio.Client
	bucket string
	prefix string
}

func newMinioETagReader(dest *config.Destination) (*minioETagReader, error) {
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
		Creds:        credentials.NewStaticV4(p["access_key_id"], p["secret_access_key"], ""),
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
		prefix: path.Join(dest.Root, ObjectsDirName) + "/",
	}, nil
}

func (r *minioETagReader) objectETags(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	for obj := range r.client.ListObjects(ctx, r.bucket, minio.ListObjectsOptions{Prefix: r.prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", r.bucket, r.prefix, obj.Err)
		}
		// minio-go strips the surrounding quotes from the raw ETag; a plain
		// 32-hex value is a whole-object MD5, a <hex>-<parts> value the
		// multipart composite form — both recorded verbatim by the caller.
		if obj.ETag != "" {
			out[path.Base(obj.Key)] = obj.ETag
		}
	}
	return out, nil
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

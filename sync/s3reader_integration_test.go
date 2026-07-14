//go:build integration

package sync

import (
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"

	"github.com/mbertschler/squirrel/config"
)

// This file holds integration tests that talk to a real S3-compatible
// endpoint (SeaweedFS in the reference fixture under test/integration/).
// They are excluded from the default build by the `integration` tag and
// additionally skip when SQUIRREL_TEST_S3_ENDPOINT is unset, so
// `go test ./...` stays hermetic. See test/integration/README.md to run.

// compositeETag matches the multipart ETag form <hex>-<parts>: the MD5 of
// the concatenated part MD5s, a dash, and the part count. A single-part
// (whole-object) ETag is a bare 32-char MD5 with no dash.
var compositeETag = regexp.MustCompile(`^[0-9a-f]{32}-[0-9]+$`)

// s3TestEnv is the connection info for the integration endpoint, read from
// the environment. Missing endpoint skips; missing credentials are allowed
// (an unauthenticated gateway), matching s3Credentials' ambient fallback.
type s3TestEnv struct {
	endpoint, accessKey, secretKey, bucket, region string
}

func s3TestEnvOrSkip(t *testing.T) s3TestEnv {
	t.Helper()
	endpoint := os.Getenv("SQUIRREL_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("SQUIRREL_TEST_S3_ENDPOINT not set; skipping S3 integration test")
	}
	bucket := os.Getenv("SQUIRREL_TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "squirrel-integration"
	}
	return s3TestEnv{
		endpoint:  endpoint,
		accessKey: os.Getenv("SQUIRREL_TEST_S3_ACCESS_KEY"),
		secretKey: os.Getenv("SQUIRREL_TEST_S3_SECRET_KEY"),
		bucket:    bucket,
		region:    os.Getenv("SQUIRREL_TEST_S3_REGION"),
	}
}

// destination builds the config.Destination the S3 ETag reader consumes.
// PathStyle is forced on: the fixture endpoint is a bare host:port that
// virtual-host addressing cannot reach.
func (e s3TestEnv) destination(root string) *config.Destination {
	return &config.Destination{
		Name:      "seaweedfs-integration",
		Type:      "s3",
		Root:      root,
		PathStyle: true,
		Params: map[string]string{
			"bucket":            e.bucket,
			"endpoint":          e.endpoint,
			"region":            e.region,
			"access_key_id":     e.accessKey,
			"secret_access_key": e.secretKey,
		},
	}
}

// uploadClient builds a minio client for the test's own uploads, reusing the
// package's endpoint/credential resolution so it addresses the bucket the
// same way the reader under test does.
func (e s3TestEnv) uploadClient(t *testing.T) *minio.Client {
	t.Helper()
	host, secure, err := s3Endpoint(e.endpoint, e.region)
	if err != nil {
		t.Fatalf("resolve endpoint %q: %v", e.endpoint, err)
	}
	client, err := minio.New(host, &minio.Options{
		Creds:        s3Credentials(e.accessKey, e.secretKey),
		Secure:       secure,
		Region:       e.region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatalf("build upload client: %v", err)
	}
	return client
}

func ensureBucket(t *testing.T, client *minio.Client, bucket string) {
	t.Helper()
	ctx := context.Background()
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		t.Fatalf("stat bucket %q: %v", bucket, err)
	}
	if exists {
		return
	}
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("create bucket %q: %v", bucket, err)
	}
}

// putObject uploads size bytes of deterministic data under root/objects/<name>
// (the layout objectETags reads), forcing multipart when partSize splits the
// body into more than one part. It returns the data so callers can verify a
// byte-exact round trip. The object is removed on cleanup.
func putObject(t *testing.T, client *minio.Client, e s3TestEnv, root, name string, size, partSize int64) []byte {
	t.Helper()
	ctx := context.Background()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 31)
	}
	key := root + "/" + ObjectsDirName + "/" + name
	_, err := client.PutObject(ctx, e.bucket, key, bytes.NewReader(data), size,
		minio.PutObjectOptions{PartSize: uint64(partSize), ContentType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("put %q: %v", key, err)
	}
	t.Cleanup(func() {
		_ = client.RemoveObject(context.Background(), e.bucket, key, minio.RemoveObjectOptions{})
	})
	return data
}

// TestS3MultipartCompositeETag is the load-bearing faithfulness check for the
// whole S3 integration story: it uploads a multipart object to the real
// endpoint and asserts the ETag read back through the production s3ETagReader
// is the composite <hex>-<parts> form. If a server hands back a plain
// whole-object MD5 for a multipart upload instead, s3reader.go's entire
// reason to exist is void on that server, and pack fingerprinting silently
// compares mismatched hashes — so this test gates trusting the server at all.
func TestS3MultipartCompositeETag(t *testing.T) {
	env := s3TestEnvOrSkip(t)
	client := env.uploadClient(t)
	ensureBucket(t, client, env.bucket)

	const partSize = 5 << 20 // 5 MiB: S3's minimum part size
	const size = 11 << 20    // 11 MiB => 3 parts (5 + 5 + 1)
	root := "integration/multipart"
	name := blake3Hex("squirrel-multipart-etag-fixture")
	data := putObject(t, client, env, root, name, size, partSize)

	reader, err := newS3ETagReader(env.destination(root), ObjectsDirName)
	if err != nil {
		t.Fatalf("newS3ETagReader: %v", err)
	}
	etags, err := reader.objectETags(context.Background())
	if err != nil {
		t.Fatalf("objectETags: %v", err)
	}

	got, ok := etags[name]
	if !ok {
		t.Fatalf("object %q not listed; got keys %v", name, keysOf(etags))
	}
	if !compositeETag.MatchString(got) {
		t.Fatalf("multipart ETag %q is not composite <hex>-<parts>; server does not "+
			"expose composite ETags and cannot back the S3 fingerprint path", got)
	}
	if parts := partCount(t, got); parts < 2 {
		t.Fatalf("composite ETag %q reports %d parts, want >1 for a multipart upload", got, parts)
	}
	t.Logf("multipart composite ETag read back: %s", got)

	// A multipart round trip must also return the exact bytes: content
	// integrity is the point of the whole tool.
	assertRoundTrip(t, client, env, root, name, data)
}

// TestS3SinglePartPlainETag pins the contrast the reader relies on: an object
// small enough for a single PUT comes back as a bare 32-hex MD5, never
// composite. Together with the multipart case this proves the endpoint
// distinguishes the two forms the way real S3 does.
func TestS3SinglePartPlainETag(t *testing.T) {
	env := s3TestEnvOrSkip(t)
	client := env.uploadClient(t)
	ensureBucket(t, client, env.bucket)

	const size = 1 << 20 // 1 MiB: well under the multipart threshold
	root := "integration/singlepart"
	name := blake3Hex("squirrel-singlepart-etag-fixture")
	putObject(t, client, env, root, name, size, 5<<20)

	reader, err := newS3ETagReader(env.destination(root), ObjectsDirName)
	if err != nil {
		t.Fatalf("newS3ETagReader: %v", err)
	}
	etags, err := reader.objectETags(context.Background())
	if err != nil {
		t.Fatalf("objectETags: %v", err)
	}

	got, ok := etags[name]
	if !ok {
		t.Fatalf("object %q not listed; got keys %v", name, keysOf(etags))
	}
	if strings.Contains(got, "-") {
		t.Fatalf("single-part ETag %q is composite; want a bare whole-object MD5", got)
	}
	if len(got) != 32 {
		t.Fatalf("single-part ETag %q is %d chars, want a 32-hex MD5", got, len(got))
	}
	t.Logf("single-part plain ETag read back: %s", got)
}

func assertRoundTrip(t *testing.T, client *minio.Client, e s3TestEnv, root, name string, want []byte) {
	t.Helper()
	key := root + "/" + ObjectsDirName + "/" + name
	obj, err := client.GetObject(context.Background(), e.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	defer func() { _ = obj.Close() }()
	got, err := io.ReadAll(obj)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round trip of %q returned %d bytes, want %d (content mismatch)", key, len(got), len(want))
	}
}

func partCount(t *testing.T, etag string) int {
	t.Helper()
	_, n, ok := strings.Cut(etag, "-")
	if !ok {
		t.Fatalf("etag %q has no part suffix", etag)
	}
	parts, err := strconv.Atoi(n)
	if err != nil {
		t.Fatalf("etag %q part count %q not an integer: %v", etag, n, err)
	}
	return parts
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

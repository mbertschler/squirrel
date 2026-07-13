package sync

import (
	"context"
	"testing"

	"github.com/mbertschler/squirrel/config"
)

// fakeS3Reader is the test stand-in for the S3 API: the fake-rclone shim
// cannot model ListObjectsV2, so s3 ETag reads are mocked at the
// s3ETagReader interface. etags maps a blake3 hex to the raw ETag the
// listing would return; err simulates a transient read failure.
type fakeS3Reader struct {
	etags map[string]string
	err   error
}

func (f fakeS3Reader) objectETags(context.Context) (map[string]string, error) {
	return f.etags, f.err
}

// installS3Reader points the package's S3-reader constructor at r for the
// duration of the test, restoring the real minio-go builder afterward.
func installS3Reader(t *testing.T, r s3ETagReader) {
	t.Helper()
	prev := newS3ETagReader
	newS3ETagReader = func(*config.Destination) (s3ETagReader, error) { return r, nil }
	t.Cleanup(func() { newS3ETagReader = prev })
}

func TestS3Endpoint(t *testing.T) {
	cases := []struct {
		name, endpoint, region string
		wantHost               string
		wantSecure             bool
	}{
		{"aws default", "", "", "s3.amazonaws.com", true},
		{"aws regional", "", "eu-central-1", "s3.eu-central-1.amazonaws.com", true},
		{"https endpoint", "https://minio.local:9000", "", "minio.local:9000", true},
		{"http endpoint", "http://10.0.0.1:9000", "", "10.0.0.1:9000", false},
		{"bare host defaults secure", "minio.local:9000", "", "minio.local:9000", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, secure, err := s3Endpoint(c.endpoint, c.region)
			if err != nil {
				t.Fatalf("s3Endpoint: %v", err)
			}
			if host != c.wantHost || secure != c.wantSecure {
				t.Fatalf("s3Endpoint = (%q, %t), want (%q, %t)", host, secure, c.wantHost, c.wantSecure)
			}
		})
	}
}

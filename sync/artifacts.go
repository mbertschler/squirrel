package sync

import (
	"context"
	"fmt"
	"os"

	"github.com/zeebo/blake3"
)

// artifactStore is where a content-addressed or packed push lands its
// artifacts: an rclone remote, or a native destination root reached
// through squirrel's own transport. Names are slash-separated and relative
// to the destination root, as namer and the layout constants build them.
type artifactStore interface {
	// markers gates the push on the destination's markers, writing missing
	// ones under opts.Init; a dry run only checks.
	markers(ctx context.Context, opts Options) error
	// exists reports whether the artifact at name is present.
	exists(ctx context.Context, name string) (bool, error)
	// rootEmpty reports whether the root holds nothing beyond squirrel's
	// markers.
	rootEmpty(ctx context.Context) (bool, error)
	// reconcile clears what finished pushes left in flight, once at push
	// start.
	reconcile(ctx context.Context, rep *Report, runID int64) error
	// put lands the local file src at name whole, and confirms it landed:
	// its size, and bytes whose BLAKE3 is sum — errContentDrift when src
	// no longer hashes to sum. It returns the fingerprint the landing
	// already confirmed, or nil when capture must read one later.
	put(ctx context.Context, runID int64, name, src string, size int64, sum []byte) (*remoteChecksum, error)
	// capture fills the fingerprints put left pending for this run's
	// artifacts under dir (ObjectsDirName or PacksDirName).
	capture(ctx context.Context, rep *Report, dir string, targets []captureTarget)
	// shelf is the volume's .squirrel-index/ directory, where runID's
	// ride-along index snapshot lands.
	shelf(runID int64) snapshotShelf
	// where names an artifact for a message.
	where(name string) string
	// close releases the destination once the push returns.
	close()
}

// putBytes lands body at name through art, staged in a temporary file.
// what names the artifact in error messages.
func putBytes(ctx context.Context, art artifactStore, runID int64, name string, body []byte, what string) error {
	tmp, err := os.CreateTemp("", "squirrel-meta-*")
	if err != nil {
		return fmt.Errorf("stage %s: %w", what, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", what, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", what, err)
	}
	sum := blake3.Sum256(body)
	if _, err := art.put(ctx, runID, name, tmp.Name(), int64(len(body)), sum[:]); err != nil {
		return fmt.Errorf("upload %s to %s: %w", what, art.where(name), err)
	}
	return nil
}

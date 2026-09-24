package sync

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// rcloneArtifacts is an artifact store rclone reaches, through the crypt
// overlay when the destination has one.
type rcloneArtifacts struct {
	store *store.Store
	rcl   *Rclone
	vol   *config.Volume
	dest  *config.Destination
}

// names is how the destination names its artifacts.
func (a *rcloneArtifacts) names() namer { return namerFor(a.dest) }

// markers gates the push on the root's naming scheme, then on the
// destination's per-volume .squirrel-volume marker, exactly as the mirror
// layout does (the marker sits at the volume root regardless of layout). A
// dry run checks the naming scheme read-only and writes nothing.
func (a *rcloneArtifacts) markers(ctx context.Context, opts Options) error {
	if opts.DryRun {
		_, err := a.checkNamingScheme(ctx)
		return err
	}
	if err := a.ensureNamingScheme(ctx, opts.Init); err != nil {
		return err
	}
	return ensureRemoteDestinationMarker(ctx, a.store, a.rcl, a.dest, a.vol.Name, opts.Init)
}

func (a *rcloneArtifacts) exists(ctx context.Context, name string) (bool, error) {
	return a.rcl.statRemoteExists(ctx, a.where(name), checkersArgs(a.dest)...)
}

// rootEmpty reports whether the destination root holds no files beyond
// squirrel's markers — a wiped or repointed destination, or one whose
// recorded state was cleared by `squirrel destination reset`.
func (a *rcloneArtifacts) rootEmpty(ctx context.Context) (bool, error) {
	return a.rcl.remoteRootEmpty(ctx, remoteSubpathURI(a.dest, ""), rootMarkerNames(a.dest), checkersArgs(a.dest)...)
}

// reconcile returns at once: rclone lands every artifact under its own
// name, and squirrel records it only after it landed.
func (a *rcloneArtifacts) reconcile(context.Context, *Report, int64) error { return nil }

// put re-hashes src immediately before the transfer and refuses
// (errContentDrift) when the digest no longer matches sum, catching a
// size+mtime-preserving in-place edit that a metadata stat would pass. The
// post-transfer stat confirms presence and size; the fingerprint is read
// back afterwards (capture).
//
// Residual: rclone reads the file in a separate child process after the
// re-hash, so a writer that edits the file in the window between the hash
// and rclone's read could still upload drifted bytes. The window is the
// fork/exec of one rclone invocation rather than the whole walk-to-push
// span, and the scan-back fingerprint pass (#109) re-reads the landed
// object to upgrade the durability vector, catching any byte that slipped
// through before the object is treated as content-verified.
func (a *rcloneArtifacts) put(ctx context.Context, _ int64, name, src string, size int64, sum []byte) (*remoteChecksum, error) {
	digest, err := hashLocalFile(src)
	if err != nil {
		return nil, fmt.Errorf("re-hash %s before upload: %w", src, err)
	}
	if !bytes.Equal(digest, sum) {
		return nil, fmt.Errorf("%w: %s now hashes to %s, indexed as %s", errContentDrift, src, hex.EncodeToString(digest), hex.EncodeToString(sum))
	}
	uri := a.where(name)
	if err := a.rcl.copyTo(ctx, src, uri, checkersArgs(a.dest)...); err != nil {
		return nil, err
	}
	landed, err := a.rcl.statRemote(ctx, uri, checkersArgs(a.dest)...)
	if err != nil {
		return nil, fmt.Errorf("confirm %s after upload: %w", uri, err)
	}
	if landed != size {
		return nil, fmt.Errorf("%s landed with size %d, want %d", uri, landed, size)
	}
	return nil, nil
}

func (a *rcloneArtifacts) capture(ctx context.Context, rep *Report, dir string, targets []captureTarget) {
	a.captureScanBackFingerprints(ctx, rep, dir, targets)
}

// shelf is the destination's .squirrel-index/ directory, reached through
// rclone like the rest of the push.
func (a *rcloneArtifacts) shelf(int64) snapshotShelf {
	return rcloneShelf{rcl: a.rcl, dir: indexDirURI(a.dest, a.vol.Name)}
}

// where is the artifact's rclone URI, through the crypt overlay when the
// destination has one.
func (a *rcloneArtifacts) where(name string) string { return remoteSubpathURI(a.dest, name) }

func (a *rcloneArtifacts) close() {}

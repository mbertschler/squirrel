package sync

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// snapshotTimeLayout is the ISO8601-ish, lexically sortable timestamp
// embedded in snapshot filenames. Millisecond precision so two snapshots
// taken in the same second (back-to-back CLI invocations, tests) get
// distinct names without a retry loop. Matches the layout the store and
// `db backup` already use so one backups/ directory stays consistent.
const snapshotTimeLayout = "20060102T150405.000Z"

// snapshotPrefix is the filename stem for snapshot-on-sync files. The
// run id follows so a snapshot is traceable to the exact runs row that
// produced it, and the leading timestamp keeps the directory lexically
// (and chronologically) sortable for rotation.
const snapshotPrefix = "index-"

// Snapshotter coordinates the snapshot-on-sync feature (#75) across the
// pairs of one `squirrel sync` invocation. It takes at most one VACUUM
// INTO snapshot — lazily, on the first pair that reaches a terminal
// success/partial state — and reuses that single file for the local tier
// and every destination ride-along. Construct one per CLI invocation with
// NewSnapshotter and pass it via Options.Snapshot; a nil *Snapshotter is
// the disabled state and every method is a safe no-op on it.
type Snapshotter struct {
	store     *store.Store
	rcl       *Rclone
	dir       string // resolved local snapshot directory
	keep      int    // local rotation bound (0 = no rotation)
	cloud     bool   // ride snapshots along to destination buckets
	cloudKeep int    // per-volume .squirrel-index/ rotation bound

	mu        sync.Mutex
	taken     bool   // the single VACUUM has been attempted
	localPath string // the snapshot file, "" if the VACUUM failed
	takeErr   error  // memoised local-snapshot/rotation error
}

// SnapshotConfig is the resolved input to NewSnapshotter. The CLI builds
// it from config.Backups, resolving Dir against the live DB path (an
// empty config.Backups.Dir means "<dirname(db)>/backups").
type SnapshotConfig struct {
	Dir       string
	Keep      int
	Cloud     bool
	CloudKeep int
}

// NewSnapshotter returns a Snapshotter ready to be shared across one
// invocation's pairs. The store backs the VACUUM INTO snapshot; the
// rclone wrapper (with its Config already written) backs the ride-along.
func NewSnapshotter(s *store.Store, rcl *Rclone, cfg SnapshotConfig) *Snapshotter {
	return &Snapshotter{
		store:     s,
		rcl:       rcl,
		dir:       cfg.Dir,
		keep:      cfg.Keep,
		cloud:     cfg.Cloud,
		cloudKeep: cfg.CloudKeep,
	}
}

// afterSync is the post-run hook called by Sync and SyncNode once the
// run's terminal state is committed. It takes (once) the local snapshot
// and, for destination syncs with cloud enabled, rides a copy along to
// the destination. Failures are surfaced on rep.SnapshotErr and never
// mutate rep.Status — the snapshot is defense-in-depth, not part of the
// sync's success contract. A nil receiver (feature disabled) is a no-op.
func (sn *Snapshotter) afterSync(ctx context.Context, rep *Report, vol *config.Volume, dest *config.Destination) {
	if sn == nil {
		return
	}
	// Only snapshot when the run actually reached a terminal good state
	// and wrote a row. Dry-run never populates RunID (and the CLI leaves
	// Snapshot nil for it anyway), so this also guards that path.
	if rep.RunID == 0 {
		return
	}
	if rep.Status != store.RunStatusSuccess && rep.Status != store.RunStatusPartial {
		return
	}

	localPath, err := sn.ensureLocalSnapshot(ctx, rep.RunID)
	if err != nil {
		rep.SnapshotErr = err
	}
	if localPath == "" {
		// The VACUUM itself failed; there is nothing to ride along.
		return
	}
	if dest == nil || !sn.cloud {
		return
	}
	if rideErr := sn.rideAlong(ctx, localPath, dest, vol.Name); rideErr != nil {
		rep.SnapshotErr = rideErr
	}
}

// ensureLocalSnapshot takes the single VACUUM INTO snapshot the first
// time it is called and memoises the result; later pairs reuse the same
// file (decision #1: one snapshot per invocation, fanned out — never one
// VACUUM per pair). The returned path is "" only when the VACUUM failed;
// a non-fatal rotation error is returned alongside a valid path so the
// ride-along still proceeds.
func (sn *Snapshotter) ensureLocalSnapshot(ctx context.Context, runID int64) (string, error) {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	if sn.taken {
		return sn.localPath, sn.takeErr
	}
	sn.taken = true

	name := fmt.Sprintf("%s%s-run-%d.db", snapshotPrefix, time.Now().UTC().Format(snapshotTimeLayout), runID)
	dst := filepath.Join(sn.dir, name)
	if err := sn.store.Backup(ctx, dst); err != nil {
		sn.takeErr = fmt.Errorf("snapshot index to %s: %w", dst, err)
		return "", sn.takeErr
	}
	sn.localPath = dst
	if _, err := rotateSnapshots(sn.dir, sn.keep); err != nil {
		// The snapshot we just wrote is valid; a rotation hiccup shouldn't
		// block the ride-along. Record it but keep the path.
		sn.takeErr = fmt.Errorf("rotate local snapshots in %s: %w", sn.dir, err)
	}
	return sn.localPath, sn.takeErr
}

// rideAlong uploads localPath to <dest>/<volume>/.squirrel-index/ via the
// rclone wrapper, then rotates that per-volume directory to at most
// cloudKeep snapshots. The uploaded copy keeps the snapshot's filename so
// the catalog is traceable to its producing run on the destination too.
func (sn *Snapshotter) rideAlong(ctx context.Context, localPath string, dest *config.Destination, volumeName string) error {
	dirURI := indexDirURI(dest, volumeName)
	name := filepath.Base(localPath)
	if err := sn.rcl.copyTo(ctx, localPath, dirURI+"/"+name); err != nil {
		return fmt.Errorf("ride-along upload to %s: %w", dest.Name, err)
	}
	if err := sn.rotateCloud(ctx, dirURI); err != nil {
		return fmt.Errorf("rotate %s/%s/%s: %w", dest.Name, volumeName, IndexDirName, err)
	}
	return nil
}

// rotateCloud lists the destination's .squirrel-index/ directory and
// deletes the oldest snapshots until at most cloudKeep remain. Snapshots
// are lexically sortable (decision #3), so "newest N" is the tail of the
// name-sorted list — no per-file metadata read required. cloudKeep<=0
// means "no rotation".
func (sn *Snapshotter) rotateCloud(ctx context.Context, dirURI string) error {
	if sn.cloudKeep <= 0 {
		return nil
	}
	names, err := sn.rcl.listSnapshots(ctx, dirURI)
	if err != nil {
		return err
	}
	if len(names) <= sn.cloudKeep {
		return nil
	}
	sort.Strings(names)
	for _, old := range names[:len(names)-sn.cloudKeep] {
		if err := sn.rcl.deleteFile(ctx, dirURI+"/"+old); err != nil {
			return err
		}
	}
	return nil
}

// indexDirURI returns the rclone URI of the per-volume .squirrel-index/
// directory under dest, addressed the same way the data transfer is
// (through the crypt overlay when the destination has one).
func indexDirURI(dest *config.Destination, volumeName string) string {
	return remoteSubpathURI(dest, path.Join(volumeName, IndexDirName))
}

// rotateSnapshots deletes the oldest snapshot-on-sync files in dir until
// only keep remain. Only the index-* files this routine writes are in the
// pool: the snapshot-on-sync directory defaults to the same backups/ dir
// the migration runner writes pre-migration-* snapshots to, and those are
// a buggy migration's only rollback surface — at the default keep=7 a
// sync cadence could rotate one away within days of a schema upgrade, so
// they are exempt here and only an explicit `db backup --keep` retention
// ever removes them. Unknown files are left untouched. keep<=0 means "no
// rotation".
func rotateSnapshots(dir string, keep int) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type snap struct {
		name    string
		modTime time.Time
	}
	var snaps []snap
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, snapshotPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		snaps = append(snaps, snap{name: name, modTime: info.ModTime()})
	}
	if len(snaps) <= keep {
		return nil, nil
	}
	// Order oldest-first. Break modtime ties by name: filenames embed a
	// sortable timestamp, so on filesystems with coarse mtime resolution
	// (or snapshots written within one tick) the name keeps the order
	// deterministic and chronological — without it, equal modtimes could
	// rotate away a newer snapshot.
	sort.Slice(snaps, func(i, j int) bool {
		if snaps[i].modTime.Equal(snaps[j].modTime) {
			return snaps[i].name < snaps[j].name
		}
		return snaps[i].modTime.Before(snaps[j].modTime)
	})
	var removed []string
	for _, s := range snaps[:len(snaps)-keep] {
		p := filepath.Join(dir, s.name)
		if err := os.Remove(p); err != nil {
			return removed, err
		}
		removed = append(removed, p)
	}
	return removed, nil
}

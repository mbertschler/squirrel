package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
// invocation's pairs. The store backs the VACUUM INTO snapshot; each
// push hands over the shelf its ride-along lands on.
func NewSnapshotter(s *store.Store, cfg SnapshotConfig) *Snapshotter {
	return &Snapshotter{
		store:     s,
		dir:       cfg.Dir,
		keep:      cfg.Keep,
		cloud:     cfg.Cloud,
		cloudKeep: cfg.CloudKeep,
	}
}

// snapshotShelf is a destination's per-volume .squirrel-index/ directory,
// where ride-along snapshots are kept.
type snapshotShelf interface {
	// upload copies the local snapshot at localPath onto the shelf as name.
	upload(ctx context.Context, localPath, name string) error
	// snapshots lists the snapshot names on the shelf.
	snapshots(ctx context.Context) ([]string, error)
	remove(ctx context.Context, name string) error
}

// afterSync is the post-run hook every push calls once the run's terminal
// state is committed. It takes (once) the local snapshot and, with cloud
// enabled, rides a copy along to shelf; a nil shelf (peer and kopia
// pushes) keeps the local snapshot only.
// Failures are surfaced on rep.SnapshotErr and never mutate rep.Status —
// the snapshot is defense-in-depth, not part of the sync's success
// contract. A nil receiver (feature disabled) is a no-op.
func (sn *Snapshotter) afterSync(ctx context.Context, rep *Report, shelf snapshotShelf) {
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
	if shelf == nil || !sn.cloud {
		return
	}
	if rideErr := sn.rideAlong(ctx, localPath, rep, shelf); rideErr != nil {
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

// rideAlong uploads localPath to the destination's
// <volume>/.squirrel-index/, then rotates that directory to at most
// cloudKeep snapshots. The uploaded copy keeps the snapshot's filename so
// the catalog is traceable to its producing run on the destination too.
func (sn *Snapshotter) rideAlong(ctx context.Context, localPath string, rep *Report, shelf snapshotShelf) error {
	if err := shelf.upload(ctx, localPath, filepath.Base(localPath)); err != nil {
		return fmt.Errorf("ride-along upload to %s: %w", rep.Destination, err)
	}
	if err := sn.rotateCloud(ctx, shelf); err != nil {
		return fmt.Errorf("rotate %s/%s/%s: %w", rep.Destination, rep.Volume, IndexDirName, err)
	}
	return nil
}

// rotateCloud deletes the oldest snapshots on the shelf until at most
// cloudKeep remain. Snapshots are lexically sortable (decision #3), so
// "newest N" is the tail of the name-sorted list — no per-file metadata
// read required. cloudKeep<=0 means "no rotation".
func (sn *Snapshotter) rotateCloud(ctx context.Context, shelf snapshotShelf) error {
	if sn.cloudKeep <= 0 {
		return nil
	}
	names, err := shelf.snapshots(ctx)
	if err != nil {
		return err
	}
	if len(names) <= sn.cloudKeep {
		return nil
	}
	sort.Strings(names)
	for _, old := range names[:len(names)-sn.cloudKeep] {
		if err := shelf.remove(ctx, old); err != nil {
			return err
		}
	}
	return nil
}

// isSnapshotName reports whether a file name in .squirrel-index/ is a
// ride-along snapshot, index-*.db, as opposed to a receipt.
func isSnapshotName(name string) bool {
	return strings.HasPrefix(name, snapshotPrefix) && strings.HasSuffix(name, ".db")
}

// rcloneShelf is the .squirrel-index/ directory of a destination rclone
// writes, addressed through the same overlay as its data.
type rcloneShelf struct {
	rcl *Rclone
	dir string // the directory's rclone URI
}

func (s rcloneShelf) upload(ctx context.Context, localPath, name string) error {
	return s.rcl.copyTo(ctx, localPath, s.dir+"/"+name)
}

func (s rcloneShelf) snapshots(ctx context.Context) ([]string, error) {
	return s.rcl.listSnapshots(ctx, s.dir)
}

func (s rcloneShelf) remove(ctx context.Context, name string) error {
	return s.rcl.deleteFile(ctx, s.dir+"/"+name)
}

// transportShelf is a native mirror's .squirrel-index/ directory, reached
// through the transport open returns: for a push, its guarded transport,
// whose name guard permits removing a snapshot there and nothing else.
type transportShelf struct {
	open func(context.Context) (transport, error)
	dir  string
}

// upload removes what a failed Put left behind, so recovery never offers
// a truncated snapshot as the newest.
func (s transportShelf) upload(ctx context.Context, localPath, name string) error {
	tr, err := s.open(ctx)
	if err != nil {
		return err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	dst := path.Join(s.dir, name)
	err = tr.Put(ctx, dst, f, fi.ModTime())
	if err != nil && !errors.Is(err, fs.ErrExist) {
		_ = tr.Remove(ctx, dst)
	}
	return err
}

func (s transportShelf) snapshots(ctx context.Context) ([]string, error) {
	tr, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	return listSnapshotNames(ctx, tr, s.dir)
}

// listSnapshotNames lists the snapshots in dir through tr; a dir that
// does not exist holds none.
func listSnapshotNames(ctx context.Context, tr transport, dir string) ([]string, error) {
	entries, err := tr.List(ctx, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.kind == kindFile && isSnapshotName(e.name) {
			names = append(names, e.name)
		}
	}
	return names, nil
}

func (s transportShelf) remove(ctx context.Context, name string) error {
	tr, err := s.open(ctx)
	if err != nil {
		return err
	}
	return tr.Remove(ctx, path.Join(s.dir, name))
}

// indexDirURI returns the rclone URI of the per-volume .squirrel-index/
// directory under dest, addressed the same way the data transfer is
// (through the crypt overlay when the destination has one).
func indexDirURI(dest *config.Destination, volumeName string) string {
	return remoteSubpathURI(dest, path.Join(namerFor(dest).volumeDir(volumeName), IndexDirName))
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

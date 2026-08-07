package sync

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbertschler/squirrel/config"
)

// IndexSnapshot is one ride-along index snapshot found on a destination —
// the catalog that explains the bytes stored beside it. Snapshotter writes
// these to <dest>/<volume>/.squirrel-index/ after a successful sync; this is
// the reading half, for the case the machine that wrote them is gone.
//
// TakenAt and RunID are parsed out of the filename rather than statted,
// because the name is the only metadata every rclone backend agrees on and
// snapshot names are deliberately lexically sortable. Either may be zero if
// a name does not follow the convention — the snapshot is still listed, and
// still restorable, because refusing to show an operator a recovery
// candidate over an unparsed filename would be the wrong trade in the one
// flow where they have least to work with.
type IndexSnapshot struct {
	Volume  string
	Name    string
	TakenAt time.Time
	RunID   int64
}

// Age reports how old the snapshot is relative to now, and whether that is
// knowable at all — an unparsed timestamp has no age, which a surface must
// say rather than render as "just now".
func (s IndexSnapshot) Age(now time.Time) (time.Duration, bool) {
	if s.TakenAt.IsZero() {
		return 0, false
	}
	return now.Sub(s.TakenAt), true
}

// DiscoverIndexSnapshots lists the ride-along index snapshots a destination
// holds for each named volume, newest first, then by volume. It is
// read-only: it lists, it does not fetch, so an operator can be told what
// is recoverable before anything is touched.
//
// A volume directory that does not exist yields no snapshots rather than an
// error — a destination that has simply never carried a given volume is a
// normal answer to "what do you have", not a failure.
func DiscoverIndexSnapshots(ctx context.Context, rcl *Rclone, dest *config.Destination, volumes []string) ([]IndexSnapshot, error) {
	var out []IndexSnapshot
	for _, vol := range volumes {
		names, err := rcl.listSnapshots(ctx, indexDirURI(dest, vol))
		if err != nil {
			return nil, fmt.Errorf("list index snapshots for %s/%s: %w", dest.Name, vol, err)
		}
		for _, name := range names {
			out = append(out, parseSnapshotName(vol, name))
		}
	}
	sortSnapshots(out)
	return out, nil
}

// sortSnapshots orders newest first, then by volume, then by name. Names
// are lexically sortable and carry the timestamp, so an unparsed name still
// lands in a stable place instead of floating.
func sortSnapshots(snaps []IndexSnapshot) {
	sort.Slice(snaps, func(i, j int) bool {
		a, b := snaps[i], snaps[j]
		if !a.TakenAt.Equal(b.TakenAt) {
			return a.TakenAt.After(b.TakenAt)
		}
		if a.Volume != b.Volume {
			return a.Volume < b.Volume
		}
		return a.Name < b.Name
	})
}

// parseSnapshotName reads the volume-scoped identity out of a snapshot
// filename of the form `index-<timestamp>-run-<id>.db`, the shape
// ensureLocalSnapshot writes. Anything that does not match keeps its name
// and reports a zero time and run.
func parseSnapshotName(volume, name string) IndexSnapshot {
	snap := IndexSnapshot{Volume: volume, Name: name}
	stem := strings.TrimSuffix(strings.TrimPrefix(name, snapshotPrefix), ".db")
	stamp, run, ok := strings.Cut(stem, "-run-")
	if !ok {
		return snap
	}
	if t, err := time.Parse(snapshotTimeLayout, stamp); err == nil {
		snap.TakenAt = t.UTC()
	}
	if id, err := strconv.ParseInt(run, 10, 64); err == nil {
		snap.RunID = id
	}
	return snap
}

// FetchIndexSnapshot downloads one snapshot to localPath. It only moves the
// file; validating that the bytes are a usable index at this binary's
// schema version is store.PreflightCheckSnapshot's job, and the caller runs
// it before letting the file near the live database.
func FetchIndexSnapshot(ctx context.Context, rcl *Rclone, dest *config.Destination, snap IndexSnapshot, localPath string) error {
	uri := indexDirURI(dest, snap.Volume) + "/" + snap.Name
	if err := rcl.copyTo(ctx, uri, localPath); err != nil {
		return fmt.Errorf("fetch index snapshot %s from %s: %w", snap.Name, dest.Name, err)
	}
	return nil
}

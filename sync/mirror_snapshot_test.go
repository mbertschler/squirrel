package sync

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/volmark"
)

// TestMirrorRideAlongThroughTheTransport: a native mirror's ride-along
// index snapshot lands in <volume>/.squirrel-index/ through the transport,
// on both backends, and rotation removes only the oldest snapshots,
// leaving every receipt in place.
func TestMirrorRideAlongThroughTheTransport(t *testing.T) {
	for _, b := range mirrorBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupMirrorFixtureOn(t, b)
			var runs []int64
			for i := range 3 {
				f.write(t, "a.txt", strings.Repeat("v", i+1))
				f.index(t)
				sn := NewSnapshotter(f.store, SnapshotConfig{Dir: t.TempDir(), Keep: 7, Cloud: true, CloudKeep: 2})
				rep, err := f.push(t, Options{Snapshot: sn})
				if err != nil || rep.SnapshotErr != nil {
					t.Fatalf("push %d: err=%v snapshot=%v", i, err, rep.SnapshotErr)
				}
				runs = append(runs, rep.RunID)
			}
			entries, err := os.ReadDir(f.dest(IndexDirName))
			if err != nil {
				t.Fatal(err)
			}
			var snaps, receipts []string
			for _, e := range entries {
				if isSnapshotName(e.Name()) {
					snaps = append(snaps, e.Name())
				} else {
					receipts = append(receipts, e.Name())
				}
			}
			if len(snaps) != 2 || !strings.HasSuffix(snaps[1], "-run-"+strconv.FormatInt(runs[2], 10)+".db") {
				t.Fatalf("snapshots = %v, want the newest two", snaps)
			}
			if len(receipts) != 3 {
				t.Fatalf("receipts = %v, want one per run", receipts)
			}
		})
	}
}

// TestRecoverFindsAMirrorsSnapshots: recover's discovery lists a native
// mirror's ride-along snapshots and fetches one through the transport,
// with no rclone wrapper, on both backends.
func TestRecoverFindsAMirrorsSnapshots(t *testing.T) {
	for _, b := range mirrorBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupMirrorFixtureOn(t, b)
			f.write(t, "a.txt", "alpha")
			f.index(t)
			sn := NewSnapshotter(f.store, SnapshotConfig{Dir: t.TempDir(), Keep: 7, Cloud: true, CloudKeep: 7})
			rep, err := f.push(t, Options{Snapshot: sn})
			if err != nil || rep.SnapshotErr != nil {
				t.Fatalf("push: err=%v snapshot=%v", err, rep.SnapshotErr)
			}
			ctx := context.Background()
			snaps, err := DiscoverIndexSnapshots(ctx, nil, f.pair.Destination, []string{"pics", "never-synced"})
			if err != nil || len(snaps) != 1 || snaps[0].RunID != rep.RunID {
				t.Fatalf("discovery = %+v, %v; want run %d's snapshot", snaps, err, rep.RunID)
			}
			local := filepath.Join(t.TempDir(), snaps[0].Name)
			if err := FetchIndexSnapshot(ctx, nil, f.pair.Destination, snaps[0], local); err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if fileHash(t, local) != fileHash(t, f.dest(IndexDirName+"/"+snaps[0].Name)) {
				t.Fatal("the fetched snapshot differs from the one on the destination")
			}
		})
	}
}

// partialPuts lands one byte of every Put whose name matches, then fails
// it, while every other call works on.
type partialPuts struct {
	transport
	match func(name string) bool
}

func (p partialPuts) Put(ctx context.Context, name string, r io.Reader, mtime time.Time) error {
	if !p.match(name) {
		return p.transport.Put(ctx, name, r, mtime)
	}
	if err := p.transport.Put(ctx, name, io.LimitReader(r, 1), mtime); err != nil {
		return err
	}
	return errors.New("the link dropped")
}

// withPartialPuts makes the fixture's handler land only one byte of every
// Put whose name match selects.
func (f *mirrorFixture) withPartialPuts(t *testing.T, match func(name string) bool) *mirrorHandler {
	t.Helper()
	h := f.handler(t)
	h.openTransport = func(ctx context.Context, d *config.Destination) (transport, error) {
		raw, err := openDestinationTransport(ctx, d)
		return partialPuts{transport: raw, match: match}, err
	}
	return h
}

// TestMirrorRideAlongLeavesNoPartialSnapshot: a snapshot upload that fails
// halfway leaves nothing in .squirrel-index/, so recovery never offers a
// truncated snapshot as the newest — even when the link is gone and
// nothing could be cleaned up. The push itself still succeeds, and the
// partial copy goes with its run's staging at the next push.
func TestMirrorRideAlongLeavesNoPartialSnapshot(t *testing.T) {
	f := setupMirrorFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	f.mustPush(t)
	h := f.withPartialPuts(t, func(name string) bool { return strings.Contains(name, StagingDirName+"/") })
	sn := NewSnapshotter(f.store, SnapshotConfig{Dir: t.TempDir(), Keep: 7, Cloud: true, CloudKeep: 7})
	rep, err := h.Push(context.Background(), Options{Snapshot: sn})
	if err != nil || rep.SnapshotErr == nil {
		t.Fatalf("push: err=%v snapshot=%v, want a successful push reporting the failed ride-along", err, rep.SnapshotErr)
	}
	entries, err := os.ReadDir(f.dest(IndexDirName))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if isSnapshotName(e.Name()) {
			t.Fatalf("the partial snapshot %s reached .squirrel-index", e.Name())
		}
	}
	clean := f.mustPush(t)
	f.checkStagingEmpty(t, clean.RunID)
}

// TestMirrorMarkerWriteIsAllOrNothing: an --init whose marker write fails
// halfway leaves no marker that does not parse, so the next --init
// succeeds.
func TestMirrorMarkerWriteIsAllOrNothing(t *testing.T) {
	f := setupMirrorFixture(t)
	if err := os.Remove(f.dest(volmark.MarkerName)); err != nil {
		t.Fatal(err)
	}
	f.write(t, "a.txt", "alpha")
	f.index(t)
	h := f.withPartialPuts(t, func(name string) bool { return name == markerStagingName("pics") })
	if _, err := h.Push(context.Background(), Options{Init: true}); err == nil {
		t.Fatal("an --init whose marker write failed succeeded")
	}
	if _, err := os.Stat(f.dest(volmark.MarkerName)); !os.IsNotExist(err) {
		t.Fatalf("a failed marker write left a marker behind: %v", err)
	}
	if rep, err := f.push(t, Options{Init: true}); err != nil || rep.Status != store.RunStatusSuccess {
		t.Fatalf("the next --init: status=%q err=%v", rep.Status, err)
	}
	if err := volmark.Validate(f.dest(""), "pics"); err != nil {
		t.Fatalf("marker after the retried --init: %v", err)
	}
}

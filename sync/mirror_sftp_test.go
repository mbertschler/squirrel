package sync

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/volmark"
)

// TestMirrorPushOverSFTP: a plain sftp mirror pushes natively with no
// rclone wrapper. --init writes the marker through the transport, a
// changed path moves its prior version into history, and every run
// leaves its receipt.
func TestMirrorPushOverSFTP(t *testing.T) {
	f := setupMirrorFixtureOn(t, sftpBackend)
	if err := os.Remove(filepath.Join(f.dst, "pics", volmark.MarkerName)); err != nil {
		t.Fatal(err)
	}
	f.write(t, "a.txt", "v1")
	f.write(t, "2024/cat.jpg", "meow")
	f.index(t)

	if _, err := f.push(t, Options{}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "--init") {
		t.Fatalf("push onto a root with no marker = %v, want a refusal naming --init", err)
	}
	first, err := f.push(t, Options{Init: true})
	if err != nil || first.Status != store.RunStatusSuccess {
		t.Fatalf("--init push: status=%q err=%v", first.Status, err)
	}
	if err := volmark.Validate(filepath.Join(f.dst, "pics"), "pics"); err != nil {
		t.Fatalf("marker after --init: %v", err)
	}
	f.write(t, "a.txt", "v2")
	f.index(t)
	second := f.mustPush(t)

	if f.readDest(t, "a.txt") != "v2" || f.readDest(t, "2024/cat.jpg") != "meow" {
		t.Fatal("destination bytes differ from the source")
	}
	if got := f.readDest(t, historyName("", second.RunID, "a.txt")); got != "v1" {
		t.Fatalf("history holds %q, want the displaced v1", got)
	}
	for _, rep := range []Report{first, second} {
		if _, err := os.Stat(f.dest(IndexDirName + "/run-" + strconv.FormatInt(rep.RunID, 10))); err != nil {
			t.Fatalf("receipt of run %d: %v", rep.RunID, err)
		}
	}
	f.checkInvariants(t, nil)
}

// TestMirrorOverSFTPRefusesAnUnknownHost: a server whose host key
// known_hosts does not pin never receives a byte, and the refusal is
// recorded as its own run.
func TestMirrorOverSFTPRefusesAnUnknownHost(t *testing.T) {
	f := setupMirrorFixtureOn(t, sftpBackend)
	empty := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.pair.Destination.Params["known_hosts_file"] = empty
	f.write(t, "a.txt", "alpha")
	f.index(t)

	rep, err := f.push(t, Options{Init: true})
	if !errors.Is(err, errUnknownHostKey) || rep.Status != store.RunStatusRefused {
		t.Fatalf("push to an unknown host: status=%q err=%v, want a refused run", rep.Status, err)
	}
	if _, err := os.Stat(f.dest("a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a.txt reached the unknown host: %v", err)
	}
	if runs := mustListSyncRuns(t, f.store); len(runs) != 1 || runs[0].Status != store.RunStatusRefused {
		t.Fatalf("runs = %+v, want the one refused run", runs)
	}
}

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

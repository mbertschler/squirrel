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
// leaves its receipt. Nothing is read back, so no row records a
// fingerprint and the vector stays presence+size.
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
	for _, r := range f.rows(t) {
		if r.Checksum.Valid {
			t.Fatalf("%s row = %+v, want no fingerprint over sftp", r.Path, r)
		}
	}
	if comps := volumeComponents(t, f.store, "pics", "usb"); len(comps) != 1 || comps[0].VerifyMethod != store.VerifyMethodPresenceSize {
		t.Fatalf("vector = %+v, want one presence+size component", comps)
	}
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

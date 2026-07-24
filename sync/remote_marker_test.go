package sync

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/volmark"
)

// sftpMarkerDestBlock declares a plain (no-crypt) sftp destination whose
// root the shim strips, so a marker at <volume>/.squirrel-volume
// materialises directly under the fake remote root — keeping the
// on-disk assertions in these tests independent of RemoteRoot arithmetic.
const sftpMarkerDestBlock = `[destinations.offsite]
type   = "sftp"
host   = "remote.invalid"
user   = "u"
root   = "/data"
layout = "content-addressed"
`

func setupRemoteMarkerFixture(t *testing.T) *caFixture {
	t.Helper()
	return setupCAFixture(t, sftpMarkerDestBlock, "/data")
}

// TestRemoteMarkerMissingWithoutInitRefuses: the threat model — a typo'd
// or unmounted remote root has no marker, and a sync without --init must
// refuse with the bootstrap hint rather than push into an unknown tree.
func TestRemoteMarkerMissingWithoutInitRefuses(t *testing.T) {
	f := setupRemoteMarkerFixture(t)
	dest := f.cfg.Destinations["offsite"]
	err := ensureRemoteDestinationMarker(context.Background(), f.store, f.rcl, dest, "fresh", false)
	if err == nil || !strings.Contains(err.Error(), "--init") || !strings.Contains(err.Error(), volmark.MarkerName) {
		t.Fatalf("want an --init hint naming %s, got %v", volmark.MarkerName, err)
	}
	if _, statErr := os.Stat(f.remotePath("fresh", volmark.MarkerName)); statErr == nil {
		t.Fatalf("a refused check must not write the marker")
	}
}

// TestRemoteMarkerInitWritesThenValidates: --init bootstraps the marker
// over rclone, stamping the writing node, and every later sync passes the
// gate without --init.
func TestRemoteMarkerInitWritesThenValidates(t *testing.T) {
	f := setupRemoteMarkerFixture(t)
	dest := f.cfg.Destinations["offsite"]
	ctx := context.Background()
	if err := ensureRemoteDestinationMarker(ctx, f.store, f.rcl, dest, "fresh", true); err != nil {
		t.Fatalf("init: %v", err)
	}
	data, err := os.ReadFile(f.remotePath("fresh", volmark.MarkerName))
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	m, err := volmark.Parse(data)
	if err != nil {
		t.Fatalf("parse written marker: %v", err)
	}
	if m.Volume != "fresh" || m.Node == "" {
		t.Fatalf("marker = %+v, want volume=fresh with a writing node", m)
	}
	if err := ensureRemoteDestinationMarker(ctx, f.store, f.rcl, dest, "fresh", false); err != nil {
		t.Fatalf("validate after init: %v", err)
	}
}

// TestRemoteMarkerMismatchAlwaysRefuses: a marker naming a different
// volume is refused with or without --init, and is never overwritten —
// the marker is the only trail distinguishing the two volumes.
func TestRemoteMarkerMismatchAlwaysRefuses(t *testing.T) {
	f := setupRemoteMarkerFixture(t)
	dest := f.cfg.Destinations["offsite"]
	if err := os.MkdirAll(f.remotePath("fresh"), 0o755); err != nil {
		t.Fatal(err)
	}
	other, err := volmark.Marshal(volmark.Marker{Volume: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.remotePath("fresh", volmark.MarkerName), other, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, init := range []bool{false, true} {
		err := ensureRemoteDestinationMarker(context.Background(), f.store, f.rcl, dest, "fresh", init)
		if err == nil || !strings.Contains(err.Error(), "different volume") {
			t.Fatalf("init=%v: want a mismatch refusal, got %v", init, err)
		}
	}
	data, err := os.ReadFile(f.remotePath("fresh", volmark.MarkerName))
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := volmark.Parse(data); m.Volume != "other" {
		t.Fatalf("mismatched marker was overwritten: now names %q", m.Volume)
	}
}

// TestRemoteMarkerReadErrorRefusesWithoutWriting is the safety-critical
// case: a read that fails for any reason other than a definite "not
// found" (here a simulated transient failure) must refuse the sync even
// under --init, never mistaking a reachability blip for a fresh root and
// clobbering a marker it could not read.
func TestRemoteMarkerReadErrorRefusesWithoutWriting(t *testing.T) {
	f := setupRemoteMarkerFixture(t)
	dest := f.cfg.Destinations["offsite"]
	t.Setenv("RCLONE_FAKE_CAT_FAIL", "Failed to cat: connection refused")
	err := ensureRemoteDestinationMarker(context.Background(), f.store, f.rcl, dest, "fresh", true)
	if err == nil || strings.Contains(err.Error(), "--init") {
		t.Fatalf("a transient read error must refuse hard, got %v", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error should surface the underlying failure, got %v", err)
	}
	if _, statErr := os.Stat(f.remotePath("fresh", volmark.MarkerName)); statErr == nil {
		t.Fatalf("a transient error must not bootstrap the marker")
	}
}

// TestContentAddressedRefusesUninitialisedRemote wires the gate into the
// content-addressed handler: a remote root with no marker refuses the
// push (with the --init hint) before any runs row is allocated.
func TestContentAddressedRefusesUninitialisedRemote(t *testing.T) {
	f := setupContentAddressedFixture(t)
	if err := os.Remove(f.remoteBlob("pics", volmark.MarkerName)); err != nil {
		t.Fatalf("remove seeded marker: %v", err)
	}
	f.write(t, "a.txt", "alpha")
	f.index(t)
	_, err := f.sync(t)
	if err == nil || !strings.Contains(err.Error(), "--init") {
		t.Fatalf("want an --init refusal, got %v", err)
	}
	runs, _ := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	for _, r := range runs {
		if r.Kind == store.RunKindSync {
			t.Fatalf("a refused push wrote a sync runs row: %+v", r)
		}
	}
}

// TestContentAddressedInitWritesRemoteMarker confirms --init bootstraps
// the marker on a content-addressed remote and the push then succeeds.
func TestContentAddressedInitWritesRemoteMarker(t *testing.T) {
	f := setupContentAddressedFixture(t)
	if err := os.Remove(f.remoteBlob("pics", volmark.MarkerName)); err != nil {
		t.Fatalf("remove seeded marker: %v", err)
	}
	f.write(t, "a.txt", "alpha")
	f.index(t)
	rep, err := RunPair(context.Background(), f.store, Tools{Rclone: f.rcl}, f.pair, Options{Init: true})
	if err != nil {
		t.Fatalf("sync with --init: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success", rep.Status)
	}
	data, err := os.ReadFile(f.remoteBlob("pics", volmark.MarkerName))
	if err != nil {
		t.Fatalf("marker not written on --init: %v", err)
	}
	if m, _ := volmark.Parse(data); m.Volume != "pics" {
		t.Fatalf("bootstrapped marker names %q, want pics", m.Volume)
	}
}

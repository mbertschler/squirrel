package sync

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// TestKeyedNamesDiscloseNothing is the property the feature exists for:
// after a push to an encrypted archive destination, nothing at the remote
// is named by a content hash or by a volume name. It walks the whole
// materialised tree rather than checking the paths the push reported, so an
// artifact written through some other path would still be caught.
func TestKeyedNamesDiscloseNothing(t *testing.T) {
	f := setupPackedFixture(t, "1KiB")
	small, large := "tiny-content", strings.Repeat("B", 4096)
	f.write(t, "small.txt", small)
	f.write(t, "big.bin", large)
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}

	secrets := []string{blake3Hex(small), blake3Hex(large), "pics", "docs"}
	var names []string
	if err := filepath.WalkDir(f.fakeRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(f.fakeRoot, p)
		if relErr != nil {
			return relErr
		}
		names = append(names, rel)
		return nil
	}); err != nil {
		t.Fatalf("walk remote: %v", err)
	}
	if len(names) < 4 {
		t.Fatalf("remote tree looks empty (%v); the push wrote nothing to inspect", names)
	}
	for _, rel := range names {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			part = strings.TrimSuffix(part, cryptDataSuffix)
			for _, secret := range secrets {
				if part == secret {
					t.Errorf("remote path %q discloses %q", rel, secret)
				}
			}
		}
	}
}

// TestPlainDestinationKeepsContentHashNames: without a crypt block there is
// no key to derive from, so the append-only layouts keep naming artifacts by
// content hash — the shape every existing destination already holds.
func TestPlainDestinationKeepsContentHashNames(t *testing.T) {
	f := setupPlainContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := os.Stat(f.remotePath(ObjectsDirName, blake3Hex("alpha"))); err != nil {
		t.Fatalf("object not at its content-hash name: %v", err)
	}
	if _, err := os.Stat(f.remotePath(NamingMarkerName)); err == nil {
		t.Fatalf("an unencrypted destination wrote a %s marker", NamingMarkerName)
	}
}

// TestKeyedNameDomainsSeparate: the same bytes under two roles derive two
// names, so the objects/, packs/, and per-volume namespaces stay
// independent even where a content hash and a pack key coincide.
func TestKeyedNameDomainsSeparate(t *testing.T) {
	dest := keyedTestDest(t)
	raw := mustHex(t, blake3Hex("alpha"))
	n := namerFor(dest)
	obj, pack := n.object(raw), n.pack(raw)
	if obj == pack {
		t.Fatalf("object and pack names collide for identical input: %s", obj)
	}
	if obj == hex.EncodeToString(raw) {
		t.Fatal("keyed object name equals the content hash")
	}
	if vol := n.volumeDir("pics"); vol == "pics" || vol == obj {
		t.Fatalf("volume directory name %q is not independently keyed", vol)
	}
	for _, name := range []string{obj, pack, n.volumeDir("pics")} {
		if len(name) != 64 {
			t.Errorf("keyed name %q is %d chars, want 64 hex", name, len(name))
		}
		if _, err := hex.DecodeString(name); err != nil {
			t.Errorf("keyed name %q is not hex: %v", name, err)
		}
	}
}

// TestKeyedNamesFollowTheKey: two destinations differing only in their
// crypt password name the same content differently, so one password's
// archive discloses nothing about another's.
func TestKeyedNamesFollowTheKey(t *testing.T) {
	raw := mustHex(t, blake3Hex("alpha"))
	a, b := keyedTestDest(t), keyedTestDest(t)
	b.Crypt.NamingKey = config.DeriveNamingKey("a-different-password", "")
	if namerFor(a).object(raw) == namerFor(b).object(raw) {
		t.Fatal("two naming keys produced one object name")
	}
}

// TestNamingMarkerBootstrapped: a push to a fresh encrypted root records
// the naming scheme, and a second push accepts the root it wrote.
func TestNamingMarkerBootstrapped(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	data, err := os.ReadFile(f.remoteBlob(NamingMarkerName))
	if err != nil {
		t.Fatalf("read %s: %v", NamingMarkerName, err)
	}
	var m namingMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s (%q): %v", NamingMarkerName, data, err)
	}
	if m.Naming != namingSchemeKeyed {
		t.Errorf("marker naming = %q, want %q", m.Naming, namingSchemeKeyed)
	}
	if m.CreatedAt == "" {
		t.Error("marker carries no created_at")
	}
	f.write(t, "b.txt", "bravo")
	f.index(t)
	if _, err := f.sync(t); err != nil {
		t.Fatalf("second sync against the root it bootstrapped: %v", err)
	}
}

// TestNamingRefusesPopulatedRootWithoutMarker: a root holding files but no
// marker was written under other names — the shape every encrypted archive
// had before keyed naming. Mixing keyed names in would leave those files
// named as they are while squirrel treated the root as private, so the push
// refuses and names the remedy.
func TestNamingRefusesPopulatedRootWithoutMarker(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.makeLegacyRoot(t, "previously-uploaded")
	f.write(t, "a.txt", "alpha")
	f.index(t)

	rep, err := f.sync(t)
	if err == nil || !strings.Contains(err.Error(), NamingMarkerName) {
		t.Fatalf("want a refusal naming %s, got %v", NamingMarkerName, err)
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("the naming gate should be a refusal (ErrRefused), got %v", err)
	}
	if !strings.Contains(err.Error(), "destination reset") {
		t.Errorf("refusal does not name the remedy: %v", err)
	}
	for _, r := range mustListRuns(t, f) {
		if r.Kind == store.RunKindSync {
			t.Fatalf("a refused push wrote a sync runs row: %+v", r)
		}
	}
	if rep.Status == store.RunStatusSuccess {
		t.Errorf("Status = %q on a refused push", rep.Status)
	}
}

// TestNamingRefusesUnknownScheme: a marker naming a scheme this binary does
// not write refuses rather than writing a second naming generation into the
// root — the forward-compatibility half of the gate.
func TestNamingRefusesUnknownScheme(t *testing.T) {
	f := setupContentAddressedFixture(t)
	writeRemoteJSON(t, f, NamingMarkerName, namingMarker{Naming: "keyed-blake3-v9"})
	f.write(t, "a.txt", "alpha")
	f.index(t)

	_, err := f.sync(t)
	if err == nil || !strings.Contains(err.Error(), "keyed-blake3-v9") {
		t.Fatalf("want a refusal naming the recorded scheme, got %v", err)
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("want ErrRefused, got %v", err)
	}
}

// TestNamingRefusesUnreadableMarker: the marker is the only record of how a
// root was named, so one that will not parse refuses too. Treating it as
// absent would write keyed names into a root whose naming is unknown.
func TestNamingRefusesUnreadableMarker(t *testing.T) {
	f := setupContentAddressedFixture(t)
	p := f.remoteBlob(NamingMarkerName)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.write(t, "a.txt", "alpha")
	f.index(t)

	_, err := f.sync(t)
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("want an unreadable-marker refusal, got %v", err)
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("want ErrRefused, got %v", err)
	}
}

// TestNamingGateHoldsOnDryRun: a dry run asks the same question and writes
// no marker — an honest rehearsal refuses what the real push would refuse.
func TestNamingGateHoldsOnDryRun(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.makeLegacyRoot(t, "previously-uploaded")
	f.write(t, "a.txt", "alpha")
	f.index(t)

	_, err := RunPair(context.Background(), f.store, Tools{Rclone: f.rcl}, f.pair, Options{DryRun: true})
	if err == nil || !strings.Contains(err.Error(), NamingMarkerName) {
		t.Fatalf("want the dry run refused by the naming gate, got %v", err)
	}
}

// TestDryRunWritesNoNamingMarker: the bootstrap is the real push's, so a
// dry run against a fresh root leaves the remote as it found it.
func TestDryRunWritesNoNamingMarker(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.wipeRemote(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if _, err := RunPair(context.Background(), f.store, Tools{Rclone: f.rcl}, f.pair, Options{DryRun: true}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if _, err := os.Stat(f.remoteBlob(NamingMarkerName)); err == nil {
		t.Fatalf("a dry run wrote %s", NamingMarkerName)
	}
}

// TestKeyedVolumeDirFoundByRecovery covers the disaster-recovery entry
// point across the keyed volume directory: the ride-along index snapshot
// lands under the keyed directory, and DiscoverIndexSnapshots — which
// derives that directory from the volume names in the config — finds it
// again.
//
// It is the one path where a naming mistake would be silent rather than
// loud: a wrongly derived directory lists as absent, and absent is
// reported as "this destination holds no snapshots for you" at the moment
// an operator has least to work with.
func TestKeyedVolumeDirFoundByRecovery(t *testing.T) {
	f := setupContentAddressedFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	sn := NewSnapshotter(f.store, f.rcl, SnapshotConfig{Dir: t.TempDir(), Keep: 7, Cloud: true, CloudKeep: 7})
	rep, err := RunPair(context.Background(), f.store, Tools{Rclone: f.rcl}, f.pair, Options{Snapshot: sn})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if rep.SnapshotErr != nil {
		t.Fatalf("SnapshotErr = %v, want nil", rep.SnapshotErr)
	}

	keyed := namerFor(f.dest()).volumeDir("pics")
	if keyed == "pics" {
		t.Fatal("volume directory was not keyed; the test proves nothing")
	}
	if _, err := os.Stat(f.remotePath(keyed, IndexDirName)); err != nil {
		t.Fatalf("ride-along snapshot dir not under the keyed volume directory: %v", err)
	}

	snaps, err := DiscoverIndexSnapshots(context.Background(), f.rcl, f.dest(), []string{"pics", "docs"})
	if err != nil {
		t.Fatalf("DiscoverIndexSnapshots: %v", err)
	}
	var found *IndexSnapshot
	for i := range snaps {
		if snaps[i].Volume == "pics" {
			found = &snaps[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("recovery found no snapshot for pics under its keyed directory: %+v", snaps)
	}
	if found.RunID != rep.RunID {
		t.Errorf("snapshot RunID = %d, want %d", found.RunID, rep.RunID)
	}
	if found.TakenAt.IsZero() {
		t.Error("snapshot timestamp did not parse; the name does not follow the convention")
	}
}

// keyedTestDest is a destination configured exactly as an encrypted archive
// one, for the pure naming assertions that need no remote.
func keyedTestDest(t *testing.T) *config.Destination {
	t.Helper()
	d := &config.Destination{
		Name:   "offsite",
		Type:   "sftp",
		Root:   "/data",
		Layout: config.LayoutContentAddressed,
		Crypt:  &config.Crypt{Password: "irrelevant-here"},
	}
	d.Crypt.NamingKey = config.DeriveNamingKey("hunter2", "the-salt")
	if !d.HidesArtifactNames() {
		t.Fatal("fixture destination does not hide artifact names")
	}
	return d
}

func mustListRuns(t *testing.T, f *caFixture) []store.Run {
	t.Helper()
	runs, err := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	return runs
}

// writeRemoteJSON materialises a JSON artifact at the destination root as
// the crypt overlay would leave it, for the gate's refusal paths.
func writeRemoteJSON(t *testing.T, f *caFixture, name string, v any) {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode %s: %v", name, err)
	}
	p := f.remoteBlob(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

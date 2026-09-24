package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/volmark"
)

// nativeContentFixture is one volume, "pics", pushed to a content-addressed
// or packed destination squirrel writes itself, "vault": a directory on
// this machine, or one an in-process sftp server serves. dst is where the
// destination's bytes land on this machine either way.
type nativeContentFixture struct {
	store *store.Store
	pair  Pair
	src   string
	dst   string
}

// contentBackend is where a native content fixture's destination lives.
type contentBackend struct {
	name     string
	settings func(t *testing.T, dst string) string
}

var (
	localContent = contentBackend{"local", func(_ *testing.T, dst string) string {
		return fmt.Sprintf("type = \"local\"\nroot = %q\n", dst)
	}}
	// sftpContent runs sha256sum, as an OpenSSH host does.
	sftpContent = contentBackend{"sftp", func(t *testing.T, dst string) string {
		return sftpSettings(t, dst, map[string]string{"sha256sum": "sha256"})
	}}
	// sftpContentNoPrograms runs no programs, the cloudbox shape.
	sftpContentNoPrograms = contentBackend{"sftp without programs", func(t *testing.T, dst string) string {
		return sftpSettings(t, dst, nil)
	}}
	contentBackends = []contentBackend{localContent, sftpContent}
	contentLayouts  = []string{config.LayoutContentAddressed, config.LayoutPacked}
)

// setupNativeContentFixture declares vault on b with layout; a packed one
// packs content under 8 bytes. The destination root exists, and on sftp it
// carries the volume marker.
func setupNativeContentFixture(t *testing.T, b contentBackend, layout string) *nativeContentFixture {
	t.Helper()
	root := t.TempDir()
	f := &nativeContentFixture{src: filepath.Join(root, "src"), dst: filepath.Join(root, "vault")}
	for _, d := range []string{f.src, filepath.Join(f.dst, "pics")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := volmark.Write(filepath.Join(f.dst, "pics"), volmark.Marker{Volume: "pics"}); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	s, err := store.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	f.store = s
	body := "[destinations.vault]\n" + b.settings(t, f.dst) + fmt.Sprintf("layout = %q\n", layout)
	if layout == config.LayoutPacked {
		body += "pack_threshold = \"8B\"\n"
	}
	body += "\n[volumes.pics]\npath = \"" + f.src + "\"\nsync_to = [\"vault\"]\n"
	cfgPath := filepath.Join(root, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	f.pair = Pair{Volume: cfg.Volumes["pics"], Destination: cfg.Destinations["vault"]}
	return f
}

func (f *nativeContentFixture) write(t *testing.T, rel, body string) {
	t.Helper()
	p := filepath.Join(f.src, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *nativeContentFixture) index(t *testing.T) {
	t.Helper()
	if _, err := index.Index(context.Background(), f.store, f.src, index.Options{Name: "pics"}); err != nil {
		t.Fatalf("index: %v", err)
	}
}

// push runs one push with no rclone wrapper at all.
func (f *nativeContentFixture) push(t *testing.T, opts Options) (Report, error) {
	t.Helper()
	return RunPair(context.Background(), f.store, Tools{}, f.pair, opts)
}

func (f *nativeContentFixture) mustPush(t *testing.T) Report {
	t.Helper()
	rep, err := f.push(t, Options{})
	if err != nil || rep.Status != store.RunStatusSuccess {
		t.Fatalf("push: status=%q err=%v warnings=%v", rep.Status, err, rep.Warnings)
	}
	return rep
}

// pushCrashing runs one push whose transport crashes at the first call
// crashAt selects.
func (f *nativeContentFixture) pushCrashing(t *testing.T, crashAt func(transportCall) bool) (Report, error) {
	t.Helper()
	h, err := HandlerFor(f.store, Tools{}, f.pair)
	if err != nil {
		t.Fatalf("HandlerFor: %v", err)
	}
	var art artifactStore
	switch h := h.(type) {
	case *contentAddressedHandler:
		art = h.art
	case *packedHandler:
		art = h.art
	}
	root := art.(*transportArtifacts).destinationRoot
	root.openTransport = func(ctx context.Context, d *config.Destination) (transport, error) {
		raw, err := openDestinationTransport(ctx, d)
		if err != nil {
			return nil, err
		}
		return &faultTransport{transport: raw, crashAt: crashAt, mode: crashAfter}, nil
	}
	return h.Push(context.Background(), Options{})
}

func (f *nativeContentFixture) objectPath(body string) string {
	return filepath.Join(f.dst, ObjectsDirName, blake3Hex(body))
}

func (f *nativeContentFixture) remoteObject(t *testing.T, body string) (store.RemoteObject, bool) {
	t.Helper()
	ctx := context.Background()
	v, err := f.store.GetVolumeByName(ctx, "pics")
	if err != nil {
		t.Fatal(err)
	}
	present, err := f.store.ListPresentContent(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range present {
		if hex.EncodeToString(d.Blake3) == blake3Hex(body) {
			obj, err := f.store.GetRemoteObject(ctx, d.ContentID, "vault")
			return obj, err == nil
		}
	}
	t.Fatalf("no present content holds %q", body)
	return store.RemoteObject{}, false
}

// wantFingerprint is what a native push records for body's bytes on b: the
// BLAKE3 a local read-back confirmed, or the server's sha256.
func wantFingerprint(b contentBackend, body string) (string, string) {
	if b.name == localContent.name {
		return store.ChecksumAlgoBlake3, blake3Hex(body)
	}
	sum := sha256.Sum256([]byte(body))
	return "sha256", hex.EncodeToString(sum[:])
}

// TestNativeContentPushWithoutRclone: both content layouts push to a local
// disk and to an sftp server with no rclone wrapper. Every artifact lands
// at its name with its bytes, records the fingerprint its landing confirmed
// — a read-back BLAKE3 locally, the server's sha256 over sftp — and the
// vector advances as fingerprint-verified. The next push clears the first
// run's staging.
func TestNativeContentPushWithoutRclone(t *testing.T) {
	for _, b := range contentBackends {
		for _, layout := range contentLayouts {
			t.Run(b.name+"/"+layout, func(t *testing.T) {
				f := setupNativeContentFixture(t, b, layout)
				f.write(t, "a.txt", "alpha")
				f.write(t, "big.txt", "a larger file")
				f.index(t)
				first := f.mustPush(t)

				if got, err := os.ReadFile(f.objectPath("a larger file")); err != nil || string(got) != "a larger file" {
					t.Fatalf("object = %q, %v", got, err)
				}
				obj, ok := f.remoteObject(t, "a larger file")
				algo, value := wantFingerprint(b, "a larger file")
				if !ok || obj.ChecksumAlgo.String != algo || obj.Checksum.String != value || !obj.VerifiedAtNs.Valid {
					t.Fatalf("remote object = %+v, want %s %s recorded at landing", obj, algo, value)
				}
				segment := filepath.Join(f.dst, "pics", ManifestDirName, fmt.Sprintf("run-%d", first.RunID))
				if _, err := os.Stat(segment); err != nil {
					t.Fatalf("manifest segment: %v", err)
				}
				if layout == config.LayoutPacked {
					if _, err := os.Stat(filepath.Join(f.dst, PacksDirName, fmt.Sprintf("map-%d", first.RunID))); err != nil {
						t.Fatalf("placement map: %v", err)
					}
					if _, err := os.Stat(f.objectPath("alpha")); err == nil {
						t.Fatal("content under the threshold landed as an object")
					}
				}
				if comps := volumeComponents(t, f.store, "pics", "vault"); len(comps) != 1 || comps[0].VerifyMethod != store.VerifyMethodFingerprint {
					t.Fatalf("vector = %+v, want one fingerprint-verified component", comps)
				}

				f.mustPush(t)
				if _, err := os.Stat(filepath.Join(f.dst, stagingName("pics", first.RunID, ""))); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("the next push left the first run's staging: %v", err)
				}
			})
		}
	}
}

// TestNativeContentWithoutServerHash: an sftp server that runs no programs
// still takes every artifact, whole, but fingerprints nothing: the objects
// stay pending with a warning and the vector stays presence+size.
func TestNativeContentWithoutServerHash(t *testing.T) {
	f := setupNativeContentFixture(t, sftpContentNoPrograms, config.LayoutContentAddressed)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	rep := f.mustPush(t)
	if obj, ok := f.remoteObject(t, "alpha"); !ok || obj.Checksum.Valid {
		t.Fatalf("remote object = %+v, want recorded with a pending fingerprint", obj)
	}
	if !strings.Contains(strings.Join(rep.Warnings, "\n"), "without a fingerprint") {
		t.Fatalf("warnings = %v, want the pending fingerprints named", rep.Warnings)
	}
	if comps := volumeComponents(t, f.store, "pics", "vault"); len(comps) != 1 || comps[0].VerifyMethod != store.VerifyMethodPresenceSize {
		t.Fatalf("vector = %+v, want one presence+size component", comps)
	}
}

// TestNativeContentAdoptsAnArtifactItAlreadyLanded: a push that crashed
// between landing an object and recording it leaves the object at its
// name. The next push finds it there, confirms its bytes, and records it
// instead of replacing it.
func TestNativeContentAdoptsAnArtifactItAlreadyLanded(t *testing.T) {
	for _, b := range contentBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupNativeContentFixture(t, b, config.LayoutContentAddressed)
			f.write(t, "a.txt", "alpha")
			f.index(t)
			if _, err := f.pushCrashing(t, func(c transportCall) bool {
				return c.op == "rename" && strings.HasPrefix(c.to, ObjectsDirName+"/")
			}); err == nil {
				t.Fatal("the crashing push succeeded")
			}
			if _, err := os.Stat(f.objectPath("alpha")); err != nil {
				t.Fatalf("the crash left no object: %v", err)
			}
			if _, ok := f.remoteObject(t, "alpha"); ok {
				t.Fatal("the crashed push recorded the object")
			}

			f.mustPush(t)
			obj, ok := f.remoteObject(t, "alpha")
			algo, value := wantFingerprint(b, "alpha")
			if !ok || obj.ChecksumAlgo.String != algo || obj.Checksum.String != value {
				t.Fatalf("remote object = %+v, want the landed object adopted with %s %s", obj, algo, value)
			}
		})
	}
}

// TestNativeContentRefusesOtherBytesAtAnArtifactName: bytes squirrel did
// not record sitting at an object's name are never replaced: the object
// fails, the push fails before its segment, and the bytes stay.
func TestNativeContentRefusesOtherBytesAtAnArtifactName(t *testing.T) {
	f := setupNativeContentFixture(t, localContent, config.LayoutContentAddressed)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	if err := os.MkdirAll(filepath.Join(f.dst, ObjectsDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.objectPath("alpha"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := f.push(t, Options{})
	if err == nil || len(rep.RcloneResult.FailedFiles) != 1 || !strings.Contains(rep.RcloneResult.FailedFiles[0].Message, errReadBackMismatch.Error()) {
		t.Fatalf("push: err=%v failed=%+v, want the object refused as other bytes", err, rep.RcloneResult.FailedFiles)
	}
	if got, _ := os.ReadFile(f.objectPath("alpha")); string(got) != "other" {
		t.Fatalf("object = %q, want the bytes that were there left alone", got)
	}
	if _, ok := f.remoteObject(t, "alpha"); ok {
		t.Fatal("the refused object was recorded")
	}
}

// TestNativeContentMarkerGate: a native content destination is gated on
// its volume marker, as a native mirror is, and --init writes it through
// the transport. On a local disk --init also creates a missing root.
func TestNativeContentMarkerGate(t *testing.T) {
	for _, b := range contentBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupNativeContentFixture(t, b, config.LayoutPacked)
			bare := filepath.Join(f.dst, "pics")
			if b.name == localContent.name {
				bare = f.dst
			}
			if err := os.RemoveAll(bare); err != nil {
				t.Fatal(err)
			}
			f.write(t, "a.txt", "alpha")
			f.index(t)
			if _, err := f.push(t, Options{}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "--init") {
				t.Fatalf("push without a marker = %v, want a refusal naming --init", err)
			}
			if rep, err := f.push(t, Options{Init: true}); err != nil || rep.Status != store.RunStatusSuccess {
				t.Fatalf("--init push: status=%q err=%v", rep.Status, err)
			}
			if err := volmark.Validate(filepath.Join(f.dst, "pics"), "pics"); err != nil {
				t.Fatalf("marker after --init: %v", err)
			}
		})
	}
}

// TestNativeContentDriftLeavesNothingAtTheName: a source whose bytes
// drifted from their indexed hash is refused after staging; nothing lands
// at the object's name, and the next push clears the staged copy and lands
// the honest bytes.
func TestNativeContentDriftLeavesNothingAtTheName(t *testing.T) {
	f := setupNativeContentFixture(t, localContent, config.LayoutContentAddressed)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	src := filepath.Join(f.src, "a.txt")
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, "a.txt", "ALPHA")
	if err := os.Chtimes(src, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	rep, err := f.push(t, Options{})
	if err == nil || !strings.Contains(err.Error(), "drifting") {
		t.Fatalf("push = %v, want a drift refusal", err)
	}
	if _, err := os.Stat(f.objectPath("alpha")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("drifted bytes reached the object name: %v", err)
	}
	f.write(t, "a.txt", "alpha")
	if err := os.Chtimes(src, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	f.mustPush(t)
	if got, _ := os.ReadFile(f.objectPath("alpha")); string(got) != "alpha" {
		t.Fatalf("object = %q, want the honest bytes", got)
	}
	if _, err := os.Stat(filepath.Join(f.dst, stagingName("pics", rep.RunID, ""))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the drifted run's staging is still there: %v", err)
	}
}

// TestNativeContentRestoreWithoutRclone: both content layouts restore from
// a local disk and from an sftp server with no rclone wrapper, every byte
// re-hashed on the way down.
func TestNativeContentRestoreWithoutRclone(t *testing.T) {
	for _, b := range contentBackends {
		for _, layout := range contentLayouts {
			t.Run(b.name+"/"+layout, func(t *testing.T) {
				f := setupNativeContentFixture(t, b, layout)
				f.write(t, "a.txt", "alpha")
				f.write(t, "2024/big.txt", "a larger file")
				f.index(t)
				f.mustPush(t)
				to := t.TempDir()
				rep, err := Restore(context.Background(), f.store, nil, f.pair.Volume, f.pair.Destination, RestoreOptions{ToPath: to})
				if err != nil || rep.Status != store.RunStatusSuccess {
					t.Fatalf("restore: status=%q err=%v", rep.Status, err)
				}
				for rel, want := range map[string]string{"a.txt": "alpha", "2024/big.txt": "a larger file"} {
					if got, err := os.ReadFile(filepath.Join(to, filepath.FromSlash(rel))); err != nil || string(got) != want {
						t.Fatalf("%s = %q, %v; want %q", rel, got, err, want)
					}
				}
			})
		}
	}
}

// TestNativeContentRideAlongAndRecover: a native content push rides the
// index snapshot along through the transport, and recover's discovery lists
// and fetches it without rclone.
func TestNativeContentRideAlongAndRecover(t *testing.T) {
	for _, b := range contentBackends {
		t.Run(b.name, func(t *testing.T) {
			f := setupNativeContentFixture(t, b, config.LayoutContentAddressed)
			f.write(t, "a.txt", "alpha")
			f.index(t)
			sn := NewSnapshotter(f.store, SnapshotConfig{Dir: t.TempDir(), Keep: 7, Cloud: true, CloudKeep: 7})
			rep, err := f.push(t, Options{Snapshot: sn})
			if err != nil || rep.SnapshotErr != nil {
				t.Fatalf("push: err=%v snapshot=%v", err, rep.SnapshotErr)
			}
			ctx := context.Background()
			snaps, err := DiscoverIndexSnapshots(ctx, nil, f.pair.Destination, []string{"pics"})
			if err != nil || len(snaps) != 1 || snaps[0].RunID != rep.RunID {
				t.Fatalf("snapshots = %+v, %v; want run %d's", snaps, err, rep.RunID)
			}
			local := filepath.Join(t.TempDir(), "index.db")
			if err := FetchIndexSnapshot(ctx, nil, f.pair.Destination, snaps[0], local); err != nil {
				t.Fatalf("FetchIndexSnapshot: %v", err)
			}
		})
	}
}

func (f *nativeContentFixture) verify(t *testing.T) RemoteVerifyReport {
	t.Helper()
	rep, err := VerifyRemote(context.Background(), f.store, nil, f.pair.Destination)
	if err != nil {
		t.Fatalf("VerifyRemote: %v", err)
	}
	return rep
}

// TestNativeContentVerifyWithoutRclone: verify re-reads a native content
// destination through the transport — BLAKE3 against the content hash or
// pack key on a local disk, the server's hash command over sftp — and
// finds an artifact changed in place or gone.
func TestNativeContentVerifyWithoutRclone(t *testing.T) {
	for _, b := range contentBackends {
		for _, layout := range contentLayouts {
			t.Run(b.name+"/"+layout, func(t *testing.T) {
				f := setupNativeContentFixture(t, b, layout)
				f.write(t, "a.txt", "alpha")
				f.write(t, "big.txt", "a larger file")
				f.write(t, "gone.txt", "gone, soon")
				f.index(t)
				f.mustPush(t)
				rep := f.verify(t)
				objects, packs := 3, 0
				if layout == config.LayoutPacked {
					objects, packs = 2, 1
				}
				if !rep.Clean() || rep.Verified != objects || rep.PacksVerified != packs {
					t.Fatalf("rep = %+v, want a clean pass re-confirming every artifact", rep)
				}

				if err := os.WriteFile(f.objectPath("a larger file"), []byte("A LARGER FILE"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(f.objectPath("gone, soon")); err != nil {
					t.Fatal(err)
				}
				if layout == config.LayoutPacked {
					f.flipPack(t)
				}
				rep, err := VerifyRemote(context.Background(), f.store, nil, f.pair.Destination)
				if err != nil || len(rep.Mismatched) != 1 || len(rep.Missing) != 1 || len(rep.PackMismatched) != packs || !rep.AlarmRaised {
					t.Fatalf("rep = %+v, %v; want one object changed, one gone, every pack changed, and the alarm raised", rep, err)
				}
			})
		}
	}
}

// flipPack changes the first byte of every pack in place.
func (f *nativeContentFixture) flipPack(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.dst, PacksDirName))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), packMapPrefix) {
			continue
		}
		p := filepath.Join(f.dst, PacksDirName, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b[0] ^= 0xff
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNativeContentVerifyWithoutAServerHash: over sftp, an artifact the
// server cannot hash this pass — it lost its command, or the row records
// another hash_algo — is neither re-confirmed nor a finding.
func TestNativeContentVerifyWithoutAServerHash(t *testing.T) {
	f := setupNativeContentFixture(t, sftpContent, config.LayoutContentAddressed)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	f.mustPush(t)
	f.pair.Destination.HashAlgo = "sha1"
	rep := f.verify(t)
	if !rep.Clean() || rep.Unchecked != 1 || rep.Verified != 0 || rep.AlarmRaised {
		t.Fatalf("rep = %+v, want the object left unchecked and no alarm", rep)
	}
}

// TestNativeContentVerifyRefusesAnUnmountedRoot: a native content root
// without its volume markers fails the pass instead of reporting every
// object missing.
func TestNativeContentVerifyRefusesAnUnmountedRoot(t *testing.T) {
	f := setupNativeContentFixture(t, localContent, config.LayoutContentAddressed)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	f.mustPush(t)
	entries, err := os.ReadDir(f.dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(f.dst, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := VerifyRemote(context.Background(), f.store, nil, f.pair.Destination)
	if err == nil || !strings.Contains(err.Error(), "nothing was checked") || rep.AlarmRaised {
		t.Fatalf("VerifyRemote = %+v, %v; want the pass refused without an alarm", rep, err)
	}
}

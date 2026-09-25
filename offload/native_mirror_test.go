package offload

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/sync"
	"github.com/mbertschler/squirrel/volmark"
)

// pushNativeMirror pushes the indexed volume at root to a native mirror
// on a local disk at dst, as `squirrel sync` does, and returns the
// destination.
func pushNativeMirror(t *testing.T, s *store.Store, root, dst string) *config.Destination {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dst, volName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := volmark.Write(filepath.Join(dst, volName), volmark.Marker{Volume: volName}); err != nil {
		t.Fatalf("seed destination marker: %v", err)
	}
	dest := &config.Destination{Name: "usb", Type: "local", Root: dst, Layout: config.LayoutMirror}
	pair := sync.Pair{Volume: &config.Volume{Name: volName, Path: root}, Destination: dest}
	rep, err := sync.RunPair(context.Background(), s, sync.Tools{}, pair, sync.Options{})
	if err != nil || rep.Status != store.RunStatusSuccess {
		t.Fatalf("push: status=%q err=%v warnings=%v", rep.Status, err, rep.Warnings)
	}
	return dest
}

// TestOffloadGatesOnNativeLocalMirror: a native local mirror reads every
// copy back through BLAKE3, so its push alone lets offload delete the local
// bytes, with or without a verify cadence. Once a verify pass finds a copy
// gone, that copy stops vouching and its file stays.
func TestOffloadGatesOnNativeLocalMirror(t *testing.T) {
	for _, cadenced := range []bool{false, true} {
		t.Run(map[bool]string{false: "no cadence", true: "verify cadence"}[cadenced], func(t *testing.T) {
			root, dst := t.TempDir(), t.TempDir()
			writeFile(t, filepath.Join(root, "a.txt"), "alpha")
			writeFile(t, filepath.Join(root, "b.txt"), "bravo")
			s := setupStore(t)
			ctx := context.Background()
			indexVolume(t, s, root)
			dest := pushNativeMirror(t, s, root, dst)

			if err := os.Remove(filepath.Join(dst, volName, "b.txt")); err != nil {
				t.Fatal(err)
			}
			if rep, err := sync.VerifyRemote(ctx, s, nil, dest); err != nil || len(rep.PathsMissing) != 1 {
				t.Fatalf("verify = %+v, %v; want b.txt found missing", rep, err)
			}

			rep, err := Offload(ctx, s, root, Options{
				Name: volName, Paths: []string{"."}, Require: []string{"usb"},
				RequireDests:   map[string]*config.Destination{"usb": dest},
				VerifyCadenced: map[string]bool{"usb": cadenced},
			})
			if err != nil {
				t.Fatalf("Offload: %v", err)
			}
			oneResult(t, rep, "a.txt", OutcomeOffloaded)
			mustBeGone(t, filepath.Join(root, "a.txt"))
			oneFailure(t, oneResult(t, rep, "b.txt", OutcomeNotDurable), "usb", FailureNotVerified)
			mustExist(t, filepath.Join(root, "b.txt"))
		})
	}
}

// TestOffloadRefusesNativeSFTPMirrorUpFront: a native sftp mirror reads
// nothing back, so naming one aborts the offload before any file is
// walked, and the reason says so.
func TestOffloadRefusesNativeSFTPMirrorUpFront(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	indexVolume(t, s, root)
	dests := map[string]*config.Destination{"box": {Name: "box", Type: "sftp", Layout: config.LayoutMirror}}
	_, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"box"}, RequireDests: dests,
	})
	if err == nil || !strings.Contains(err.Error(), "reads nothing back") {
		t.Fatalf("err = %v, want an up-front refusal naming the reason", err)
	}
	mustExist(t, filepath.Join(root, "a.txt"))
}

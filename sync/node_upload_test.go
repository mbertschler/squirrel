package sync

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

func writeVolumeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func receiverRow(t *testing.T, f *nodeFixture, rel string) (store.FileRow, error) {
	t.Helper()
	ctx := context.Background()
	v, err := f.recvStore.GetVolumeByName(ctx, f.recvVol.Name)
	if err != nil {
		t.Fatalf("receiver GetVolumeByName: %v", err)
	}
	return f.recvStore.GetByPath(ctx, v.ID, rel)
}

// TestNodeSyncVanishedFileLeavesRestCommitted pins that one unreadable
// source costs only its own path: the run ends partial, names the path,
// and the receiver commits everything that did arrive.
func TestNodeSyncVanishedFileLeavesRestCommitted(t *testing.T) {
	f := setupNodeFixture(t)
	writeVolumeFile(t, f.initVol.Path, "kept.txt", "still here")
	writeVolumeFile(t, f.initVol.Path, "gone.txt", "deleted after indexing")
	f.indexInitiator(t)
	if err := os.Remove(filepath.Join(f.initVol.Path, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	rep, err := SyncNode(context.Background(), f.initStore, f.initVol, f.node, Options{})
	if err != nil {
		t.Fatalf("SyncNode: %v", err)
	}
	if rep.Status != store.RunStatusPartial {
		t.Fatalf("status = %q, want partial", rep.Status)
	}
	failed := rep.RcloneResult.FailedFiles
	if len(failed) != 1 || failed[0].Object != "gone.txt" || !strings.Contains(failed[0].Message, "no such file") {
		t.Fatalf("failed files = %+v, want gone.txt with the local read error", failed)
	}
	if _, err := receiverRow(t, f, "kept.txt"); err != nil {
		t.Errorf("kept.txt was not committed on the receiver: %v", err)
	}
	if _, err := receiverRow(t, f, "gone.txt"); err == nil {
		t.Error("gone.txt has a receiver row though its bytes never arrived")
	}
}

// TestNodeSyncSendsIndexedPrefixOfAppendedFile pins that a file appended
// to after indexing delivers the content the index recorded; the growth
// ships once the next index observes it.
func TestNodeSyncSendsIndexedPrefixOfAppendedFile(t *testing.T) {
	f := setupNodeFixture(t)
	writeVolumeFile(t, f.initVol.Path, "log.txt", "first line\n")
	f.indexInitiator(t)
	appendTo, err := os.OpenFile(filepath.Join(f.initVol.Path, "log.txt"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendTo.WriteString("second line\n"); err != nil {
		t.Fatal(err)
	}
	_ = appendTo.Close()

	rep, err := SyncNode(context.Background(), f.initStore, f.initVol, f.node, Options{})
	if err != nil {
		t.Fatalf("SyncNode: %v", err)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("status = %q, want success (failed files: %+v)", rep.Status, rep.RcloneResult.FailedFiles)
	}
	got, err := os.ReadFile(filepath.Join(f.recvVol.Path, "log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first line\n" {
		t.Fatalf("receiver holds %q, want the indexed content", got)
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

// TestPutContentGivesUpOnStalledPeer pins the no-progress bound on an
// upload, both while the peer has stopped reading the body and after it
// has read every byte but never replies.
func TestPutContentGivesUpOnStalledPeer(t *testing.T) {
	cases := []struct {
		name     string
		readBody bool
		body     io.Reader
		size     int64
	}{
		{name: "stops reading", body: io.LimitReader(zeroReader{}, 1<<30), size: 1 << 30},
		{name: "never replies", readBody: true, body: strings.NewReader("all of it"), size: 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if tc.readBody {
					_, _ = io.Copy(io.Discard, req.Body)
				}
				select {
				case <-release:
				case <-req.Context().Done():
				}
			}))
			t.Cleanup(ts.Close)
			t.Cleanup(func() { close(release) })
			endpoint, err := url.Parse(ts.URL)
			if err != nil {
				t.Fatal(err)
			}
			c := newNodeClient(&config.Node{Name: "stuck", Endpoint: endpoint, Token: "t"})
			c.stallTimeout = 100 * time.Millisecond

			start := time.Now()
			err = c.putContent(context.Background(), 1, strings.Repeat("0", 64), tc.body, tc.size)
			if !errors.Is(err, errUploadStalled) {
				t.Fatalf("putContent = %v, want errUploadStalled", err)
			}
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Fatalf("gave up after %s, want roughly the stall timeout", elapsed)
			}
		})
	}
}

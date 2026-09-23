package sync

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/blake3"

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
			err = c.putContent(context.Background(), 1, strings.Repeat("0", 64), tc.body, tc.size, c.stallTimeout)
			if !errors.Is(err, errUploadStalled) {
				t.Fatalf("putContent = %v, want errUploadStalled", err)
			}
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Fatalf("gave up after %s, want roughly the stall timeout", elapsed)
			}
		})
	}
}

// uploadDriver is a transfer-phase driver against a fake receiver whose
// reply to each upload is chosen by digest.
func uploadDriver(t *testing.T, statusFor func(digest string) int) *nodeSyncDriver {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		if code := statusFor(path.Base(req.URL.Path)); code != http.StatusOK {
			http.Error(w, "refused by test", code)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(ts.Close)
	endpoint, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	node := &config.Node{Name: "peer", Endpoint: endpoint, Token: "t"}
	return &nodeSyncDriver{
		ctx:           context.Background(),
		vol:           &config.Volume{Name: "pics", Path: t.TempDir()},
		node:          node,
		client:        newNodeClient(node),
		report:        &Report{},
		receiverRunID: 1,
		planned:       make(map[string]plannedContent),
	}
}

// plant writes rel and plans it for upload, returning its digest.
func (d *nodeSyncDriver) plant(t *testing.T, rel, body string) string {
	t.Helper()
	writeVolumeFile(t, d.vol.Path, rel, body)
	sum := blake3.Sum256([]byte(body))
	digest := hex.EncodeToString(sum[:])
	d.planned[rel] = plannedContent{hashHex: digest, size: int64(len(body))}
	return digest
}

func TestUploadPathsKeepsGoingPastFailedObjects(t *testing.T) {
	statuses := map[string]int{}
	d := uploadDriver(t, func(digest string) int { return statuses[digest] })
	statuses[d.plant(t, "a.txt", "alpha")] = http.StatusBadRequest
	statuses[d.plant(t, "b.txt", "beta")] = http.StatusOK
	statuses[d.plant(t, "c.txt", "gamma")] = http.StatusInternalServerError
	statuses[d.plant(t, "d.txt", "delta")] = http.StatusOK

	if err := d.uploadPaths([]string{"a.txt", "b.txt", "c.txt", "d.txt"}); err != nil {
		t.Fatalf("uploadPaths = %v, want the run to carry on", err)
	}
	if d.report.RcloneResult.Transferred != 2 {
		t.Errorf("transferred = %d, want 2", d.report.RcloneResult.Transferred)
	}
	if got := d.retryable([]string{"a.txt", "c.txt"}); len(got) != 2 {
		t.Errorf("retryable = %v, want both receiver-side failures", got)
	}
}

func TestUploadPathsGivesUpOnAnUnwellPeer(t *testing.T) {
	cases := []struct {
		name   string
		status int
		wantAt int
	}{
		{name: "session gone", status: http.StatusNotFound, wantAt: 1},
		{name: "receiver failing", status: http.StatusInternalServerError, wantAt: maxConsecutiveUploadFailures},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			d := uploadDriver(t, func(string) int { attempts++; return tc.status })
			var paths []string
			for i := range 5 {
				rel := string(rune('a'+i)) + ".txt"
				d.plant(t, rel, rel)
				paths = append(paths, rel)
			}
			if err := d.uploadPaths(paths); err == nil {
				t.Fatal("uploadPaths succeeded, want the transfer to end")
			}
			if attempts != tc.wantAt {
				t.Errorf("gave up after %d uploads, want %d", attempts, tc.wantAt)
			}
		})
	}
}

// TestUploadFallsBackToAnotherCopy pins that content deleted at one path
// since indexing still moves from another path holding it.
func TestUploadFallsBackToAnotherCopy(t *testing.T) {
	d := uploadDriver(t, func(string) int { return http.StatusOK })
	d.plant(t, "IMG_1 (1).jpg", "same bytes")
	d.plant(t, "IMG_1.jpg", "same bytes")
	if err := os.Remove(filepath.Join(d.vol.Path, "IMG_1 (1).jpg")); err != nil {
		t.Fatal(err)
	}
	if err := d.uploadPaths([]string{"IMG_1 (1).jpg", "IMG_1.jpg"}); err != nil {
		t.Fatal(err)
	}
	if d.report.RcloneResult.Transferred != 1 || len(d.uploadFailures) != 0 {
		t.Fatalf("transferred = %d, failures = %v, want one upload from the surviving copy",
			d.report.RcloneResult.Transferred, d.uploadFailures)
	}
}

// TestLocalFailuresAreNotRetried pins that a source that vanished or
// changed is left out of the retry round, and that a path which later
// uploads cleanly sheds its recorded failure.
func TestLocalFailuresAreNotRetried(t *testing.T) {
	fail := true
	d := uploadDriver(t, func(string) int {
		if fail {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	d.plant(t, "gone.txt", "vanished")
	d.plant(t, "flaky.txt", "retry me")
	if err := os.Remove(filepath.Join(d.vol.Path, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := d.uploadPaths([]string{"gone.txt", "flaky.txt"}); err != nil {
		t.Fatal(err)
	}
	retry := d.retryable([]string{"gone.txt", "flaky.txt"})
	if len(retry) != 1 || retry[0] != "flaky.txt" {
		t.Fatalf("retryable = %v, want only flaky.txt", retry)
	}
	fail = false
	if err := d.uploadPaths(retry); err != nil {
		t.Fatal(err)
	}
	if _, stale := d.uploadFailures["flaky.txt"]; stale {
		t.Error("flaky.txt kept its failure after uploading cleanly")
	}
}

func TestUploadSourceEndingEarlyIsDrift(t *testing.T) {
	dir := t.TempDir()
	writeVolumeFile(t, dir, "short.txt", "abc")
	f, err := os.Open(filepath.Join(dir, "short.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	_, err = io.ReadAll(&uploadSource{f: f, rel: "short.txt", remaining: 10})
	if !errors.Is(err, errContentDrift) {
		t.Fatalf("read = %v, want errContentDrift", err)
	}
}

// TestAbortCloseIsBounded pins that an aborting run's /close gives up on a
// receiver that never answers, so the run still ends.
func TestAbortCloseIsBounded(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		<-release
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(release) })
	endpoint, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	node := &config.Node{Name: "hung", Endpoint: endpoint, Token: "t"}
	d := &nodeSyncDriver{ctx: context.Background(), node: node, client: newNodeClient(node), report: &Report{}, receiverRunID: 1}
	d.client.stallTimeout = 100 * time.Millisecond

	start := time.Now()
	_ = d.abortWithError("transfer", errUploadStalled)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("abort took %s, want roughly the stall timeout", elapsed)
	}
}

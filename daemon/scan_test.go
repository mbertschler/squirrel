package daemon

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// newScanServer returns a Server with one volume rooted under a fresh
// temp directory plus the store backing it. Tests drive the audit
// loop one tick at a time via runScanTick, keeping wall-clock
// dependencies out of the assertions. ScanStrategy defaults to
// shallow; tests that need deep mutate srv.cfg.ScanStrategy directly.
func newScanServer(t *testing.T) (*Server, *store.Store, *config.Volume) {
	t.Helper()
	volRoot := t.TempDir()
	vol := &config.Volume{Name: "pics", Path: volRoot}
	dbPath := filepath.Join(t.TempDir(), "index.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	srv, err := New(Config{
		Listen:       "127.0.0.1:0",
		Token:        "test-token",
		Version:      "test",
		Volumes:      map[string]*config.Volume{vol.Name: vol},
		ScanInterval: 0, // tests drive runScanTick directly
		ScanStrategy: ScanStrategyShallow,
	}, s)
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return srv, s, vol
}

// TestScanTickRecordsAuditRun is the acceptance scenario from the
// issue: a file changes on disk between two audit ticks, the next
// tick records an `audit` run, supersedes the prior row, and inserts
// a fresh present row at the new digest.
func TestScanTickRecordsAuditRun(t *testing.T) {
	srv, s, vol := newScanServer(t)
	ctx := context.Background()

	target := filepath.Join(vol.Path, "doc.md")
	if err := os.WriteFile(target, []byte("version-X"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First tick establishes the baseline; no prior rows exist so
	// every file is recorded as 'added'.
	var buf bytes.Buffer
	srv.runScanTick(ctx, &buf)
	v, err := s.GetVolumeByName(ctx, vol.Name)
	if err != nil {
		t.Fatalf("GetVolumeByName: %v", err)
	}
	row, err := s.GetByPath(ctx, v.ID, "doc.md")
	if err != nil {
		t.Fatalf("GetByPath after tick 1: %v", err)
	}
	if row.Status != store.StatusPresent {
		t.Fatalf("tick 1 row status = %q, want present", row.Status)
	}
	firstAuditID := row.FirstSeenRunID

	// Modify the file out-of-band to simulate drift.
	if err := os.WriteFile(target, []byte("version-Y-drift"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Force a different mtime so the shallow shortcut can't hide the
	// change (in case the rewrite happened in the same nanosecond on
	// fast filesystems).
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(target, future, future); err != nil {
		t.Fatal(err)
	}

	buf.Reset()
	srv.runScanTick(ctx, &buf)

	hist, err := s.ListHistoryByPath(ctx, v.ID, "doc.md")
	if err != nil {
		t.Fatalf("ListHistoryByPath: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("history has %d rows, want 2 (prior superseded + new present): %+v", len(hist), hist)
	}
	var sawPresent, sawSuperseded bool
	for _, r := range hist {
		switch r.Status {
		case store.StatusPresent:
			sawPresent = true
		case store.StatusSuperseded:
			sawSuperseded = true
		}
	}
	if !sawPresent || !sawSuperseded {
		t.Fatalf("history missing one of the expected states: %+v", hist)
	}

	// The second tick wrote an 'audit'-kind row.
	runs, err := s.ListRuns(ctx, store.ListRunsOpts{Descending: true})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	var auditID int64
	for _, r := range runs {
		if r.Kind == store.RunKindAudit && r.ID != firstAuditID {
			auditID = r.ID
			break
		}
	}
	if auditID == 0 {
		t.Fatalf("no new audit run after tick 2: %+v", runs)
	}
	modified, err := s.CountModifiedFilesByRun(ctx, auditID)
	if err != nil {
		t.Fatalf("CountModifiedFilesByRun: %v", err)
	}
	if modified != 1 {
		t.Fatalf("modified by audit run %d = %d, want 1", auditID, modified)
	}
}

// TestScanTickSkipsLockedVolume proves the audit tick respects the
// per-volume lock the /v1/sync/* handlers hold during a session. A
// tick arriving while a sync is in flight logs a skip and leaves
// state alone — no audit run row materialises for the locked volume.
func TestScanTickSkipsLockedVolume(t *testing.T) {
	srv, s, vol := newScanServer(t)
	ctx := context.Background()

	// First, plant a volume row + one file so the scan has something
	// to skip rather than create from scratch.
	if err := os.WriteFile(filepath.Join(vol.Path, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	srv.runScanTick(ctx, &buf)
	v, _ := s.GetVolumeByName(ctx, vol.Name)

	// Simulate an in-flight sync by acquiring the per-volume lock.
	if !srv.router.acquireVolumeLock(v.ID) {
		t.Fatalf("acquire volume lock failed; want success on first try")
	}

	priorRunCount := func() int {
		runs, _ := s.ListRuns(ctx, store.ListRunsOpts{})
		return len(runs)
	}()

	buf.Reset()
	srv.runScanTick(ctx, &buf)

	// No new run row written.
	if got := func() int {
		runs, _ := s.ListRuns(ctx, store.ListRunsOpts{})
		return len(runs)
	}(); got != priorRunCount {
		t.Fatalf("run count = %d, want %d (no new audit during locked sync)", got, priorRunCount)
	}
	if !strings.Contains(buf.String(), "skipped") {
		t.Fatalf("scan output = %q, want a 'skipped' note", buf.String())
	}

	// Release the lock so subsequent ticks behave normally.
	srv.router.releaseVolumeLock(v.ID)
}

// TestScanLoopRespectsContextCancellation starts the loop on a fast
// interval, then cancels the context and confirms the loop returns
// cleanly. Goroutine leak detection in the test harness catches a
// regression where ctx.Done() isn't honoured.
func TestScanLoopRespectsContextCancellation(t *testing.T) {
	srv, _, _ := newScanServer(t)
	srv.cfg.ScanInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var buf bytes.Buffer
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv.runScanLoop(ctx, &buf)
	}()

	// Give the ticker time to fire at least once so the body of the
	// loop is exercised before cancellation.
	time.Sleep(50 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("scan loop did not return within 2s of context cancellation")
	}
}

// TestServeIntegratesScanLoop ties the scan loop into the daemon's
// Serve method: with ScanInterval set, the loop fires while the HTTP
// server is up, and a context cancel cleans both up together. The
// listener exists only to satisfy Serve's signature; we don't issue
// HTTP requests.
func TestServeIntegratesScanLoop(t *testing.T) {
	srv, _, vol := newScanServer(t)
	srv.cfg.ScanInterval = 25 * time.Millisecond
	if err := os.WriteFile(filepath.Join(vol.Path, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	// Wait long enough for at least one scan tick to write a run row.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := srv.store.ListRuns(ctx, store.ListRunsOpts{})
		if len(runs) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	runs, _ := srv.store.ListRuns(ctx, store.ListRunsOpts{})
	if len(runs) == 0 {
		t.Fatalf("scan loop did not record any runs while Serve was up")
	}
	for _, r := range runs {
		if r.Kind != store.RunKindAudit {
			t.Fatalf("expected only audit runs, got %q", r.Kind)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Serve did not return within 3s of cancellation")
	}
}

// TestNewRejectsBadScanConfig validates the daemon.Config guards on
// the new fields. Negative intervals and unknown strategies must be
// caught at New() time rather than surfacing later as silent
// no-scans.
func TestNewRejectsBadScanConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			"negative interval",
			Config{Listen: ":0", Token: "t", Version: "v", ScanInterval: -time.Second},
			"ScanInterval",
		},
		{
			"unknown strategy",
			Config{Listen: ":0", Token: "t", Version: "v", ScanStrategy: "fancy"},
			"ScanStrategy",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(c.cfg, openTestStore(t))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

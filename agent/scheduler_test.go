package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

func newLocalListener() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}

// schedulerFixture bundles the scheduler-under-test plus the
// dependencies a test typically wants to poke at: the store (to plant
// runs rows directly), the configured volumes, a log buffer for
// assertion, and a controllable clock.
type schedulerFixture struct {
	t       *testing.T
	srv     *Server
	store   *store.Store
	logBuf  *bytes.Buffer
	clock   *fakeClock
	syncLog *fakeSyncRunner
}

// fakeClock returns whatever time has been set on it. Tests advance
// time by calling Add — no wall-clock sleeps anywhere.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeSyncRunner records every (volume, destination) the scheduler
// invokes it on. The default behaviour is to insert a 'success' sync
// run row (so the next syncDue check moves the watermark) and return
// a successful report. Tests can swap the inner func for failure
// scenarios.
type fakeSyncRunner struct {
	mu     sync.Mutex
	calls  []syncCall
	runFn  func(ctx context.Context, vol *config.Volume, destName string, callIdx int) SyncRunReport
	nextID int64
}

type syncCall struct {
	Volume      string
	Destination string
}

func newFakeSyncRunner(s *store.Store) *fakeSyncRunner {
	f := &fakeSyncRunner{}
	f.runFn = func(ctx context.Context, vol *config.Volume, destName string, callIdx int) SyncRunReport {
		// Default: insert a sync run + finish it 'success'. This is
		// what makes the scheduler's "last sync" cadence calculation
		// progress between ticks.
		v, err := s.GetVolumeByName(ctx, vol.Name)
		if err != nil {
			return SyncRunReport{Err: err}
		}
		id, blocker, err := s.BeginSyncRunIfClear(ctx, store.SyncRunSpec{
			VolumeID:    v.ID,
			Destination: destName,
		})
		if err != nil {
			return SyncRunReport{Err: err}
		}
		if blocker != nil {
			return SyncRunReport{Err: fmt.Errorf("already running")}
		}
		if err := s.FinishRun(ctx, id, store.RunStatusSuccess, "", 0); err != nil {
			return SyncRunReport{RunID: id, Err: err}
		}
		return SyncRunReport{RunID: id, Status: store.RunStatusSuccess}
	}
	return f
}

func (f *fakeSyncRunner) Runner() SyncRunner {
	return func(ctx context.Context, vol *config.Volume, destName string) SyncRunReport {
		f.mu.Lock()
		idx := len(f.calls)
		f.calls = append(f.calls, syncCall{Volume: vol.Name, Destination: destName})
		fn := f.runFn
		f.mu.Unlock()
		return fn(ctx, vol, destName, idx)
	}
}

func (f *fakeSyncRunner) Calls() []syncCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]syncCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// newSchedulerFixture builds an agent server with one volume backed
// by a fresh temp directory and wires the scheduler with a fake clock
// + fake sync runner. The scheduler's tick method drives behaviour
// directly; the goroutine-driven run() loop is only used in the
// integration test below.
func newSchedulerFixture(t *testing.T, volumeCfg *config.Volume) *schedulerFixture {
	t.Helper()
	if volumeCfg.Path == "" {
		volumeCfg.Path = t.TempDir()
	}
	dbPath := filepath.Join(t.TempDir(), "index.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	syncRunner := newFakeSyncRunner(s)

	srv, err := New(Config{
		Listen:     "127.0.0.1:0",
		Token:      "tok",
		Version:    "test",
		Volumes:    map[string]*config.Volume{volumeCfg.Name: volumeCfg},
		Logger:     logger,
		SyncRunner: syncRunner.Runner(),
	}, s)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	// Anchor the fake clock to wall-now so the timestamps store.NowNs()
	// stamps into the runs table line up with the elapsedSince math
	// the scheduler does against this clock. Tests advance via
	// fakeClock.Add; production reads time.Now.
	clock := newFakeClock(time.Now())
	return &schedulerFixture{
		t:       t,
		srv:     srv,
		store:   s,
		logBuf:  logBuf,
		clock:   clock,
		syncLog: syncRunner,
	}
}

// scheduler builds a scheduler against the fixture's server, with the
// fake clock wired as the time source. The lock holder is the agent's
// own router so we exercise the same coordination path production
// uses.
func (f *schedulerFixture) scheduler() *scheduler {
	return &scheduler{
		store:     f.srv.store,
		volumes:   f.srv.cfg.Volumes,
		syncRun:   f.srv.cfg.SyncRunner,
		logger:    f.srv.cfg.Logger,
		locks:     f.srv.router,
		tickEvery: time.Second,
		now:       f.clock.Now,
	}
}

func (f *schedulerFixture) writeFile(rel, body string) {
	f.t.Helper()
	for _, vol := range f.srv.cfg.Volumes {
		p := filepath.Join(vol.Path, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			f.t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			f.t.Fatalf("write %s: %v", p, err)
		}
		return
	}
}

func (f *schedulerFixture) logLines() []string {
	return strings.Split(strings.TrimRight(f.logBuf.String(), "\n"), "\n")
}

// containsLogLine reports whether the captured slog output has a line
// matching all of fields. Used by the assertions below to avoid
// brittle full-line string equality.
func (f *schedulerFixture) containsLogLine(fields ...string) bool {
	for _, line := range f.logLines() {
		ok := true
		for _, want := range fields {
			if !strings.Contains(line, want) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// TestSchedulerIgnoresVolumesWithoutCadence is the "no-op" baseline:
// volumes without sync_every or index_every never trigger any
// scheduler activity, no matter how many ticks pass.
func TestSchedulerIgnoresVolumesWithoutCadence(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{Name: "idle"})
	f.writeFile("a.txt", "hello")
	sch := f.scheduler()

	for i := 0; i < 3; i++ {
		f.clock.Add(time.Hour)
		sch.tick(context.Background())
	}
	if got := len(f.syncLog.Calls()); got != 0 {
		t.Fatalf("sync invoked %d times; want 0", got)
	}
	runs, err := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("scheduler created %d run rows; want 0: %+v", len(runs), runs)
	}
}

// TestSchedulerKicksStandaloneIndex covers index_every with no
// sync_every: tick fires a kind='index' run, the run lands as
// 'success', and a follow-up tick within the cadence window does not
// re-fire.
func TestSchedulerKicksStandaloneIndex(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{
		Name:       "pics",
		IndexEvery: 10 * time.Minute,
	})
	f.writeFile("a.txt", "hello")
	sch := f.scheduler()
	ctx := context.Background()

	// First tick: no prior runs, so index is due.
	sch.tick(ctx)
	runs, err := f.store.ListRuns(ctx, store.ListRunsOpts{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Kind != store.RunKindIndex {
		t.Fatalf("after first tick: runs=%+v; want one kind='index'", runs)
	}
	if runs[0].Status != store.RunStatusSuccess {
		t.Fatalf("first tick run status=%q; want %q", runs[0].Status, store.RunStatusSuccess)
	}
	if !f.containsLogLine("scheduler.kicked", "kind=index", "volume=pics", "reason=standalone") {
		t.Fatalf("missing kicked log line:\n%s", f.logBuf.String())
	}
	if !f.containsLogLine("scheduler.finished", "kind=index", "status=success") {
		t.Fatalf("missing finished log line:\n%s", f.logBuf.String())
	}

	// Advance under the cadence; second tick must not re-kick.
	f.clock.Add(5 * time.Minute)
	sch.tick(ctx)
	runs2, _ := f.store.ListRuns(ctx, store.ListRunsOpts{})
	if len(runs2) != 1 {
		t.Fatalf("after under-cadence tick: runs=%d; want 1", len(runs2))
	}

	// Advance past the cadence; third tick re-kicks.
	f.clock.Add(10 * time.Minute)
	sch.tick(ctx)
	runs3, _ := f.store.ListRuns(ctx, store.ListRunsOpts{})
	if len(runs3) != 2 {
		t.Fatalf("after past-cadence tick: runs=%d; want 2", len(runs3))
	}
}

// TestSchedulerSyncRunsPreSyncIndexFirst pins the invariant that a
// scheduled sync always indexes immediately before pushing. The fake
// sync runner records every call; the runs table records every kind.
// On the first tick we expect: one kind='index' run, then one
// kind='sync' run, both against the same volume.
func TestSchedulerSyncRunsPreSyncIndexFirst(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{
		Name:      "pics",
		SyncTo:    []string{"backup"},
		SyncEvery: time.Hour,
	})
	f.writeFile("a.txt", "hello")
	sch := f.scheduler()
	ctx := context.Background()

	sch.tick(ctx)

	runs, err := f.store.ListRuns(ctx, store.ListRunsOpts{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("after tick: %d runs; want 2 (index + sync): %+v", len(runs), runs)
	}
	// Index must be id=1, sync id=2 — the index runs first.
	if runs[0].Kind != store.RunKindIndex || runs[1].Kind != store.RunKindSync {
		t.Fatalf("run order wrong; got [%s,%s], want [index,sync]: %+v",
			runs[0].Kind, runs[1].Kind, runs)
	}
	calls := f.syncLog.Calls()
	if len(calls) != 1 || calls[0].Destination != "backup" {
		t.Fatalf("sync calls = %+v; want one for backup", calls)
	}
}

// TestSchedulerSkipsSyncWhenPreSyncIndexFails proves the issue's
// failure-mode rule: "If index fails, sync is skipped this tick."
// We make the index fail by removing the volume directory between
// resolveVolume and Index — the deferred FinishRun marks the run
// failed, and the scheduler must not invoke the sync runner.
func TestSchedulerSkipsSyncWhenPreSyncIndexFails(t *testing.T) {
	volRoot := t.TempDir()
	f := newSchedulerFixture(t, &config.Volume{
		Name:      "pics",
		Path:      volRoot,
		SyncTo:    []string{"backup"},
		SyncEvery: time.Hour,
	})
	// Delete the volume dir so index.Index fails fast at the
	// filepath.WalkDir root stat.
	if err := os.RemoveAll(volRoot); err != nil {
		t.Fatal(err)
	}

	sch := f.scheduler()
	sch.tick(context.Background())

	if got := len(f.syncLog.Calls()); got != 0 {
		t.Fatalf("sync invoked %d times after failed pre-sync index; want 0", got)
	}
}

// TestSchedulerSkipsWhenInFlightIndexRun plants a stale 'running'
// index row directly in the DB (simulating a crashed CLI invocation
// that left behind a row, or an in-flight `squirrel index`) and
// asserts the scheduler logs scheduler.skipped instead of kicking a
// duplicate.
func TestSchedulerSkipsWhenInFlightIndexRun(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{
		Name:       "pics",
		IndexEvery: 10 * time.Minute,
	})
	f.writeFile("a.txt", "x")

	// Resolve the volume up front (so the runs.volume_id FK has a row
	// to point at) then plant a 'running' index row.
	v, err := f.store.CreateVolume(context.Background(), "pics", f.srv.cfg.Volumes["pics"].Path)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if _, err := f.store.BeginRun(context.Background(), store.RunKindIndex, v.ID, ""); err != nil {
		t.Fatalf("BeginRun: %v", err)
	}

	sch := f.scheduler()
	sch.tick(context.Background())

	if !f.containsLogLine("scheduler.skipped", "kind=index", "in-flight") {
		t.Fatalf("missing in-flight skip log:\n%s", f.logBuf.String())
	}
	// Exactly one runs row should exist — the planted 'running' one.
	runs, _ := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	if len(runs) != 1 || runs[0].Status != store.RunStatusRunning {
		t.Fatalf("runs=%+v; want the single planted 'running' row", runs)
	}
}

// TestSchedulerSkipsWhenInFlightSyncRun plants a stale running sync
// row and asserts the scheduler skips the sync (but, per the
// invariant, the pre-sync index still ran in this tick). We use a
// fake sync runner so we can verify it never gets invoked.
func TestSchedulerSkipsWhenInFlightSyncRun(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{
		Name:      "pics",
		SyncTo:    []string{"backup"},
		SyncEvery: time.Hour,
	})
	f.writeFile("a.txt", "x")

	v, err := f.store.CreateVolume(context.Background(), "pics", f.srv.cfg.Volumes["pics"].Path)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if _, _, err := f.store.BeginSyncRunIfClear(context.Background(), store.SyncRunSpec{
		VolumeID:    v.ID,
		Destination: "backup",
	}); err != nil {
		t.Fatalf("BeginSyncRunIfClear: %v", err)
	}

	sch := f.scheduler()
	sch.tick(context.Background())

	if got := len(f.syncLog.Calls()); got != 0 {
		t.Fatalf("sync invoked %d times despite running sync row; want 0", got)
	}
	if !f.containsLogLine("scheduler.skipped", "kind=sync", "in-flight") {
		t.Fatalf("missing in-flight sync skip log:\n%s", f.logBuf.String())
	}
	// Pre-sync index still ran — the invariant says sync always
	// indexes first, and the only thing skipped here is the sync
	// itself.
	runs, _ := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	var sawIndex, sawSync bool
	for _, r := range runs {
		switch r.Kind {
		case store.RunKindIndex:
			sawIndex = true
		case store.RunKindSync:
			sawSync = true
		}
	}
	if !sawIndex {
		t.Fatalf("expected a pre-sync index row even when sync is gated: %+v", runs)
	}
	if !sawSync {
		// The planted 'running' sync row counts as a sync; just sanity.
		t.Fatalf("expected the planted running sync row to remain: %+v", runs)
	}
}

// TestSchedulerSkipsWhenVolumeLockHeld pins the coordination with the
// peer-sync surface: an incoming /v1/sync session holds the per-volume
// router lock for the duration of the session; a scheduler tick during
// that window must skip rather than race against the receiver-side
// upserts.
func TestSchedulerSkipsWhenVolumeLockHeld(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{
		Name:       "pics",
		IndexEvery: 10 * time.Minute,
	})
	f.writeFile("a.txt", "x")

	v, err := f.store.CreateVolume(context.Background(), "pics", f.srv.cfg.Volumes["pics"].Path)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if !f.srv.router.acquireVolumeLock(v.ID) {
		t.Fatalf("router lock already held; want unheld at fixture start")
	}
	defer f.srv.router.releaseVolumeLock(v.ID)

	sch := f.scheduler()
	sch.tick(context.Background())

	if !f.containsLogLine("scheduler.skipped", "kind=index", "volume busy") {
		t.Fatalf("missing volume-busy skip log:\n%s", f.logBuf.String())
	}
	runs, _ := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	if len(runs) != 0 {
		t.Fatalf("scheduler created %d runs while lock was held; want 0", len(runs))
	}
}

// TestSchedulerCadenceUsesLastFinished walks through the multi-tick
// scenario: a sync completes, the next tick is within the cadence
// window and skips, and a later tick past the cadence re-fires. This
// is the integration of syncDue + LatestFinishedRun against real DB
// state across ticks.
func TestSchedulerCadenceUsesLastFinished(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{
		Name:      "pics",
		SyncTo:    []string{"backup"},
		SyncEvery: time.Hour,
	})
	f.writeFile("a.txt", "x")

	sch := f.scheduler()
	ctx := context.Background()

	// Tick 1: nothing prior → fires.
	sch.tick(ctx)
	if got := len(f.syncLog.Calls()); got != 1 {
		t.Fatalf("after tick 1: sync calls = %d; want 1", got)
	}

	// Advance 30min, still under cadence → does not fire.
	f.clock.Add(30 * time.Minute)
	sch.tick(ctx)
	if got := len(f.syncLog.Calls()); got != 1 {
		t.Fatalf("after tick 2 (under cadence): sync calls = %d; want 1", got)
	}

	// Advance another hour, past cadence → fires again.
	f.clock.Add(time.Hour)
	sch.tick(ctx)
	if got := len(f.syncLog.Calls()); got != 2 {
		t.Fatalf("after tick 3 (over cadence): sync calls = %d; want 2", got)
	}
}

// TestSchedulerFailedSyncStillConsumesCadence guards the failure
// policy ("Failed runs aren't specially retried — the next tick
// re-evaluates"). After a failed sync, the next within-cadence tick
// must still skip — failed runs count as last_finished.
func TestSchedulerFailedSyncStillConsumesCadence(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{
		Name:      "pics",
		SyncTo:    []string{"backup"},
		SyncEvery: time.Hour,
	})
	f.writeFile("a.txt", "x")

	// First call fails; subsequent calls also fail (but won't run).
	var calls atomic.Int32
	f.syncLog.runFn = func(ctx context.Context, vol *config.Volume, destName string, callIdx int) SyncRunReport {
		calls.Add(1)
		v, _ := f.store.GetVolumeByName(ctx, vol.Name)
		id, _, _ := f.store.BeginSyncRunIfClear(ctx, store.SyncRunSpec{VolumeID: v.ID, Destination: destName})
		_ = f.store.FinishRun(ctx, id, store.RunStatusFailed, "boom", 0)
		return SyncRunReport{RunID: id, Status: store.RunStatusFailed, Err: errors.New("boom")}
	}

	sch := f.scheduler()
	ctx := context.Background()

	sch.tick(ctx)
	if calls.Load() != 1 {
		t.Fatalf("tick 1 sync calls = %d; want 1", calls.Load())
	}

	// Within cadence → no re-fire even though the prior run failed.
	f.clock.Add(10 * time.Minute)
	sch.tick(ctx)
	if calls.Load() != 1 {
		t.Fatalf("under-cadence tick after failure called sync %d times; want 1 (no special retry)",
			calls.Load())
	}

	// Past cadence → re-fires (regardless of prior failure).
	f.clock.Add(time.Hour)
	sch.tick(ctx)
	if calls.Load() != 2 {
		t.Fatalf("past-cadence tick after failure called sync %d times; want 2", calls.Load())
	}
}

// TestSchedulerBothCadencesShareIndexRun verifies the "both knobs set"
// branch: when sync_every fires on a tick, the pre-sync index it runs
// also satisfies the index_every cadence (same kind='index' rows),
// so the standalone-index path doesn't double-fire.
func TestSchedulerBothCadencesShareIndexRun(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{
		Name:       "pics",
		SyncTo:     []string{"backup"},
		SyncEvery:  time.Hour,
		IndexEvery: 15 * time.Minute,
	})
	f.writeFile("a.txt", "x")
	sch := f.scheduler()
	ctx := context.Background()

	sch.tick(ctx)

	runs, _ := f.store.ListRuns(ctx, store.ListRunsOpts{})
	var indexCount int
	for _, r := range runs {
		if r.Kind == store.RunKindIndex {
			indexCount++
		}
	}
	if indexCount != 1 {
		t.Fatalf("after tick: index runs = %d; want exactly 1 (pre-sync satisfies index_every)", indexCount)
	}
}

// TestSchedulerStandaloneIndexAfterSyncCadenceWaits is the converse:
// once sync fires (kind='index' written as the pre-sync), the
// standalone-index cadence sees a fresh index and skips until
// index_every elapses since that.
func TestSchedulerStandaloneIndexAfterSyncCadenceWaits(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{
		Name:       "pics",
		SyncTo:     []string{"backup"},
		SyncEvery:  time.Hour,
		IndexEvery: 15 * time.Minute,
	})
	f.writeFile("a.txt", "x")
	sch := f.scheduler()
	ctx := context.Background()

	sch.tick(ctx) // fires sync + pre-sync index.

	// 10min later → under index_every from the pre-sync index, no
	// standalone fires.
	f.clock.Add(10 * time.Minute)
	sch.tick(ctx)
	runs, _ := f.store.ListRuns(ctx, store.ListRunsOpts{})
	var indexCount int
	for _, r := range runs {
		if r.Kind == store.RunKindIndex {
			indexCount++
		}
	}
	if indexCount != 1 {
		t.Fatalf("under-cadence tick: index runs = %d; want 1", indexCount)
	}

	// 20min after the pre-sync index → standalone fires (sync is not
	// due yet because sync_every is 1h).
	f.clock.Add(10 * time.Minute) // total 20min since pre-sync.
	sch.tick(ctx)
	runs, _ = f.store.ListRuns(ctx, store.ListRunsOpts{})
	indexCount = 0
	for _, r := range runs {
		if r.Kind == store.RunKindIndex {
			indexCount++
		}
	}
	if indexCount != 2 {
		t.Fatalf("past-cadence tick: index runs = %d; want 2", indexCount)
	}
}

// TestServeIntegratesSchedulerLoop ties the scheduler into the agent's
// Serve method: with a cadence-bearing volume configured, the loop
// fires while the HTTP server is up, and a context cancel cleans both
// up together. Parallel to TestServeIntegratesScanLoop for the audit
// scanner.
func TestServeIntegratesSchedulerLoop(t *testing.T) {
	volRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(volRoot, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol := &config.Volume{
		Name:       "pics",
		Path:       volRoot,
		IndexEvery: 10 * time.Millisecond,
	}
	dbPath := filepath.Join(t.TempDir(), "index.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	srv, err := New(Config{
		Listen:        "127.0.0.1:0",
		Token:         "tok",
		Version:       "test",
		Volumes:       map[string]*config.Volume{vol.Name: vol},
		SchedulerTick: 25 * time.Millisecond,
	}, s)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	ln, err := newLocalListener()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := s.ListRuns(ctx, store.ListRunsOpts{})
		if len(runs) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	runs, _ := s.ListRuns(ctx, store.ListRunsOpts{})
	if len(runs) == 0 {
		t.Fatalf("scheduler loop did not record any runs while Serve was up")
	}
	for _, r := range runs {
		if r.Kind != store.RunKindIndex {
			t.Fatalf("expected kind='index' from scheduler, got %q", r.Kind)
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

// TestSchedulerRunRespectsContextCancellation starts the goroutine
// and confirms it exits on context cancellation. The fake clock is
// not relevant here — we use the real ticker, but with a small
// interval so cancellation has something to interrupt.
func TestSchedulerRunRespectsContextCancellation(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{
		Name:       "pics",
		IndexEvery: time.Hour, // far enough away that no tick body actually runs
	})
	sch := f.scheduler()
	sch.tickEvery = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sch.run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("scheduler.run did not return within 2s of cancellation")
	}
}

// TestSchedulerAnyScheduledVolume covers the start-up gate. A config
// with at least one cadence-bearing volume returns true; a config
// with only manual-only volumes returns false.
func TestSchedulerAnyScheduledVolume(t *testing.T) {
	cases := []struct {
		name string
		vol  *config.Volume
		want bool
	}{
		{"no cadence", &config.Volume{Name: "v"}, false},
		{"index-only", &config.Volume{Name: "v", IndexEvery: time.Minute}, true},
		{"sync-only", &config.Volume{Name: "v", SyncEvery: time.Minute}, true},
		{"both", &config.Volume{Name: "v", SyncEvery: time.Hour, IndexEvery: time.Minute}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newSchedulerFixture(t, c.vol)
			if got := f.scheduler().anyScheduledVolume(); got != c.want {
				t.Fatalf("anyScheduledVolume = %v; want %v", got, c.want)
			}
		})
	}
}

// TestSchedulerDropsSyncWhenRunnerNil covers the index-only schedule
// on a host without rclone: SyncRunner is nil, so any sync-triggering
// path logs a skip and creates no runs. The pre-sync index still runs
// (we don't know upfront that sync will be a no-op), but the sync
// itself doesn't materialise.
func TestSchedulerDropsSyncWhenRunnerNil(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{
		Name:      "pics",
		SyncTo:    []string{"backup"},
		SyncEvery: time.Hour,
	})
	f.writeFile("a.txt", "x")
	sch := f.scheduler()
	sch.syncRun = nil

	sch.tick(context.Background())

	if !f.containsLogLine("scheduler.skipped", "kind=sync", "sync runner not configured") {
		t.Fatalf("missing nil-runner skip log:\n%s", f.logBuf.String())
	}
	runs, _ := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	for _, r := range runs {
		if r.Kind == store.RunKindSync {
			t.Fatalf("sync run created when SyncRunner is nil: %+v", r)
		}
	}
}

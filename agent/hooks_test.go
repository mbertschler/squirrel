package agent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// newHookFixture opens a store with one volume row and returns a hookRunner
// wired to a captured logger. Kept local to the hook tests so they don't
// lean on the scheduler fixture's clock/sync machinery they don't need.
func newHookFixture(t *testing.T) (*hookRunner, *store.Store, *bytes.Buffer, store.Volume) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	vol, err := s.CreateVolume(context.Background(), "v", t.TempDir())
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return newHookRunner(s, logger), s, buf, vol
}

func hookVol(cmd []string) *config.Volume {
	return &config.Volume{
		Name: "v",
		Path: "/tmp/v",
		Hook: &config.VolumeHook{Command: cmd, Timeout: 5 * time.Second},
	}
}

func TestHookRunnerFireSuccess(t *testing.T) {
	h, s, _, vol := newHookFixture(t)
	ctx := context.Background()
	// A change hook's triggering run must exist — the column is an FK.
	runID, err := s.BeginIndexRun(ctx, store.RunKindIndex, vol.ID, true)
	if err != nil {
		t.Fatalf("BeginIndexRun: %v", err)
	}

	h.fire(ctx, hookVol([]string{"sh", "-c", "exit 0"}), vol.ID, store.HookTriggerChange, runID, true)
	h.wait()

	runs, err := s.ListHookRuns(ctx, store.HookRunListOpts{})
	if err != nil {
		t.Fatalf("ListHookRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("hook runs = %d, want 1", len(runs))
	}
	r := runs[0]
	if r.Status != store.HookStatusSuccess {
		t.Fatalf("status = %q, want success", r.Status)
	}
	if !r.ExitCode.Valid || r.ExitCode.Int64 != 0 {
		t.Fatalf("ExitCode = %v, want 0", r.ExitCode)
	}
	if r.Trigger != store.HookTriggerChange {
		t.Fatalf("trigger = %q, want change", r.Trigger)
	}
	if !r.TriggeringRunID.Valid || r.TriggeringRunID.Int64 != runID {
		t.Fatalf("TriggeringRunID = %v, want %d", r.TriggeringRunID, runID)
	}
	if !r.Changed {
		t.Fatalf("Changed = false, want true")
	}
}

func TestHookRunnerFireFailureRecorded(t *testing.T) {
	h, s, _, vol := newHookFixture(t)
	ctx := context.Background()
	runID, err := s.BeginIndexRun(ctx, store.RunKindIndex, vol.ID, true)
	if err != nil {
		t.Fatalf("BeginIndexRun: %v", err)
	}

	h.fire(ctx, hookVol([]string{"sh", "-c", "echo boom 1>&2; exit 3"}), vol.ID, store.HookTriggerChange, runID, true)
	h.wait()

	runs, _ := s.ListHookRuns(ctx, store.HookRunListOpts{})
	if len(runs) != 1 {
		t.Fatalf("hook runs = %d, want 1", len(runs))
	}
	r := runs[0]
	if r.Status != store.HookStatusFailed {
		t.Fatalf("status = %q, want failed", r.Status)
	}
	if !r.ExitCode.Valid || r.ExitCode.Int64 != 3 {
		t.Fatalf("ExitCode = %v, want 3", r.ExitCode)
	}
	if !r.Error.Valid || !strings.Contains(r.Error.String, "boom") {
		t.Fatalf("Error = %v, want it to carry the stderr tail", r.Error)
	}
}

func TestHookRunnerNoHookConfigured(t *testing.T) {
	h, s, _, vol := newHookFixture(t)
	ctx := context.Background()
	// A volume with no [hook] block must never record a hook run.
	h.fire(ctx, &config.Volume{Name: "v", Path: "/tmp/v"}, vol.ID, store.HookTriggerChange, 1, true)
	h.wait()
	runs, _ := s.ListHookRuns(ctx, store.HookRunListOpts{})
	if len(runs) != 0 {
		t.Fatalf("hook runs = %d, want 0 when no hook is configured", len(runs))
	}
}

func TestHookRunnerDontStack(t *testing.T) {
	h, s, buf, vol := newHookFixture(t)
	ctx := context.Background()

	// Simulate an in-flight invocation by holding the guard, then fire:
	// the new invocation must be skipped, not stacked.
	if !h.tryStart(vol.ID) {
		t.Fatalf("tryStart returned false on a fresh volume")
	}
	h.fire(ctx, hookVol([]string{"sh", "-c", "exit 0"}), vol.ID, store.HookTriggerChange, 1, true)
	h.wait()
	h.done(vol.ID)

	runs, _ := s.ListHookRuns(ctx, store.HookRunListOpts{})
	if len(runs) != 0 {
		t.Fatalf("hook runs = %d, want 0 — a stacked invocation must be skipped", len(runs))
	}
	if !strings.Contains(buf.String(), "hook.skipped") {
		t.Fatalf("expected a hook.skipped log line, got:\n%s", buf.String())
	}
}

func TestHookGuardReleases(t *testing.T) {
	h, _, _, vol := newHookFixture(t)
	if !h.tryStart(vol.ID) {
		t.Fatalf("first tryStart = false")
	}
	if h.tryStart(vol.ID) {
		t.Fatalf("second tryStart = true, want false while in flight")
	}
	h.done(vol.ID)
	if !h.tryStart(vol.ID) {
		t.Fatalf("tryStart after done = false, want true")
	}
}

// TestSchedulerFiresChangeHook drives the real tick path: a standalone
// index run on a volume with a hook must record an on-change hook tied to
// that index run, and the index run itself must still report success (the
// hook is best-effort and never affects it).
func TestSchedulerFiresChangeHook(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "fired")
	vol := &config.Volume{
		Name:       "v",
		IndexEvery: time.Minute,
		Hook: &config.VolumeHook{
			// Touch a marker so we can prove the command actually ran.
			Command: []string{"sh", "-c", "echo x > " + marker},
			Timeout: 5 * time.Second,
		},
	}
	f := newSchedulerFixture(t, vol)
	f.seedFile()
	sched := f.scheduler()
	ctx := context.Background()

	sched.tick(ctx)
	sched.hooks.wait()

	hooks, err := f.store.ListHookRuns(ctx, store.HookRunListOpts{})
	if err != nil {
		t.Fatalf("ListHookRuns: %v", err)
	}
	if len(hooks) != 1 {
		t.Fatalf("hook runs = %d, want 1 after a change-observing index", len(hooks))
	}
	hr := hooks[0]
	if hr.Trigger != store.HookTriggerChange {
		t.Fatalf("trigger = %q, want change", hr.Trigger)
	}
	if hr.Status != store.HookStatusSuccess {
		t.Fatalf("hook status = %q, want success", hr.Status)
	}
	if !hr.Changed {
		t.Fatalf("Changed = false, want true (the seeded file is an addition)")
	}
	// The hook's triggering run must be the index run the tick produced.
	idx, err := f.store.LatestSuccessfulIndexRun(ctx, hr.VolumeID)
	if err != nil {
		t.Fatalf("LatestSuccessfulIndexRun: %v", err)
	}
	if !hr.TriggeringRunID.Valid || hr.TriggeringRunID.Int64 != idx.ID {
		t.Fatalf("TriggeringRunID = %v, want index run %d", hr.TriggeringRunID, idx.ID)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("hook command did not run (marker absent): %v", err)
	}
}

// TestSchedulerFiresIntervalHook drives the interval ("check") trigger:
// a volume with only a hook interval (no index/sync cadence) fires the
// command on its cadence regardless of change, and not before. The
// trigger is recorded as 'interval' with no triggering run and
// changed=false.
func TestSchedulerFiresIntervalHook(t *testing.T) {
	vol := &config.Volume{
		Name: "v",
		Hook: &config.VolumeHook{
			Command:  []string{"sh", "-c", "exit 0"},
			Timeout:  5 * time.Second,
			Interval: 10 * time.Minute,
		},
	}
	f := newSchedulerFixture(t, vol)
	sched := f.scheduler()
	ctx := context.Background()

	// First tick: no prior interval hook, so it is due.
	sched.tick(ctx)
	sched.hooks.wait()
	assertIntervalHookCount(t, f, 1)

	// The interval hook fires independent of indexing — no runs row exists.
	runs, _ := f.store.ListRuns(ctx, store.ListRunsOpts{})
	if len(runs) != 0 {
		t.Fatalf("interval hook created %d runs rows; want 0 (it never indexes)", len(runs))
	}

	hooks, _ := f.store.ListHookRuns(ctx, store.HookRunListOpts{Descending: true})
	hr := hooks[0]
	if hr.Trigger != store.HookTriggerInterval {
		t.Fatalf("trigger = %q, want interval", hr.Trigger)
	}
	if hr.TriggeringRunID.Valid {
		t.Fatalf("TriggeringRunID = %v, want NULL for interval hook", hr.TriggeringRunID)
	}
	if hr.Changed {
		t.Fatalf("Changed = true, want false for interval hook")
	}
	if hr.Status != store.HookStatusSuccess {
		t.Fatalf("status = %q, want success", hr.Status)
	}

	// Under the cadence: a second tick must not re-fire.
	f.clock.Add(5 * time.Minute)
	sched.tick(ctx)
	sched.hooks.wait()
	assertIntervalHookCount(t, f, 1)

	// Past the cadence: the third tick re-fires.
	f.clock.Add(10 * time.Minute)
	sched.tick(ctx)
	sched.hooks.wait()
	assertIntervalHookCount(t, f, 2)
}

func assertIntervalHookCount(t *testing.T, f *schedulerFixture, want int) {
	t.Helper()
	hooks, err := f.store.ListHookRuns(context.Background(), store.HookRunListOpts{})
	if err != nil {
		t.Fatalf("ListHookRuns: %v", err)
	}
	n := 0
	for _, h := range hooks {
		if h.Trigger == store.HookTriggerInterval {
			n++
		}
	}
	if n != want {
		t.Fatalf("interval hook runs = %d, want %d", n, want)
	}
}

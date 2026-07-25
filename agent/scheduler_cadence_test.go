package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// TestResolveVerifyCadences pins the per-destination-vs-agent-default
// resolution and the verifiable-layout filter that the scheduler and the
// CLI's rclone wiring both rely on.
func TestResolveVerifyCadences(t *testing.T) {
	dests := map[string]*config.Destination{
		"ca":     {Name: "ca", Layout: config.LayoutContentAddressed, VerifyEvery: time.Hour},
		"packed": {Name: "packed", Layout: config.LayoutPacked}, // no own cadence → default
		"mirror": {Name: "mirror", Layout: config.LayoutMirror}, // never verifiable
	}
	got := resolveVerifyCadences(dests, 6*time.Hour)
	if got["ca"] != time.Hour {
		t.Errorf("ca cadence = %s; want its own 1h", got["ca"])
	}
	if got["packed"] != 6*time.Hour {
		t.Errorf("packed cadence = %s; want the 6h agent default", got["packed"])
	}
	if _, ok := got["mirror"]; ok {
		t.Errorf("mirror layout must never carry a verify cadence, got %s", got["mirror"])
	}

	// With no agent default, a verifiable destination without its own
	// cadence is omitted entirely.
	noDefault := resolveVerifyCadences(dests, 0)
	if _, ok := noDefault["packed"]; ok {
		t.Errorf("packed should be omitted when neither its own nor the agent default is set")
	}
	if noDefault["ca"] != time.Hour {
		t.Errorf("ca should still carry its own 1h with no agent default")
	}
}

// TestResolvePullCadences pins that only nodes declaring
// pull_durability_every carry a cadence.
func TestResolvePullCadences(t *testing.T) {
	nodes := map[string]*config.Node{
		"nas":    {Name: "nas", PullDurabilityEvery: 24 * time.Hour},
		"laptop": {Name: "laptop"}, // no cadence
	}
	got := resolvePullCadences(nodes)
	if got["nas"] != 24*time.Hour {
		t.Errorf("nas cadence = %s; want 24h", got["nas"])
	}
	if _, ok := got["laptop"]; ok {
		t.Errorf("laptop declares no pull cadence; must be omitted")
	}
}

// TestSchedulerAnyScheduledWork covers the broadened start gate: a
// verify-only or pull-only config (a receive-only node has no volume
// cadence at all) must still start the scheduler.
func TestSchedulerAnyScheduledWork(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{Name: "idle"})
	sch := f.scheduler()
	if sch.anyScheduledWork() {
		t.Fatalf("idle config should have no scheduled work")
	}
	sch.verifyEvery = map[string]time.Duration{"s3archive": time.Hour}
	if !sch.anyScheduledWork() {
		t.Fatalf("a verify cadence alone must start the scheduler")
	}
	sch.verifyEvery = nil
	sch.pullEvery = map[string]time.Duration{"nas": time.Hour}
	if !sch.anyScheduledWork() {
		t.Fatalf("a durability-pull cadence alone must start the scheduler (receive-only node)")
	}
}

// TestScheduledWorkInConfig is the config-side half of the same gate, which
// the CLI uses to refuse a do-nothing listener-less agent (F35). It must
// agree with anyScheduledWork on every cadence — in particular a
// receive-only machine (the reference htpc: no volume cadence, no scan, no
// listener) whose sole work is a peer pull_durability_every, or a
// verify-only machine, since refusing either would keep exactly the nodes
// F32/F33 exist to serve from ever starting.
func TestScheduledWorkInConfig(t *testing.T) {
	idleVol := map[string]*config.Volume{"idle": {Name: "idle"}}
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"nil config", nil, false},
		{"no agent block", &config.Config{Volumes: idleVol}, false},
		{"idle", &config.Config{Agent: &config.Agent{}, Volumes: idleVol}, false},
		{"scan interval", &config.Config{Agent: &config.Agent{ScanInterval: time.Hour}}, true},
		{"volume sync cadence", &config.Config{
			Agent:   &config.Agent{},
			Volumes: map[string]*config.Volume{"pics": {Name: "pics", SyncEvery: time.Hour}},
		}, true},
		{"per-destination verify cadence", &config.Config{
			Agent:        &config.Agent{},
			Volumes:      idleVol,
			Destinations: map[string]*config.Destination{"s3archive": {Name: "s3archive", Layout: config.LayoutPacked, VerifyEvery: time.Hour}},
		}, true},
		{"agent-default verify cadence", &config.Config{
			Agent:        &config.Agent{VerifyEvery: time.Hour},
			Volumes:      idleVol,
			Destinations: map[string]*config.Destination{"s3archive": {Name: "s3archive", Layout: config.LayoutContentAddressed}},
		}, true},
		{"verify default on an unverifiable layout", &config.Config{
			Agent:        &config.Agent{VerifyEvery: time.Hour},
			Volumes:      idleVol,
			Destinations: map[string]*config.Destination{"cloudbox": {Name: "cloudbox", Layout: config.LayoutMirror}},
		}, false},
		{"receive-only pull cadence", &config.Config{
			Agent:   &config.Agent{},
			Volumes: idleVol,
			Nodes:   map[string]*config.Node{"nas": {Name: "nas", PullDurabilityEvery: 24 * time.Hour}},
		}, true},
		{"peer node without a pull cadence", &config.Config{
			Agent:   &config.Agent{},
			Volumes: idleVol,
			Nodes:   map[string]*config.Node{"nas": {Name: "nas"}},
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ScheduledWorkInConfig(tc.cfg); got != tc.want {
				t.Fatalf("ScheduledWorkInConfig = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestSchedulerVerifyCadence walks the verify cadence across ticks: first
// tick kicks (never verified), a within-cadence tick skips, a past-cadence
// tick re-fires. The kicked/finished discipline is asserted from the log.
func TestSchedulerVerifyCadence(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{Name: "idle"})
	sch := f.scheduler()
	var calls atomic.Int32
	sch.verifyRun = func(context.Context, string) VerifyRunReport {
		calls.Add(1)
		return VerifyRunReport{RunID: 7, Status: store.RunStatusSuccess}
	}
	sch.verifyEvery = map[string]time.Duration{"s3archive": time.Hour}
	ctx := context.Background()

	sch.tick(ctx)
	if calls.Load() != 1 {
		t.Fatalf("tick 1 verify calls = %d; want 1", calls.Load())
	}
	if !f.containsLogLine("scheduler.kicked", "kind=verify", "destination=s3archive") {
		t.Fatalf("missing verify kicked log:\n%s", f.logBuf.String())
	}
	if !f.containsLogLine("scheduler.finished", "kind=verify", "destination=s3archive", "status=success", "run_id=7") {
		t.Fatalf("missing verify finished log:\n%s", f.logBuf.String())
	}

	f.clock.Add(30 * time.Minute)
	sch.tick(ctx)
	if calls.Load() != 1 {
		t.Fatalf("within-cadence tick verify calls = %d; want 1", calls.Load())
	}

	f.clock.Add(time.Hour)
	sch.tick(ctx)
	if calls.Load() != 2 {
		t.Fatalf("past-cadence tick verify calls = %d; want 2", calls.Load())
	}
}

// TestSchedulerVerifyNoOp: a destination with nothing recorded to verify
// yields an empty status + zero run id, logged distinctly as a no-op (no
// audit run was written) rather than as a run.
func TestSchedulerVerifyNoOp(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{Name: "idle"})
	sch := f.scheduler()
	sch.verifyRun = func(context.Context, string) VerifyRunReport {
		return VerifyRunReport{RunID: 0, Status: ""}
	}
	sch.verifyEvery = map[string]time.Duration{"s3archive": time.Hour}

	sch.tick(context.Background())
	if !f.containsLogLine("scheduler.finished", "kind=verify", "status=no-op", "run_id=0") {
		t.Fatalf("missing no-op verify finished log:\n%s", f.logBuf.String())
	}
}

// TestSchedulerVerifyErrorConsumesCadence: a failed verify logs an error but
// still advances the watermark, so the next within-cadence tick does not
// re-fire (no special retry — same failure policy as the volume cadences).
func TestSchedulerVerifyErrorConsumesCadence(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{Name: "idle"})
	sch := f.scheduler()
	var calls atomic.Int32
	sch.verifyRun = func(context.Context, string) VerifyRunReport {
		calls.Add(1)
		return VerifyRunReport{RunID: 9, Status: store.RunStatusFailed, Err: errors.New("boom")}
	}
	sch.verifyEvery = map[string]time.Duration{"s3archive": time.Hour}
	ctx := context.Background()

	sch.tick(ctx)
	if calls.Load() != 1 {
		t.Fatalf("tick 1 verify calls = %d; want 1", calls.Load())
	}
	if !f.containsLogLine("scheduler.error", "kind=verify", "destination=s3archive", "boom") {
		t.Fatalf("missing verify error log:\n%s", f.logBuf.String())
	}
	f.clock.Add(10 * time.Minute)
	sch.tick(ctx)
	if calls.Load() != 1 {
		t.Fatalf("within-cadence tick after failure verify calls = %d; want 1 (no retry)", calls.Load())
	}
}

// TestSchedulerDurabilityPullCadence walks the pull cadence for a volume
// with a durability stake: first tick pulls, within-cadence skips,
// past-cadence re-fires. Counts ride into the finished log line.
func TestSchedulerDurabilityPullCadence(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{
		Name:            "media",
		OffloadRequires: []string{"s3archive"},
	})
	sch := f.scheduler()
	var calls []string
	sch.durabilityPull = func(_ context.Context, vol *config.Volume, peer string) DurabilityPullReport {
		calls = append(calls, vol.Name+"/"+peer)
		return DurabilityPullReport{RunID: 5, Status: store.RunStatusSuccess, Fetched: 3, Applied: 2}
	}
	sch.pullEvery = map[string]time.Duration{"nas": 24 * time.Hour}
	ctx := context.Background()

	sch.tick(ctx)
	if len(calls) != 1 || calls[0] != "media/nas" {
		t.Fatalf("tick 1 pull calls = %v; want [media/nas]", calls)
	}
	if !f.containsLogLine("scheduler.kicked", "kind=pull-durability", "volume=media", "peer=nas") {
		t.Fatalf("missing pull kicked log:\n%s", f.logBuf.String())
	}
	if !f.containsLogLine("scheduler.finished", "kind=pull-durability", "status=success", "run_id=5", "fetched=3", "applied=2") {
		t.Fatalf("missing pull finished log:\n%s", f.logBuf.String())
	}

	f.clock.Add(6 * time.Hour)
	sch.tick(ctx)
	if len(calls) != 1 {
		t.Fatalf("within-cadence tick pull calls = %d; want 1", len(calls))
	}

	f.clock.Add(24 * time.Hour)
	sch.tick(ctx)
	if len(calls) != 2 {
		t.Fatalf("past-cadence tick pull calls = %d; want 2", len(calls))
	}
}

// TestSchedulerDurabilitySkipsVolumeWithoutStake: a volume that names
// neither offload_requires nor sync_to has no evidence to gain from a pull
// (every component would be dropped), so it is skipped — the reference
// htpc's playback-only photos volume, versus its offloadable media volume.
func TestSchedulerDurabilitySkipsVolumeWithoutStake(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{Name: "photos"})
	sch := f.scheduler()
	var calls int
	sch.durabilityPull = func(context.Context, *config.Volume, string) DurabilityPullReport {
		calls++
		return DurabilityPullReport{Status: store.RunStatusSuccess}
	}
	sch.pullEvery = map[string]time.Duration{"nas": time.Hour}

	sch.tick(context.Background())
	if calls != 0 {
		t.Fatalf("pull invoked %d times for a stakeless volume; want 0", calls)
	}
}

// TestSchedulerDurabilityPullPartialOnRewind: a puller reporting refused
// rewinds finishes 'partial' (surfaced, never applied — the agent does not
// escalate) and still advances the watermark.
func TestSchedulerDurabilityPullPartialOnRewind(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{
		Name:   "media",
		SyncTo: []string{"nas"},
	})
	sch := f.scheduler()
	sch.durabilityPull = func(context.Context, *config.Volume, string) DurabilityPullReport {
		return DurabilityPullReport{RunID: 8, Status: store.RunStatusPartial, Fetched: 2, Rewinds: 1}
	}
	sch.pullEvery = map[string]time.Duration{"nas": time.Hour}

	sch.tick(context.Background())
	if !f.containsLogLine("scheduler.finished", "kind=pull-durability", "status=partial", "run_id=8", "rewinds=1") {
		t.Fatalf("missing partial pull finished log (with refused-rewind count):\n%s", f.logBuf.String())
	}
}

// TestSchedulerNilCadenceRunners: when the injected runners are nil (no
// rclone wired, no pull configured) the phases are inert and touch no
// watermark — a config that declares neither cadence never trips them.
func TestSchedulerNilCadenceRunners(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{Name: "idle"})
	sch := f.scheduler()
	// Cadences present but runners nil: must not panic and must do nothing.
	sch.verifyEvery = map[string]time.Duration{"s3archive": time.Hour}
	sch.pullEvery = map[string]time.Duration{"nas": time.Hour}
	sch.verifyRun = nil
	sch.durabilityPull = nil

	sch.tick(context.Background())
	if f.containsLogLine("kind=verify") || f.containsLogLine("kind=pull-durability") {
		t.Fatalf("nil runners produced cadence activity:\n%s", f.logBuf.String())
	}
}

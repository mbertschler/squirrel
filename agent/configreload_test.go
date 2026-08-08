package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// The three edits every reload test is built from: a policy-only change
// (add a volume), a process-shaped change (rotate the bearer token), and
// the two together.
const (
	addVolumeBody    = driftConfigBody + "\n[volumes.docs]\npath = \"/tmp/docs\"\n"
	rotatedTokenBody = `
[agent]
listen = "127.0.0.1:0"
[agent.auth]
token = "rotated"

[volumes.pictures]
path = "/tmp/pictures"

[volumes.docs]
path = "/tmp/docs"
`
	revertedTokenBody = `
[agent]
listen = "127.0.0.1:0"
[agent.auth]
token = "tok"

[volumes.pictures]
path = "/tmp/pictures"

[volumes.docs]
path = "/tmp/docs"
`
)

// TestConfigMonitorReloadsPolicyChange is the headline #204 path: an
// operator adds a volume and the running agent is hosting it a minute
// later, with nothing typed and no latch left behind.
func TestConfigMonitorReloadsPolicyChange(t *testing.T) {
	f := newReloadFixture(t, nil)
	ctx := context.Background()

	f.rewrite(addVolumeBody)
	f.monitor.check(ctx)

	if _, ok := f.latch(); ok {
		t.Fatal("latched after a reload that applied the whole edit")
	}
	if _, ok := f.live.Get().Volumes["docs"]; !ok {
		t.Fatalf("live config = %v, want the added volume in force", f.live.Get().Volumes)
	}
	if !strings.Contains(f.logBuf.String(), "config reloaded") {
		t.Fatalf("reload was not logged:\n%s", f.logBuf.String())
	}
	// The agent changed its own operating configuration: that is automatic
	// work, and automatic work is never invisible (ux-principle 5).
	assertReloadRun(t, f, store.RunStatusSuccess, "applied=volumes", "pending=none")

	// A second check has nothing to do — the agent is now running the file.
	before := len(listRuns(t, f))
	f.monitor.check(ctx)
	if after := len(listRuns(t, f)); after != before {
		t.Fatalf("runs = %d after a no-op check, want %d", after, before)
	}
}

// TestConfigMonitorLatchesPendingRestartKeys is the honest-partial path: the
// policy half of an edit goes live, and the latch names exactly what a
// restart still owes rather than saying only that *something* changed.
func TestConfigMonitorLatchesPendingRestartKeys(t *testing.T) {
	f := newReloadFixture(t, nil)
	ctx := context.Background()

	f.rewrite(rotatedTokenBody)
	f.monitor.check(ctx)

	d, ok := f.latch()
	if !ok {
		t.Fatal("no latch after an edit that rotated the bearer token")
	}
	if want := []string{keyAgentToken}; !slices.Equal(d.PendingKeys, want) {
		t.Fatalf("pending keys = %v, want %v", d.PendingKeys, want)
	}
	if d.ApplyError != "" {
		t.Fatalf("apply error = %q, want none — the reload succeeded as far as it goes", d.ApplyError)
	}
	if _, ok := f.live.Get().Volumes["docs"]; !ok {
		t.Fatal("the reloadable half of the edit was not applied")
	}
	if msg := store.ConfigDriftMessageFor(d.PendingKeys, d.ApplyError); !strings.Contains(msg, keyAgentToken) {
		t.Fatalf("latch message %q does not name the key that needs a restart", msg)
	}
	assertReloadRun(t, f, store.RunStatusPartial, "applied=volumes", "pending="+keyAgentToken)

	// The agent is running the file on disk, so the digests now agree — but
	// the restart is still owed and the latch must not fall away on the
	// next tick as if the edit had been reverted.
	f.monitor.check(ctx)
	if _, ok := f.latch(); !ok {
		t.Fatal("the pending-restart latch was cleared by a later check")
	}
}

// TestConfigMonitorClearsWhenPendingKeyReverts: the operator rotates a
// token, thinks better of it, and puts the old value back while keeping the
// rest of their edit. Nothing needs a restart any more — the listener is
// already running that credential — so the latch must go.
func TestConfigMonitorClearsWhenPendingKeyReverts(t *testing.T) {
	f := newReloadFixture(t, nil)
	ctx := context.Background()

	f.rewrite(rotatedTokenBody)
	f.monitor.check(ctx)
	if _, ok := f.latch(); !ok {
		t.Fatal("no latch after the token rotation")
	}

	f.rewrite(revertedTokenBody)
	f.monitor.check(ctx)
	if d, ok := f.latch(); ok {
		t.Fatalf("latch %+v survived the token going back to what the listener is running", d)
	}
	if _, ok := f.live.Get().Volumes["docs"]; !ok {
		t.Fatal("the volume added alongside the rotation was lost on the revert")
	}
}

// TestConfigMonitorRefusesUnparseableConfig is the safety property #199
// stopped short for: a file that no longer loads must leave the running
// agent exactly as it was, and say so.
func TestConfigMonitorRefusesUnparseableConfig(t *testing.T) {
	f := newReloadFixture(t, nil)
	ctx := context.Background()
	before := f.live.Get()

	f.rewrite("[volumes.docs\npath = ")
	f.monitor.check(ctx)

	if f.live.Get() != before {
		t.Fatal("a config that does not parse replaced the running one")
	}
	d, ok := f.latch()
	if !ok {
		t.Fatal("no latch after the config on disk stopped parsing")
	}
	if d.ApplyError == "" {
		t.Fatalf("latch %+v carries no reason, want the parse failure", d)
	}
	if len(d.PendingKeys) != 0 {
		t.Fatalf("pending keys = %v, want none — nothing was applied", d.PendingKeys)
	}
	if msg := store.ConfigDriftMessageFor(d.PendingKeys, d.ApplyError); !strings.Contains(msg, "could not be applied") {
		t.Fatalf("latch message = %q, want it to say the edit was refused", msg)
	}
}

// TestConfigMonitorRefusesWhenPrepareFails: the state derived from a config
// is part of adopting it. If the rclone.conf cannot be rendered — or rclone
// cannot be found for a destination the edit just added — the reload is
// abandoned whole, not applied halfway.
func TestConfigMonitorRefusesWhenPrepareFails(t *testing.T) {
	f := newReloadFixture(t, func(context.Context, *config.Config) error {
		return errors.New("rclone not found")
	})
	ctx := context.Background()
	before := f.live.Get()

	f.rewrite(addVolumeBody)
	f.monitor.check(ctx)

	if f.live.Get() != before {
		t.Fatal("the reload swapped the config despite prepare failing")
	}
	d, ok := f.latch()
	if !ok {
		t.Fatal("no latch after prepare refused the new config")
	}
	if !strings.Contains(d.ApplyError, "rclone not found") {
		t.Fatalf("latch reason = %q, want it to carry the prepare failure", d.ApplyError)
	}
	// The agent has not adopted the file, so the next check must try again
	// rather than treating the edit as dealt with.
	f.monitor.check(ctx)
	if _, ok := f.latch(); !ok {
		t.Fatal("the refusal latch disappeared on the next check")
	}
}

// TestConfigMonitorCosmeticEditRecordsNothing: a comment or a reformat
// changes the file's bytes but not what it resolves to. The agent adopts it
// silently — no latch, and no run in a trail that is never pruned.
func TestConfigMonitorCosmeticEditRecordsNothing(t *testing.T) {
	f := newReloadFixture(t, nil)
	ctx := context.Background()

	f.rewrite(driftConfigBody + "\n# a note to self\n")
	f.monitor.check(ctx)

	if d, ok := f.latch(); ok {
		t.Fatalf("latch %+v raised for an edit that changed nothing", d)
	}
	if runs := listRuns(t, f); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none for a cosmetic edit", runs)
	}
}

// TestConfigMonitorWithoutLiveConfigOnlyDetects pins the embedder's
// behaviour: no live config means no reload, and the latch says exactly what
// it said before #204.
func TestConfigMonitorWithoutLiveConfigOnlyDetects(t *testing.T) {
	f := newDriftFixture(t)
	ctx := context.Background()

	f.rewrite(addVolumeBody)
	f.monitor.check(ctx)

	d, ok := f.latch()
	if !ok {
		t.Fatal("no latch after the config changed")
	}
	if len(d.PendingKeys) != 0 || d.ApplyError != "" {
		t.Fatalf("latch = %+v, want the plain whole-file-pending shape", d)
	}
	if got := store.ConfigDriftMessageFor(d.PendingKeys, d.ApplyError); got != store.ConfigDriftMessage {
		t.Fatalf("message = %q, want %q", got, store.ConfigDriftMessage)
	}
}

// TestSchedulerAdoptsReloadedCadence proves the reload reaches the thing it
// exists for: a volume that gained a sync cadence while the agent ran is
// evaluated on the next tick, with no restart and no new scheduler.
func TestSchedulerAdoptsReloadedCadence(t *testing.T) {
	f := newSchedulerFixture(t, &config.Volume{Name: "pics"})
	f.seedFile()
	sch := f.scheduler()
	ctx := context.Background()

	sch.tick(ctx)
	if got := len(f.syncLog.Calls()); got != 0 {
		t.Fatalf("sync invoked %d times before the cadence existed; want 0", got)
	}

	scheduled := *f.srv.live.Get().Volumes["pics"]
	scheduled.SyncEvery = time.Hour
	scheduled.SyncTo = []string{"nas"}
	f.srv.live.Store(&config.Config{Volumes: map[string]*config.Volume{"pics": &scheduled}})

	f.clock.Add(time.Hour)
	sch.tick(ctx)
	sch.dispatch.wait() // syncs run on per-destination workers; drain them
	if got := f.syncLog.Calls(); len(got) != 1 || got[0].Destination != "nas" {
		t.Fatalf("sync calls = %+v, want one kick to nas from the reloaded cadence", got)
	}
}

// TestConfigChangesClassifiesEveryKey is the classification table: which
// side of the reloadable line each config key falls on, and the baseline
// each side is measured against.
func TestConfigChangesClassifiesEveryKey(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{
			DB:       "/idx.db",
			NodeName: "homepc",
			Volumes:  map[string]*config.Volume{"pics": {Name: "pics", Path: "/pics"}},
			Backups:  config.DefaultBackups(),
			Agent: &config.Agent{
				Listen: ":8443", Token: "tok", ScanStrategy: config.ScanStrategyShallow,
			},
		}
	}
	for _, tc := range []struct {
		name        string
		edit        func(*config.Config)
		wantApplied []string
		wantPending []string
	}{
		{"nothing", func(*config.Config) {}, nil, nil},
		{"volume path", func(c *config.Config) { c.Volumes["pics"].Path = "/photos" },
			[]string{keyVolumes}, nil},
		{"volume added", func(c *config.Config) { c.Volumes["docs"] = &config.Volume{Name: "docs"} },
			[]string{keyVolumes}, nil},
		{"destination added", func(c *config.Config) {
			c.Destinations = map[string]*config.Destination{"s3": {Name: "s3"}}
		}, []string{keyDestinations}, nil},
		{"node added", func(c *config.Config) { c.Nodes = map[string]*config.Node{"nas": {Name: "nas"}} },
			[]string{keyNodes}, nil},
		{"backups", func(c *config.Config) { c.Backups.Keep = 30 }, []string{keyBackups}, nil},
		{"verify default", func(c *config.Config) { c.Agent.VerifyEvery = time.Hour },
			[]string{keyAgentVerify}, nil},
		{"db", func(c *config.Config) { c.DB = "/other.db" }, nil, []string{keyDB}},
		{"node name", func(c *config.Config) { c.NodeName = "nas" }, nil, []string{keyNodeName}},
		{"listen", func(c *config.Config) { c.Agent.Listen = ":9000" }, nil, []string{keyAgentListen}},
		{"agent db", func(c *config.Config) { c.Agent.DB = "/agent.db" }, nil, []string{keyAgentDB}},
		{"tls", func(c *config.Config) { c.Agent.TLSCert, c.Agent.TLSKey = "/c.pem", "/k.pem" },
			nil, []string{keyAgentTLS}},
		{"token", func(c *config.Config) { c.Agent.Token = "rotated" }, nil, []string{keyAgentToken}},
		{"peers", func(c *config.Config) { c.Agent.PeerTokens = map[string]string{"secret": "nas"} },
			nil, []string{keyAgentPeers}},
		{"scan interval", func(c *config.Config) { c.Agent.ScanInterval = time.Hour },
			nil, []string{keyAgentScanEvery}},
		{"scan strategy", func(c *config.Config) { c.Agent.ScanStrategy = config.ScanStrategyDeep },
			nil, []string{keyAgentScanPolicy}},
		{"agent block removed", func(c *config.Config) { c.Agent = nil },
			nil, []string{keyAgentListen, keyAgentToken, keyAgentScanPolicy}},
		{"both halves", func(c *config.Config) {
			c.Volumes["docs"] = &config.Volume{Name: "docs"}
			c.Agent.Token = "rotated"
		}, []string{keyVolumes}, []string{keyAgentToken}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			booted, next := base(), base()
			tc.edit(next)
			applied, pending := configChanges(booted, booted, next)
			if !slices.Equal(applied, tc.wantApplied) {
				t.Errorf("applied = %v, want %v", applied, tc.wantApplied)
			}
			if !slices.Equal(pending, tc.wantPending) {
				t.Errorf("pending = %v, want %v", pending, tc.wantPending)
			}
		})
	}
}

// TestConfigChangesMeasuresProcessKeysAgainstBoot is the subtle half of the
// classification: a process-shaped key that has already drifted from the
// running config is only pending while it differs from what the process
// actually bound. Measuring it against the live config instead would demand
// a restart for the credential the listener is already using.
func TestConfigChangesMeasuresProcessKeysAgainstBoot(t *testing.T) {
	booted := &config.Config{Agent: &config.Agent{Token: "tok"}}
	// A previous reload left the live config carrying the rotated token,
	// even though the listener still terminates the booted one.
	running := &config.Config{Agent: &config.Agent{Token: "rotated"}}
	next := &config.Config{Agent: &config.Agent{Token: "tok"}}

	_, pending := configChanges(running, booted, next)
	if len(pending) != 0 {
		t.Fatalf("pending = %v, want none: the file names the token the listener is running", pending)
	}
}

// assertReloadRun checks that the trail carries exactly one config-reload
// run with the expected status, and that its audit note names what moved.
func assertReloadRun(t *testing.T, f *driftFixture, wantStatus string, wantNoteParts ...string) {
	t.Helper()
	ctx := context.Background()
	for _, run := range listRuns(t, f) {
		entries, err := f.store.ListRunAudit(ctx, run.ID)
		if err != nil {
			t.Fatalf("ListRunAudit: %v", err)
		}
		for _, e := range entries {
			if e.Transition != store.TransitionConfigReload {
				continue
			}
			if run.Status != wantStatus {
				t.Fatalf("reload run status = %q, want %q", run.Status, wantStatus)
			}
			for _, part := range wantNoteParts {
				if !strings.Contains(e.Note.String, part) {
					t.Fatalf("reload note = %q, want it to contain %q", e.Note.String, part)
				}
			}
			return
		}
	}
	t.Fatalf("no %s run recorded for the reload", store.TransitionConfigReload)
}

func listRuns(t *testing.T, f *driftFixture) []store.Run {
	t.Helper()
	runs, err := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	return runs
}

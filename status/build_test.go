package status

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
)

func setupStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "status.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// indexTree writes three files into a fresh directory and indexes it as
// volume name, returning the root and the index run.
func indexTree(t *testing.T, s *store.Store, name string) (string, index.Report) {
	t.Helper()
	root := t.TempDir()
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("content-"+n), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	rep, err := index.Index(context.Background(), s, root, index.Options{Name: name, Workers: 2})
	if err != nil || rep.Errors > 0 {
		t.Fatalf("index %s: err=%v errors=%v", name, err, rep.ErrorList)
	}
	return root, rep
}

// cfgFor builds a minimal config with one volume and its targets. Every
// volume gets a one-hour sync cadence so a fresh sync reads as on-time.
func cfgFor(volName, root string, syncTo, requires []string, dests, nodes []string) *config.Config {
	cfg := &config.Config{
		Volumes:      map[string]*config.Volume{},
		Destinations: map[string]*config.Destination{},
		Nodes:        map[string]*config.Node{},
	}
	cfg.Volumes[volName] = &config.Volume{
		Name:            volName,
		Path:            root,
		SyncTo:          syncTo,
		OffloadRequires: requires,
		SyncEvery:       time.Hour,
	}
	for _, d := range dests {
		cfg.Destinations[d] = &config.Destination{Name: d, Type: "s3", Layout: config.LayoutContentAddressed}
	}
	for _, n := range nodes {
		cfg.Nodes[n] = &config.Node{Name: n}
	}
	return cfg
}

func recordSuccessfulSync(t *testing.T, s *store.Store, volID int64, dest string) {
	t.Helper()
	id, blocker, err := s.BeginSyncRunIfClear(context.Background(), store.SyncRunSpec{VolumeID: volID, Destination: dest})
	if err != nil || blocker != nil {
		t.Fatalf("BeginSyncRunIfClear(%s): err=%v blocker=%+v", dest, err, blocker)
	}
	if err := s.FinishRun(context.Background(), id, store.RunStatusSuccess, "", 3); err != nil {
		t.Fatalf("FinishRun(%s): %v", dest, err)
	}
}

// TestBuildFullyDurable exercises the happy path end to end: an indexed
// volume, a fresh successful sync, a content-verified durability vector
// covering the local origin, and an offload policy — everything green and
// every file offloadable.
func TestBuildFullyDurable(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, idx := indexTree(t, s, "photos")
	v, err := s.GetVolumeByName(ctx, "photos")
	if err != nil {
		t.Fatalf("GetVolumeByName: %v", err)
	}
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "dest", self.ID, idx.RunID, store.VerifyMethodBlake3, false); err != nil {
		t.Fatalf("seed vector: %v", err)
	}
	recordSuccessfulSync(t, s, v.ID, "dest")

	cfg := cfgFor("photos", root, []string{"dest"}, []string{"dest"}, []string{"dest"}, nil)
	rep, err := Build(ctx, s, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(rep.Volumes) != 1 {
		t.Fatalf("volumes = %d, want 1", len(rep.Volumes))
	}
	vs := rep.Volumes[0]
	if !vs.Indexed || vs.LastIndexAgo == nil {
		t.Fatalf("volume not indexed/aged: %+v", vs)
	}
	if len(vs.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(vs.Targets))
	}
	tg := vs.Targets[0]
	if tg.Kind != KindDestination || !tg.SyncTarget || !tg.Required {
		t.Errorf("target role wrong: %+v", tg)
	}
	if tg.Standing != StandingNone || tg.SyncLevel != LevelOK {
		t.Errorf("target coverage = standing %v level %v, want none/ok", tg.Standing, tg.SyncLevel)
	}
	if tg.Durability == nil || !tg.Durability.LocalContent || !tg.Durability.Covered || tg.Durability.Method != store.VerifyMethodBlake3 {
		t.Errorf("durability wrong: %+v", tg.Durability)
	}
	if !vs.Offload.Applicable || vs.Offload.OffloadableFiles != 3 || vs.Offload.PresentFiles != 3 {
		t.Errorf("offload readiness wrong: %+v", vs.Offload)
	}
	if vs.Offload.OffloadableBytes != vs.Offload.PresentBytes || vs.Offload.OffloadableBytes == 0 {
		t.Errorf("offload bytes wrong: %+v", vs.Offload)
	}
	if rep.Level() != LevelOK {
		t.Errorf("report level = %v, want ok", rep.Level())
	}
}

// TestBuildNeedsBootstrap: a refused sync whose reason names --init, with
// no prior success, is the amber one-time-bootstrap state — not red.
func TestBuildNeedsBootstrap(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, _ := indexTree(t, s, "docs")
	v, _ := s.GetVolumeByName(ctx, "docs")
	id, err := s.BeginRun(ctx, store.RunKindSync, v.ID, "dest", false)
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	if err := s.FinishRun(ctx, id, store.RunStatusRefused, "destination \"dest\" has no marker — re-run with --init to bootstrap", 0); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	cfg := cfgFor("docs", root, []string{"dest"}, nil, []string{"dest"}, nil)
	rep, err := Build(ctx, s, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	tg := rep.Volumes[0].Targets[0]
	if tg.Standing != StandingNeedsBootstrap {
		t.Errorf("standing = %v, want needs-bootstrap", tg.Standing)
	}
	if rep.Level() != LevelWarn {
		t.Errorf("report level = %v, want amber", rep.Level())
	}
}

// TestBuildAlarm: a latched destination alarm reddens its target.
func TestBuildAlarm(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, idx := indexTree(t, s, "media")
	if _, err := s.RaiseDestinationAlarm(ctx, "dest", store.AlarmKindVerifyMismatch, "checksum mismatch on object", idx.RunID); err != nil {
		t.Fatalf("RaiseDestinationAlarm: %v", err)
	}
	cfg := cfgFor("media", root, []string{"dest"}, nil, []string{"dest"}, nil)
	rep, err := Build(ctx, s, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	tg := rep.Volumes[0].Targets[0]
	if tg.Standing != StandingAlarm || tg.StandingDetail == "" {
		t.Errorf("standing = %v detail=%q, want alarm", tg.Standing, tg.StandingDetail)
	}
	if rep.Level() != LevelCritical {
		t.Errorf("report level = %v, want red", rep.Level())
	}
}

// TestBuildConfigDrift: a standing config-drift latch (F9) surfaces on the
// report as a node-wide amber, even when every volume is otherwise fine —
// the whole grid was built from a file the agent is no longer running.
func TestBuildConfigDrift(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, _ := indexTree(t, s, "media")
	loaded, disk := make([]byte, 32), make([]byte, 32)
	disk[0] = 1
	if _, err := s.RaiseConfigDrift(ctx, store.ConfigDriftState{Path: "/etc/squirrel/config.toml", Loaded: loaded, Disk: disk}); err != nil {
		t.Fatalf("RaiseConfigDrift: %v", err)
	}
	cfg := cfgFor("media", root, nil, nil, nil, nil)
	rep, err := Build(ctx, s, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.ConfigDrift == nil {
		t.Fatal("report carries no config drift, want the standing latch")
	}
	if rep.ConfigDrift.Path != "/etc/squirrel/config.toml" {
		t.Errorf("drift path = %q, want the config path", rep.ConfigDrift.Path)
	}
	if rep.Level() != LevelWarn {
		t.Errorf("report level = %v, want amber", rep.Level())
	}
	label := ConfigDriftLabel(*rep.ConfigDrift)
	if !strings.Contains(label, "restart to apply") || !strings.Contains(label, "/etc/squirrel/config.toml") {
		t.Errorf("label = %q, want the restart sentence and the path", label)
	}
}

// TestBuildNoConfigDrift: nothing latched, nothing reported — the healthy
// install must not grow a permanent amber line.
func TestBuildNoConfigDrift(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, _ := indexTree(t, s, "media")
	rep, err := Build(ctx, s, cfgFor("media", root, nil, nil, nil, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.ConfigDrift != nil {
		t.Fatalf("report carries config drift %+v with nothing latched", rep.ConfigDrift)
	}
}

// TestBuildNeverIndexed: a configured-but-unindexed volume is amber and
// lists its planned targets rather than rendering blank (F4).
func TestBuildNeverIndexed(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	cfg := cfgFor("photos", "/does/not/matter", []string{"nas"}, nil, nil, []string{"nas"})
	rep, err := Build(ctx, s, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	vs := rep.Volumes[0]
	if vs.Indexed {
		t.Errorf("volume should not be indexed")
	}
	if len(vs.Targets) != 1 || vs.Targets[0].Kind != KindNode {
		t.Errorf("planned targets wrong: %+v", vs.Targets)
	}
	if vs.Level() != LevelWarn {
		t.Errorf("volume level = %v, want amber", vs.Level())
	}
	// No offload policy configured here, so it must not claim one.
	if vs.Offload.Applicable {
		t.Errorf("offload should not be applicable without a policy")
	}
}

// TestBuildNeverIndexedReportsPolicy: a never-indexed volume that DOES
// declare offload_requires must report Applicable so the surface shows its
// policy state, not "no policy" (Copilot review on #177).
func TestBuildNeverIndexedReportsPolicy(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	cfg := cfgFor("photos", "/does/not/matter", []string{"nas"}, []string{"nas", "s3archive"}, nil, []string{"nas"})
	rep, err := Build(ctx, s, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	vs := rep.Volumes[0]
	if vs.Indexed {
		t.Errorf("volume should not be indexed")
	}
	if !vs.Offload.Applicable {
		t.Errorf("offload should be applicable — the config declares offload_requires")
	}
	if vs.Offload.OffloadableFiles != 0 || vs.Offload.PresentFiles != 0 {
		t.Errorf("never-indexed volume should report zero counts: %+v", vs.Offload)
	}
}

// TestBuildSkipOffloadReadiness: the TUI tick path skips the whole-index
// gate pass but still reports the policy flag; the CLI path computes the
// counts. Both see the same coverage/durability grid.
func TestBuildSkipOffloadReadiness(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, idx := indexTree(t, s, "photos")
	v, _ := s.GetVolumeByName(ctx, "photos")
	self, _ := s.GetSelfNode(ctx)
	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "dest", self.ID, idx.RunID, store.VerifyMethodBlake3, false); err != nil {
		t.Fatalf("seed vector: %v", err)
	}
	recordSuccessfulSync(t, s, v.ID, "dest")
	cfg := cfgFor("photos", root, []string{"dest"}, []string{"dest"}, []string{"dest"}, nil)

	skipped, err := BuildWithOptions(ctx, s, cfg, Options{SkipOffloadReadiness: true})
	if err != nil {
		t.Fatalf("BuildWithOptions(skip): %v", err)
	}
	so := skipped.Volumes[0].Offload
	if !so.Applicable || so.OffloadableFiles != 0 || so.PresentFiles != 0 {
		t.Errorf("skip-readiness offload = %+v, want applicable with zero counts", so)
	}
	full, err := Build(ctx, s, cfg)
	if err != nil {
		t.Fatalf("Build(full): %v", err)
	}
	fo := full.Volumes[0].Offload
	if !fo.Applicable || fo.OffloadableFiles != 3 || fo.PresentFiles != 3 {
		t.Errorf("full offload = %+v, want 3 offloadable of 3 present", fo)
	}
	// The coverage grid is identical regardless of the readiness option.
	if len(skipped.Volumes[0].Targets) != len(full.Volumes[0].Targets) {
		t.Errorf("target count differs between skip and full builds")
	}
}

// TestBuildRelayedRequiredTarget: a target named only in offload_requires
// (never in sync_to) reads as relayed for coverage but is scored on its
// durability; with no evidence yet it is amber, not red.
func TestBuildRelayedRequiredTarget(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, _ := indexTree(t, s, "photos")
	cfg := cfgFor("photos", root, []string{"nas"}, []string{"nas", "s3archive"}, nil, []string{"nas"})
	rep, err := Build(ctx, s, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var relayed *TargetStatus
	for i := range rep.Volumes[0].Targets {
		if rep.Volumes[0].Targets[i].Name == "s3archive" {
			relayed = &rep.Volumes[0].Targets[i]
		}
	}
	if relayed == nil {
		t.Fatalf("relayed target s3archive missing from grid")
	}
	if relayed.SyncTarget || !relayed.Required || relayed.Kind != KindRelayed {
		t.Errorf("relayed target classification wrong: %+v", relayed)
	}
	if relayed.SyncLevel != LevelNeutral {
		t.Errorf("relayed sync level = %v, want neutral", relayed.SyncLevel)
	}
	if relayed.Durability == nil || relayed.Durability.Covered {
		t.Errorf("relayed durability should be uncovered: %+v", relayed.Durability)
	}
	if relayed.Level() != LevelWarn {
		t.Errorf("relayed target level = %v, want amber (evidence not yet arrived)", relayed.Level())
	}
}

// TestBuildNodeBytePathUnavailable is F34's load-bearing case: the peer's
// byte-path does not resolve, so no byte this machine sends can land — and
// nothing said so until someone chose to run `squirrel config check`. The
// status surface now carries it, amber rather than red because the usual
// cause is a mount that is not up yet.
func TestBuildNodeBytePathUnavailable(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, _ := indexTree(t, s, "media")
	cfg := cfgFor("media", root, []string{"nas"}, nil, nil, []string{"nas"})
	cfg.Nodes["nas"].Path = filepath.Join(t.TempDir(), "mount-not-up")

	rep, err := Build(ctx, s, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	tg := rep.Volumes[0].Targets[0]
	if tg.Standing != StandingBytePath {
		t.Errorf("standing = %v, want byte-path", tg.Standing)
	}
	if tg.StandingDetail == "" {
		t.Error("standing carries no detail, so the surface cannot say what is wrong")
	}
	if got := StateLabel(tg); got != "byte-path" {
		t.Errorf("StateLabel = %q, want %q", got, "byte-path")
	}
	if rep.Level() != LevelWarn {
		t.Errorf("report level = %v, want amber", rep.Level())
	}
}

// TestBuildNodeBytePathHealthy guards the two ways this must not fire: a
// byte-path that resolves, and a node that legitimately has none because no
// volume syncs to it. A false byte-path alarm on every pull-only peer would
// make the amber meaningless.
func TestBuildNodeBytePathHealthy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		syncTo []string
	}{
		{"resolves to a directory", t.TempDir(), []string{"nas"}},
		{"pull-only peer carries no byte-path", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupStore(t)
			ctx := context.Background()
			root, _ := indexTree(t, s, "media")
			cfg := cfgFor("media", root, tc.syncTo, []string{"nas"}, nil, []string{"nas"})
			cfg.Nodes["nas"].Path = tc.path

			rep, err := Build(ctx, s, cfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if tg := rep.Volumes[0].Targets[0]; tg.Standing == StandingBytePath {
				t.Errorf("standing = byte-path (%s), want no byte-path complaint", tg.StandingDetail)
			}
		})
	}
}

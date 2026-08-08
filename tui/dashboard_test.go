package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/status"
	"github.com/mbertschler/squirrel/store"
)

// openTestStore opens a fresh on-disk SQLite store for one test. Using a
// file rather than ":memory:" keeps the migrations honest (some early
// versions of the modernc driver behaved differently against shared-cache
// memory DBs) and matches what the production code does.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "tui.db")
	s, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestLoadDashboardDataPartitionsRuns(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	vol, err := s.GetOrCreateVolume(ctx, "/srv/photos")
	if err != nil {
		t.Fatalf("GetOrCreateVolume: %v", err)
	}

	// Two finished index runs, an active sync run, a failed audit run, and
	// a successful sync run. We expect:
	//   - activeRuns: the one running sync
	//   - recentRuns: every terminal run, newest first, capped at 10
	finishedIdx1, _ := s.BeginIndexRun(ctx, store.RunKindIndex, vol.ID, false)
	if err := s.FinishRun(ctx, finishedIdx1, store.RunStatusSuccess, "", 100); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	finishedIdx2, _ := s.BeginIndexRun(ctx, store.RunKindIndex, vol.ID, false)
	if err := s.FinishRun(ctx, finishedIdx2, store.RunStatusPartial, "", 105); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	activeSync, _ := s.BeginRun(ctx, store.RunKindSync, vol.ID, "backup", false)
	// leave running
	failedAudit, _ := s.BeginIndexRun(ctx, store.RunKindAudit, vol.ID, false)
	if err := s.FinishRun(ctx, failedAudit, store.RunStatusFailed, "boom", 0); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	successSync, _ := s.BeginRun(ctx, store.RunKindSync, vol.ID, "backup", false)
	if err := s.FinishRun(ctx, successSync, store.RunStatusSuccess, "", 50); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	// cfg is nil here: this test pins the run-partitioning behaviour, which
	// is independent of the config-driven coverage grid (that is exercised
	// against the shared status layer in the status package's own tests).
	data, err := loadDashboardData(ctx, s, nil, false)
	if err != nil {
		t.Fatalf("loadDashboardData: %v", err)
	}

	if len(data.volumes) != 1 || data.volumes[0].ID != vol.ID {
		t.Errorf("volumes: got %+v, want one entry for vol#%d", data.volumes, vol.ID)
	}

	if len(data.activeRuns) != 1 || data.activeRuns[0].ID != activeSync {
		t.Errorf("activeRuns: got %+v, want [%d]", data.activeRuns, activeSync)
	}

	// recentRuns excludes the running sync. We don't pin exact order
	// across all four terminal runs but the most recent (successSync)
	// must come first.
	if len(data.recentRuns) != 4 {
		t.Errorf("recentRuns count = %d, want 4", len(data.recentRuns))
	}
	if data.recentRuns[0].ID != successSync {
		t.Errorf("recentRuns[0] = %d, want %d (most recent)", data.recentRuns[0].ID, successSync)
	}

	// With no config, the coverage grid renders nothing.
	if len(data.coverage.Volumes) != 0 {
		t.Errorf("coverage.Volumes = %d, want 0 without config", len(data.coverage.Volumes))
	}
}

// TestRenderConfigDrift: the dashboard grows a "Config drift" panel while
// the latch stands (F9) and stays clean when it does not.
func TestRenderConfigDrift(t *testing.T) {
	m := &dashboardModel{}
	if got := m.renderConfigDrift(); got != "" {
		t.Errorf("renderConfigDrift with nothing latched = %q, want empty", got)
	}
	m.data.coverage.ConfigDrift = &status.ConfigDrift{
		Path:  "/etc/squirrel/config.toml",
		Since: 90 * time.Minute,
	}
	got := m.renderConfigDrift()
	if !strings.Contains(got, "Config drift") ||
		!strings.Contains(got, "restart to apply") ||
		!strings.Contains(got, "/etc/squirrel/config.toml") {
		t.Errorf("panel = %q, want the header, the restart sentence, and the path", got)
	}
}

// TestRenderVolumeFleet: the fleet block rides under the volume's target
// grid, naming every other place the volume lives with the same words
// `squirrel status` prints — including the peer that pushes here, which no
// target row mentions.
func TestRenderVolumeFleet(t *testing.T) {
	if got := renderVolumeFleet(nil); got != "" {
		t.Errorf("fleet block for a volume that lives nowhere else = %q, want none", got)
	}
	fresh := 4 * time.Minute
	places := []status.FleetPlace{
		{
			Name: "nas", Kind: status.KindNode, SyncTarget: true, State: status.FleetSame,
			MissingKnown: true, LastChangeAgo: &fresh, LastVerifiedAgo: &fresh, AsOfAgo: &fresh,
			Level: status.LevelOK,
		},
		{Name: "laptop", Kind: status.KindNode, Level: status.LevelNeutral},
	}
	got := renderVolumeFleet(places)
	for _, want := range []string{"FLEET", "AS OF", "nas", "same", "laptop", "unknown", "4m ago"} {
		if !strings.Contains(got, want) {
			t.Errorf("fleet block missing %q:\n%s", want, got)
		}
	}
}

// TestRenderVolumeCoverageKeepsFleetWithoutTargets: a hub's copy of a
// volume an edge machine pushes to it declares no target at all, and the
// fleet block is the only thing that can say where that volume lives. It
// must survive the no-targets path.
func TestRenderVolumeCoverageKeepsFleetWithoutTargets(t *testing.T) {
	m := &dashboardModel{}
	got := m.renderVolumeCoverage(status.VolumeStatus{
		Name:  "photos",
		Fleet: []status.FleetPlace{{Name: "laptop", Kind: status.KindNode}},
	})
	if !strings.Contains(got, "no targets configured") || !strings.Contains(got, "laptop") {
		t.Errorf("block = %q, want the no-targets note and the fleet row", got)
	}
}

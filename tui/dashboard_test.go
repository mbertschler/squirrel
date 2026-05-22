package tui

import (
	"context"
	"path/filepath"
	"testing"

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
	//   - latestByVol[vol.ID]: index = most recent success, sync = the
	//     successful one (failed audit doesn't establish "last audit")
	finishedIdx1, _ := s.BeginRun(ctx, store.RunKindIndex, vol.ID, "")
	if err := s.FinishRun(ctx, finishedIdx1, store.RunStatusSuccess, "", 100); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	finishedIdx2, _ := s.BeginRun(ctx, store.RunKindIndex, vol.ID, "")
	if err := s.FinishRun(ctx, finishedIdx2, store.RunStatusPartial, "", 105); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	activeSync, _ := s.BeginRun(ctx, store.RunKindSync, vol.ID, "backup")
	// leave running
	failedAudit, _ := s.BeginRun(ctx, store.RunKindAudit, vol.ID, "")
	if err := s.FinishRun(ctx, failedAudit, store.RunStatusFailed, "boom", 0); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	successSync, _ := s.BeginRun(ctx, store.RunKindSync, vol.ID, "backup")
	if err := s.FinishRun(ctx, successSync, store.RunStatusSuccess, "", 50); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	data, err := loadDashboardData(ctx, s)
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

	byKind := data.latestByVol[vol.ID]
	if byKind == nil {
		t.Fatalf("latestByVol missing entry for vol#%d", vol.ID)
	}
	if byKind[store.RunKindIndex].ID != finishedIdx2 {
		t.Errorf("latest index = %d, want %d (the partial-but-newer one)",
			byKind[store.RunKindIndex].ID, finishedIdx2)
	}
	if byKind[store.RunKindSync].ID != successSync {
		t.Errorf("latest sync = %d, want %d", byKind[store.RunKindSync].ID, successSync)
	}
	if _, present := byKind[store.RunKindAudit]; present {
		t.Errorf("latestByVol must not include the failed audit run")
	}
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// TestUpsertPeerSyncStateWritesHistory: every successful upsert appends
// one peer_sync_state_history row, oldest first, alongside advancing the
// live watermark. This is the H6 append-only trail — the live row only
// ever shows the current value, the history preserves the chain.
func TestUpsertPeerSyncStateWritesHistory(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	peer, err := s.GetOrCreatePeerNode(ctx, "nas", "http://nas.example")
	if err != nil {
		t.Fatalf("GetOrCreatePeerNode: %v", err)
	}

	for _, wm := range []int64{7, 20, 42} {
		if err := s.UpsertPeerSyncState(ctx, vID, peer.ID, wm, false); err != nil {
			t.Fatalf("UpsertPeerSyncState(%d): %v", wm, err)
		}
	}

	history, err := s.ListPeerSyncStateHistory(ctx, vID, peer.ID)
	if err != nil {
		t.Fatalf("ListPeerSyncStateHistory: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history rows = %d, want 3", len(history))
	}
	want := []int64{7, 20, 42}
	for i, h := range history {
		if !h.LastSharedRunID.Valid || h.LastSharedRunID.Int64 != want[i] {
			t.Fatalf("history[%d] watermark = %+v, want %d", i, h.LastSharedRunID, want[i])
		}
		if h.VolumeID != vID || h.PeerNodeID != peer.ID {
			t.Fatalf("history[%d] pair = (%d,%d), want (%d,%d)", i, h.VolumeID, h.PeerNodeID, vID, peer.ID)
		}
	}
	// Live row reflects only the final advance.
	state, err := s.GetPeerSyncState(ctx, vID, peer.ID)
	if err != nil {
		t.Fatalf("GetPeerSyncState: %v", err)
	}
	if state.LastSharedRunID.Int64 != 42 {
		t.Fatalf("live watermark = %d, want 42", state.LastSharedRunID.Int64)
	}
}

// TestUpsertPeerSyncStateRefusesRewind: a watermark move below the
// current value is refused by default with a *WatermarkRewindError
// (wrapping ErrWatermarkRewind), the live row is left untouched, and no
// history row is appended for the rejected move.
func TestUpsertPeerSyncStateRefusesRewind(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	peer, _ := s.GetOrCreatePeerNode(ctx, "nas", "http://nas.example")

	if err := s.UpsertPeerSyncState(ctx, vID, peer.ID, 42, false); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	err := s.UpsertPeerSyncState(ctx, vID, peer.ID, 10, false)
	if !errors.Is(err, ErrWatermarkRewind) {
		t.Fatalf("rewind err = %v, want ErrWatermarkRewind", err)
	}
	var rewErr *WatermarkRewindError
	if !errors.As(err, &rewErr) {
		t.Fatalf("err = %v, want *WatermarkRewindError", err)
	}
	if rewErr.Current != 42 || rewErr.Attempted != 10 {
		t.Fatalf("rewind err = %+v, want current=42 attempted=10", rewErr)
	}

	// Live row unchanged; history holds only the seed advance.
	state, _ := s.GetPeerSyncState(ctx, vID, peer.ID)
	if state.LastSharedRunID.Int64 != 42 {
		t.Fatalf("live watermark = %d, want 42 (rewind must not apply)", state.LastSharedRunID.Int64)
	}
	history, _ := s.ListPeerSyncStateHistory(ctx, vID, peer.ID)
	if len(history) != 1 {
		t.Fatalf("history rows = %d, want 1 (rejected move appends nothing)", len(history))
	}
}

// TestUpsertPeerSyncStateAllowRewind: the same backwards move succeeds
// when allowRewind is set — the genuine-recovery opt-in — moving the live
// row and appending a history row.
func TestUpsertPeerSyncStateAllowRewind(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	peer, _ := s.GetOrCreatePeerNode(ctx, "nas", "http://nas.example")

	if err := s.UpsertPeerSyncState(ctx, vID, peer.ID, 42, false); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}
	if err := s.UpsertPeerSyncState(ctx, vID, peer.ID, 10, true); err != nil {
		t.Fatalf("UpsertPeerSyncState allowRewind: %v", err)
	}

	state, _ := s.GetPeerSyncState(ctx, vID, peer.ID)
	if state.LastSharedRunID.Int64 != 10 {
		t.Fatalf("live watermark = %d, want 10 after allowed rewind", state.LastSharedRunID.Int64)
	}
	history, _ := s.ListPeerSyncStateHistory(ctx, vID, peer.ID)
	if len(history) != 2 {
		t.Fatalf("history rows = %d, want 2", len(history))
	}
}

// TestSetCorrelatedRunIDWritesAudit: stamping a correlated id appends a
// 'set-correlated-run-id' runs_audit row whose note carries the old→new
// transition (the prior NULL rendered as "none"), preserving an
// append-only trail of the overwrite-in-place column (H6).
func TestSetCorrelatedRunIDWritesAudit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	peer, _ := s.GetOrCreatePeerNode(ctx, "nas", "http://nas.example")
	// Use the initiator's real path: BeginSyncRunIfClear leaves
	// correlated_run_id NULL, so the first stamp transitions none->value.
	runID, blocker, err := s.BeginSyncRunIfClear(ctx, SyncRunSpec{
		VolumeID:    vID,
		Destination: "nas",
		PeerNodeID:  sql.NullInt64{Int64: peer.ID, Valid: true},
	})
	if err != nil || blocker != nil {
		t.Fatalf("BeginSyncRunIfClear: err=%v blocker=%+v", err, blocker)
	}

	if err := s.SetCorrelatedRunID(ctx, runID, 1234); err != nil {
		t.Fatalf("SetCorrelatedRunID: %v", err)
	}
	if err := s.SetCorrelatedRunID(ctx, runID, 5678); err != nil {
		t.Fatalf("SetCorrelatedRunID second: %v", err)
	}

	audit, err := s.ListRunAudit(ctx, runID)
	if err != nil {
		t.Fatalf("ListRunAudit: %v", err)
	}
	if len(audit) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(audit))
	}
	for _, a := range audit {
		if a.Transition != TransitionSetCorrelatedRunID {
			t.Fatalf("transition = %q, want %q", a.Transition, TransitionSetCorrelatedRunID)
		}
		if a.Operator.Valid {
			t.Fatalf("operator = %+v, want NULL for machine-driven write", a.Operator)
		}
	}
	if !audit[0].Note.Valid || audit[0].Note.String != "none->1234" {
		t.Fatalf("first note = %+v, want %q", audit[0].Note, "none->1234")
	}
	if audit[1].Note.String != "1234->5678" {
		t.Fatalf("second note = %q, want %q", audit[1].Note.String, "1234->5678")
	}
	run, _ := s.GetRun(ctx, runID)
	if run.CorrelatedRunID.Int64 != 5678 {
		t.Fatalf("live correlated id = %d, want 5678", run.CorrelatedRunID.Int64)
	}
}

// TestSetCorrelatedRunIDUnknownRun: an invalid run id is refused and
// appends no audit row.
func TestSetCorrelatedRunIDUnknownRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.SetCorrelatedRunID(ctx, 99999, 1); err == nil {
		t.Fatalf("expected no-such-run error")
	}
}

// TestMigrateV11ToV12CreatesPeerSyncHistory seeds a v11-shape database by
// hand (the schema the v12 migration builds on: volumes, nodes,
// peer_sync_state, runs_audit, schema_version=11), opens it to trigger
// the v11→v12 step, and asserts the peer_sync_state_history table exists
// and accepts an upsert-driven insert.
func TestMigrateV11ToV12CreatesPeerSyncHistory(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	for _, q := range v11SchemaDDL() {
		if _, err := rawDB.Exec(q); err != nil {
			t.Fatalf("v11 DDL %q: %v", q, err)
		}
	}
	rawDB.Close()

	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open (migrates v11→v12): %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if v, _ := s.CurrentSchemaVersion(ctx); v != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", v, SchemaVersion)
	}
	// The migrated DB carries peer_sync_state_history; an upsert against
	// the seeded (volume, peer) writes both the live row and a history row.
	if err := s.UpsertPeerSyncState(ctx, 1, 2, 9, false); err != nil {
		t.Fatalf("UpsertPeerSyncState after migration: %v", err)
	}
	history, err := s.ListPeerSyncStateHistory(ctx, 1, 2)
	if err != nil {
		t.Fatalf("ListPeerSyncStateHistory after migration: %v", err)
	}
	if len(history) != 1 || history[0].LastSharedRunID.Int64 != 9 {
		t.Fatalf("post-migration history = %+v, want one row watermark=9", history)
	}
}

// v11SchemaDDL returns the minimal v11-shape DDL the v12 migration needs:
// the FK targets (volumes, nodes) for peer_sync_state_history, plus a
// self-row peer so the upsert's node FK resolves, and the prior tables
// the open path expects to find. Only the columns the migration and the
// post-migration insert touch are modelled.
func v11SchemaDDL() []string {
	return []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE nodes (
			id   INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			endpoint TEXT,
			public_key_fingerprint TEXT
		)`,
		`CREATE TABLE peer_sync_state (
			volume_id          INTEGER NOT NULL REFERENCES volumes(id),
			peer_node_id       INTEGER NOT NULL REFERENCES nodes(id),
			last_shared_run_id INTEGER,
			last_synced_at     INTEGER NOT NULL,
			PRIMARY KEY (volume_id, peer_node_id)
		)`,
		`INSERT INTO schema_version (version) VALUES (11)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'v', '/v')`,
		`INSERT INTO nodes (id, name, endpoint) VALUES (2, 'nas', 'http://nas.example')`,
	}
}

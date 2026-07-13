package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// TestMigrateV18ToV19AddsVerifyMethod builds a minimal v18 database with
// a pre-existing durability component and confirms the migration adds
// verify_method (NULL on the carried-over row) without disturbing the
// recorded coordinate.
func TestMigrateV18ToV19AddsVerifyMethod(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	v18DDL := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, endpoint TEXT, public_key_fingerprint TEXT)`,
		// contents exists from v14 on; a real v18 DB carries it, and the
		// v20→v21 triggers attach to it, so the minimal fixture must too.
		`CREATE TABLE contents (
			id             INTEGER PRIMARY KEY,
			blake3         BLOB NOT NULL UNIQUE CHECK (length(blake3) = 32),
			size_bytes     INTEGER NOT NULL,
			origin_node_id INTEGER REFERENCES nodes(id),
			origin_run_id  INTEGER
		)`,
		`CREATE TABLE destination_run_ids (
			volume_id      INTEGER NOT NULL REFERENCES volumes(id),
			destination    TEXT NOT NULL,
			origin_node_id INTEGER NOT NULL REFERENCES nodes(id),
			origin_run_id  INTEGER NOT NULL,
			updated_at_ns  INTEGER NOT NULL,
			PRIMARY KEY (volume_id, destination, origin_node_id)
		)`,
		`CREATE TABLE destination_run_ids_history (
			id             INTEGER PRIMARY KEY,
			volume_id      INTEGER NOT NULL,
			destination    TEXT NOT NULL,
			origin_node_id INTEGER NOT NULL,
			origin_run_id  INTEGER NOT NULL,
			at_ns          INTEGER NOT NULL
		)`,
		`INSERT INTO schema_version (version) VALUES (18)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'v', '/v')`,
		`INSERT INTO nodes (id, name) VALUES (1, 'self')`,
		`INSERT INTO destination_run_ids (volume_id, destination, origin_node_id, origin_run_id, updated_at_ns)
			VALUES (1, 'bucket', 1, 7, 100)`,
	}
	for _, q := range v18DDL {
		if _, err := rawDB.Exec(q); err != nil {
			t.Fatalf("v18 DDL %q: %v", q, err)
		}
	}
	rawDB.Close()

	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open (migrates v18→v19): %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	if v, _ := s.CurrentSchemaVersion(ctx); v != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", v, SchemaVersion)
	}
	got, err := s.GetDestinationRunID(ctx, 1, "bucket", 1)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.OriginRunID != 7 {
		t.Fatalf("origin run = %d, want 7 (carried over)", got.OriginRunID)
	}
	if got.VerifyMethod != "" {
		t.Fatalf("verify method = %q, want empty (NULL backfill)", got.VerifyMethod)
	}
}

// TestUpsertDestinationRunIDWritesHistory: every successful advance
// appends one destination_run_ids_history row alongside updating the
// live vector component — the same append-only contract the peer-sync
// watermark has.
func TestUpsertDestinationRunIDWritesHistory(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	node, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}

	for _, run := range []int64{7, 20, 42} {
		if err := s.UpsertDestinationRunID(ctx, vID, "bucket-a", node.ID, run, false); err != nil {
			t.Fatalf("UpsertDestinationRunID(%d): %v", run, err)
		}
	}

	history, err := s.ListDestinationRunIDHistory(ctx, vID, "bucket-a")
	if err != nil {
		t.Fatalf("ListDestinationRunIDHistory: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history rows = %d, want 3", len(history))
	}
	want := []int64{7, 20, 42}
	for i, h := range history {
		if h.OriginRunID != want[i] {
			t.Fatalf("history[%d] origin run = %d, want %d", i, h.OriginRunID, want[i])
		}
		if h.VolumeID != vID || h.Destination != "bucket-a" || h.OriginNodeID != node.ID {
			t.Fatalf("history[%d] key = (%d,%q,%d), want (%d,%q,%d)",
				i, h.VolumeID, h.Destination, h.OriginNodeID, vID, "bucket-a", node.ID)
		}
	}

	got, err := s.GetDestinationRunID(ctx, vID, "bucket-a", node.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.OriginRunID != 42 {
		t.Fatalf("live origin run = %d, want 42", got.OriginRunID)
	}
}

// TestUpsertDestinationRunIDRefusesRewind: a component move below the
// recorded value is refused by default with a *DestinationRewindError
// (wrapping the shared ErrWatermarkRewind), the live row is left
// untouched, and no history row is appended for the rejected move.
// allowRewind overrides for genuine recovery and is logged to history.
func TestUpsertDestinationRunIDRefusesRewind(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	node, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}

	if err := s.UpsertDestinationRunID(ctx, vID, "bucket-a", node.ID, 40, false); err != nil {
		t.Fatalf("seed advance: %v", err)
	}

	err = s.UpsertDestinationRunID(ctx, vID, "bucket-a", node.ID, 3, false)
	if !errors.Is(err, ErrWatermarkRewind) {
		t.Fatalf("rewind err = %v, want ErrWatermarkRewind", err)
	}
	var rewErr *DestinationRewindError
	if !errors.As(err, &rewErr) {
		t.Fatalf("err = %v, want *DestinationRewindError", err)
	}
	if rewErr.Current != 40 || rewErr.Attempted != 3 || rewErr.Destination != "bucket-a" {
		t.Fatalf("rewind detail = %+v, want current=40 attempted=3 destination=bucket-a", rewErr)
	}

	live, err := s.GetDestinationRunID(ctx, vID, "bucket-a", node.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID after refusal: %v", err)
	}
	if live.OriginRunID != 40 {
		t.Fatalf("live origin run = %d after refused rewind, want 40", live.OriginRunID)
	}
	history, err := s.ListDestinationRunIDHistory(ctx, vID, "bucket-a")
	if err != nil {
		t.Fatalf("ListDestinationRunIDHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("history rows = %d after refused rewind, want 1", len(history))
	}

	if err := s.UpsertDestinationRunID(ctx, vID, "bucket-a", node.ID, 3, true); err != nil {
		t.Fatalf("allowRewind override: %v", err)
	}
	live, _ = s.GetDestinationRunID(ctx, vID, "bucket-a", node.ID)
	if live.OriginRunID != 3 {
		t.Fatalf("live origin run = %d after override, want 3", live.OriginRunID)
	}
	history, _ = s.ListDestinationRunIDHistory(ctx, vID, "bucket-a")
	if len(history) != 2 {
		t.Fatalf("history rows = %d after override, want 2 (override is logged)", len(history))
	}
}

// TestDestinationRunIDVector: the vector for one destination carries
// one independent component per origin node, scoped per destination
// and per volume.
func TestDestinationRunIDVector(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	peer, err := s.CreateNode(ctx, "peer", "https://peer.example")
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	advances := []struct {
		destination string
		nodeID      int64
		run         int64
	}{
		{"bucket-a", self.ID, 10},
		{"bucket-a", peer.ID, 4},
		{"bucket-b", self.ID, 2},
	}
	for _, a := range advances {
		if err := s.UpsertDestinationRunID(ctx, vID, a.destination, a.nodeID, a.run, false); err != nil {
			t.Fatalf("advance %+v: %v", a, err)
		}
	}

	vector, err := s.ListDestinationRunIDs(ctx, vID, "bucket-a")
	if err != nil {
		t.Fatalf("ListDestinationRunIDs: %v", err)
	}
	if len(vector) != 2 {
		t.Fatalf("vector components = %d, want 2", len(vector))
	}
	byNode := map[int64]int64{}
	for _, c := range vector {
		byNode[c.OriginNodeID] = c.OriginRunID
	}
	if byNode[self.ID] != 10 || byNode[peer.ID] != 4 {
		t.Fatalf("vector = %+v, want self→10 peer→4", byNode)
	}

	// bucket-b's component for self is independent of bucket-a's.
	got, err := s.GetDestinationRunID(ctx, vID, "bucket-b", self.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID bucket-b: %v", err)
	}
	if got.OriginRunID != 2 {
		t.Fatalf("bucket-b self component = %d, want 2", got.OriginRunID)
	}
}

// TestUpsertDestinationRunIDRejectsEmptyDestination: the destination is
// the vector's identity, so it must be non-empty.
func TestUpsertDestinationRunIDRejectsEmptyDestination(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	node, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	if err := s.UpsertDestinationRunID(ctx, vID, "", node.ID, 1, false); err == nil {
		t.Fatalf("empty destination accepted, want error")
	}
}

// advanceFromPresentSet snapshots the volume's present-set origin maxima
// and advances the destination's vector to exactly that snapshot, the
// snapshot-pinned path every handler drives. Tests use it to exercise the
// PresentOriginMaxima → AdvanceDestinationVectorTo pair the way production
// does.
func advanceFromPresentSet(t *testing.T, s *Store, volumeID int64, destination string) {
	t.Helper()
	ctx := context.Background()
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	components, err := s.PresentOriginMaxima(ctx, volumeID, self.ID)
	if err != nil {
		t.Fatalf("PresentOriginMaxima: %v", err)
	}
	if err := s.AdvanceDestinationVectorTo(ctx, volumeID, destination, VerifyMethodPeer, components); err != nil {
		t.Fatalf("AdvanceDestinationVectorTo: %v", err)
	}
}

// TestAdvanceDestinationVector: the advance computes one component per
// origin node over the volume's present rows — locally-introduced
// content under the self node at its introduction run (the content's
// earliest first_seen, so a duplicate path observed later doesn't move
// the coordinate), forwarded content under its recorded origin
// verbatim — and excludes non-present rows and the reserved sync
// subtrees.
func TestAdvanceDestinationVector(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run1 := makeRun(t, s, vID)
	run2 := makeRun(t, s, vID)
	run3 := makeRun(t, s, vID)
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	ext, err := s.CreateNode(ctx, "ext", "peer://ext")
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	upsert := func(path string, b byte, status string, firstSeen int64, prov *Provenance) {
		t.Helper()
		if err := s.Upsert(ctx, FileRow{
			VolumeID: vID, Path: path, Blake3: digest(b),
			SizeBytes: 1, MtimeNs: 1, Status: status,
			FirstSeenRunID: firstSeen, LastSeenRunID: firstSeen, IndexedAtNs: 1,
		}, prov); err != nil {
			t.Fatalf("Upsert %s: %v", path, err)
		}
	}
	upsert("a.txt", 0xA1, StatusPresent, run1, nil)
	upsert("b.txt", 0xA2, StatusPresent, run2, nil)
	upsert("c.txt", 0xA3, StatusPresent, run1, &Provenance{NodeID: ext.ID, RunID: 50})
	// A duplicate path of a.txt's content first seen at run3: the
	// content's introduction run stays run1 — the coordinate the sender
	// materialises on the wire — so the self component must stay at
	// run2 (b.txt's introduction).
	upsert("a-dup.txt", 0xA1, StatusPresent, run3, nil)
	// Non-present and reserved-subtree rows must not advance anything:
	// gone.txt would push the self component to run3, and the conflict
	// leftover would push ext to 999.
	upsert("gone.txt", 0xA4, StatusMissing, run3, nil)
	upsert(".squirrel-conflicts/run-1/x.bin", 0xA5, StatusPresent, run3, &Provenance{NodeID: ext.ID, RunID: 999})

	advanceFromPresentSet(t, s, vID, "nas")
	vector, err := s.ListDestinationRunIDs(ctx, vID, "nas")
	if err != nil {
		t.Fatalf("ListDestinationRunIDs: %v", err)
	}
	byNode := map[int64]int64{}
	for _, c := range vector {
		byNode[c.OriginNodeID] = c.OriginRunID
	}
	if len(byNode) != 2 || byNode[self.ID] != run2 || byNode[ext.ID] != 50 {
		t.Fatalf("vector = %+v, want self→%d ext→50", byNode, run2)
	}
}

// TestAdvanceDestinationVectorKeepsHigherComponent: a recorded
// component above the computed present-set maximum stays in place (the
// destination is append-only, so the higher floor still holds) and the
// advance reports no error — componentwise max, not a rewind.
func TestAdvanceDestinationVectorKeepsHigherComponent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run1 := makeRun(t, s, vID)
	ext, err := s.CreateNode(ctx, "ext", "peer://ext")
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "c.txt", Blake3: digest(0xB1),
		SizeBytes: 1, MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: run1, LastSeenRunID: run1, IndexedAtNs: 1,
	}, &Provenance{NodeID: ext.ID, RunID: 50}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.UpsertDestinationRunID(ctx, vID, "nas", ext.ID, 60, false); err != nil {
		t.Fatalf("seed component: %v", err)
	}

	advanceFromPresentSet(t, s, vID, "nas")
	got, err := s.GetDestinationRunID(ctx, vID, "nas", ext.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.OriginRunID != 60 {
		t.Fatalf("ext component = %d, want 60 (higher recorded floor kept)", got.OriginRunID)
	}
}

// TestAdvanceDestinationVectorToPeerSnapshotPinned proves the peer-path
// advance covers only the captured snapshot: a row that becomes present
// between snapshot capture and the advance is not folded in. The advance
// is fed the snapshot taken before the row existed, tagged peer-blake3,
// so the later row's higher origin run never reaches the vector.
func TestAdvanceDestinationVectorToPeerSnapshotPinned(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run1 := makeRun(t, s, vID)
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}

	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "a.txt", Blake3: digest(0xC1),
		SizeBytes: 1, MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: run1, LastSeenRunID: run1, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert a.txt: %v", err)
	}

	// Snapshot captured before the second row exists — the peer driver
	// takes this before the transfer.
	snapshot, err := s.PresentOriginMaxima(ctx, vID, self.ID)
	if err != nil {
		t.Fatalf("PresentOriginMaxima: %v", err)
	}

	// A row committed mid-transfer with a strictly higher introduction run.
	run2 := makeRun(t, s, vID)
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "b.txt", Blake3: digest(0xC2),
		SizeBytes: 1, MtimeNs: 1, Status: StatusPresent,
		FirstSeenRunID: run2, LastSeenRunID: run2, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert b.txt: %v", err)
	}

	if err := s.AdvanceDestinationVectorTo(ctx, vID, "nas", VerifyMethodPeer, snapshot); err != nil {
		t.Fatalf("AdvanceDestinationVectorTo: %v", err)
	}
	got, err := s.GetDestinationRunID(ctx, vID, "nas", self.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.OriginRunID != run1 {
		t.Fatalf("self component = %d, want run1 %d (the mid-transfer row at run2 %d must not be covered)", got.OriginRunID, run1, run2)
	}
	if got.VerifyMethod != VerifyMethodPeer {
		t.Fatalf("verify method = %q, want %q", got.VerifyMethod, VerifyMethodPeer)
	}
}

// TestListVolumeDestinationRunIDs returns components across every
// destination of the volume, ordered by destination then origin node.
func TestListVolumeDestinationRunIDs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	otherVol := makeVolume(t, s, "/other")
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	for _, a := range []struct {
		volID int64
		dest  string
		run   int64
	}{
		{vID, "bucket-b", 7},
		{vID, "bucket-a", 3},
		{otherVol, "bucket-a", 99},
	} {
		if err := s.UpsertDestinationRunID(ctx, a.volID, a.dest, self.ID, a.run, false); err != nil {
			t.Fatalf("seed %+v: %v", a, err)
		}
	}

	rows, err := s.ListVolumeDestinationRunIDs(ctx, vID)
	if err != nil {
		t.Fatalf("ListVolumeDestinationRunIDs: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (other volume excluded)", len(rows))
	}
	if rows[0].Destination != "bucket-a" || rows[0].OriginRunID != 3 ||
		rows[1].Destination != "bucket-b" || rows[1].OriginRunID != 7 {
		t.Fatalf("rows = %+v, want bucket-a→3 then bucket-b→7", rows)
	}
}

// TestAdvanceDestinationVectorToSnapshot is the #103 fix: the advance
// reflects exactly the captured enumeration snapshot, not the live
// present set re-read after a transfer. A content row inserted between
// the snapshot and the advance is NOT claimed durable.
func TestAdvanceDestinationVectorToSnapshot(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run1 := makeRun(t, s, vID)
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}

	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "a.txt", Blake3: digest(0xA1), SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: run1, LastSeenRunID: run1, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert a.txt: %v", err)
	}

	// Snapshot captured here, before a second row lands.
	snapshot, err := s.PresentOriginMaxima(ctx, vID, self.ID)
	if err != nil {
		t.Fatalf("PresentOriginMaxima: %v", err)
	}

	// A row committed after the snapshot (a mid-push index) advances the
	// live present set to run2 — but the snapshot still reads run1.
	run2 := makeRun(t, s, vID)
	if err := s.Upsert(ctx, FileRow{
		VolumeID: vID, Path: "b.txt", Blake3: digest(0xA2), SizeBytes: 1, MtimeNs: 1,
		Status: StatusPresent, FirstSeenRunID: run2, LastSeenRunID: run2, IndexedAtNs: 1,
	}, nil); err != nil {
		t.Fatalf("Upsert b.txt: %v", err)
	}

	if err := s.AdvanceDestinationVectorTo(ctx, vID, "nas", VerifyMethodBlake3, snapshot); err != nil {
		t.Fatalf("AdvanceDestinationVectorTo: %v", err)
	}
	got, err := s.GetDestinationRunID(ctx, vID, "nas", self.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.OriginRunID != run1 {
		t.Fatalf("self component = %d, want %d (snapshot, not the live run2)", got.OriginRunID, run1)
	}
	if got.VerifyMethod != VerifyMethodBlake3 {
		t.Fatalf("verify method = %q, want %q", got.VerifyMethod, VerifyMethodBlake3)
	}
}

// TestUpsertDestinationRunIDRecordsMethod: the verified entry point
// records the method on the live row and in history; the legacy entry
// point records none.
func TestUpsertDestinationRunIDRecordsMethod(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	node, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}

	if err := s.UpsertDestinationRunIDVerified(ctx, vID, "bucket", node.ID, 5, VerifyMethodKopia, false); err != nil {
		t.Fatalf("UpsertDestinationRunIDVerified: %v", err)
	}
	got, err := s.GetDestinationRunID(ctx, vID, "bucket", node.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.VerifyMethod != VerifyMethodKopia {
		t.Fatalf("verify method = %q, want %q", got.VerifyMethod, VerifyMethodKopia)
	}
	hist, err := s.ListDestinationRunIDHistory(ctx, vID, "bucket")
	if err != nil {
		t.Fatalf("ListDestinationRunIDHistory: %v", err)
	}
	if len(hist) != 1 || hist[0].VerifyMethod != VerifyMethodKopia {
		t.Fatalf("history = %+v, want one row with method %q", hist, VerifyMethodKopia)
	}

	if err := s.UpsertDestinationRunID(ctx, vID, "bucket2", node.ID, 5, false); err != nil {
		t.Fatalf("UpsertDestinationRunID: %v", err)
	}
	plain, err := s.GetDestinationRunID(ctx, vID, "bucket2", node.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if plain.VerifyMethod != "" {
		t.Fatalf("verify method = %q, want empty (no method recorded)", plain.VerifyMethod)
	}
}

// TestUpsertDestinationRunIDPreservesMethodOnMethodlessReconfirm: a
// methodless re-confirmation at the same origin run (e.g. a pull from a
// pre-v19 peer) must not degrade a recorded content-verified method to
// unknown — provenance is preserved when the run does not advance.
func TestUpsertDestinationRunIDPreservesMethodOnMethodlessReconfirm(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	node, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}

	if err := s.UpsertDestinationRunIDVerified(ctx, vID, "bucket", node.ID, 5, VerifyMethodBlake3, false); err != nil {
		t.Fatalf("seed verified: %v", err)
	}
	// Methodless re-confirm at the same run.
	if err := s.UpsertDestinationRunID(ctx, vID, "bucket", node.ID, 5, false); err != nil {
		t.Fatalf("methodless reconfirm: %v", err)
	}
	got, err := s.GetDestinationRunID(ctx, vID, "bucket", node.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.VerifyMethod != VerifyMethodBlake3 {
		t.Fatalf("verify method = %q, want %q preserved", got.VerifyMethod, VerifyMethodBlake3)
	}

	// A methodless advance to a strictly higher run clears the method —
	// the new coordinate is genuinely unverified.
	if err := s.UpsertDestinationRunID(ctx, vID, "bucket", node.ID, 9, false); err != nil {
		t.Fatalf("methodless advance: %v", err)
	}
	got, err = s.GetDestinationRunID(ctx, vID, "bucket", node.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.OriginRunID != 9 || got.VerifyMethod != "" {
		t.Fatalf("after methodless advance: run=%d method=%q, want 9 and empty", got.OriginRunID, got.VerifyMethod)
	}
}

// TestDestinationRunIDNullVerifyMethodReadsUnverified pins the v19
// backfill contract: a component with a NULL verify_method (a pre-v19
// row, or a legacy upsert) scans back as an empty method, which
// ContentVerifiedMethod treats as not content-verified — so the offload
// gate refuses such a component rather than over-claiming.
func TestDestinationRunIDNullVerifyMethodReadsUnverified(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	node, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO destination_run_ids (volume_id, destination, origin_node_id, origin_run_id, updated_at_ns, verify_method)
		VALUES (?, ?, ?, ?, ?, NULL)
	`, vID, "legacy", node.ID, 5, NowNs()); err != nil {
		t.Fatalf("insert NULL-method component: %v", err)
	}

	got, err := s.GetDestinationRunID(ctx, vID, "legacy", node.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.VerifyMethod != "" {
		t.Fatalf("verify method = %q, want empty for a NULL column", got.VerifyMethod)
	}
	if ContentVerifiedMethod(got.VerifyMethod) {
		t.Fatalf("a NULL/empty method must not count as content-verified")
	}
}

// TestContentVerifiedMethod pins which methods the offload gate accepts
// as genuine content verification.
func TestContentVerifiedMethod(t *testing.T) {
	verified := []string{VerifyMethodBlake3, VerifyMethodPeer, VerifyMethodKopia}
	for _, m := range verified {
		if !ContentVerifiedMethod(m) {
			t.Fatalf("method %q should be content-verified", m)
		}
	}
	for _, m := range []string{VerifyMethodPresenceSize, VerifyMethodSizeMtime, "", "bogus"} {
		if ContentVerifiedMethod(m) {
			t.Fatalf("method %q must not be content-verified", m)
		}
	}
}

// TestMigrateV21ToV22AddsSourceNodeID builds a minimal v21 database with a
// pre-existing durability component and confirms the migration adds
// source_node_id (NULL on the carried-over row — the locally-verified
// class) without disturbing the recorded coordinate or its method.
func TestMigrateV21ToV22AddsSourceNodeID(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	v21DDL := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`,
		`CREATE TABLE volumes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL)`,
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, endpoint TEXT, public_key_fingerprint TEXT)`,
		`CREATE TABLE contents (
			id             INTEGER PRIMARY KEY,
			blake3         BLOB NOT NULL UNIQUE CHECK (length(blake3) = 32),
			size_bytes     INTEGER NOT NULL,
			origin_node_id INTEGER REFERENCES nodes(id),
			origin_run_id  INTEGER
		)`,
		`CREATE TABLE destination_run_ids (
			volume_id      INTEGER NOT NULL REFERENCES volumes(id),
			destination    TEXT NOT NULL,
			origin_node_id INTEGER NOT NULL REFERENCES nodes(id),
			origin_run_id  INTEGER NOT NULL,
			updated_at_ns  INTEGER NOT NULL,
			verify_method  TEXT,
			PRIMARY KEY (volume_id, destination, origin_node_id)
		)`,
		`CREATE TABLE destination_run_ids_history (
			id             INTEGER PRIMARY KEY,
			volume_id      INTEGER NOT NULL,
			destination    TEXT NOT NULL,
			origin_node_id INTEGER NOT NULL,
			origin_run_id  INTEGER NOT NULL,
			at_ns          INTEGER NOT NULL,
			verify_method  TEXT
		)`,
		`INSERT INTO schema_version (version) VALUES (21)`,
		`INSERT INTO volumes (id, name, path) VALUES (1, 'v', '/v')`,
		`INSERT INTO nodes (id, name) VALUES (1, 'self')`,
		`INSERT INTO destination_run_ids (volume_id, destination, origin_node_id, origin_run_id, updated_at_ns, verify_method)
			VALUES (1, 'bucket', 1, 7, 100, 'blake3')`,
	}
	for _, q := range v21DDL {
		if _, err := rawDB.Exec(q); err != nil {
			t.Fatalf("v21 DDL %q: %v", q, err)
		}
	}
	rawDB.Close()

	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open (migrates v21→v22): %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	if v, _ := s.CurrentSchemaVersion(ctx); v != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", v, SchemaVersion)
	}
	got, err := s.GetDestinationRunID(ctx, 1, "bucket", 1)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.OriginRunID != 7 || got.VerifyMethod != VerifyMethodBlake3 {
		t.Fatalf("carried-over component = run %d method %q, want 7 / blake3", got.OriginRunID, got.VerifyMethod)
	}
	if got.SourceNodeID.Valid {
		t.Fatalf("source_node_id = %d, want NULL (locally-verified backfill)", got.SourceNodeID.Int64)
	}
}

// TestUpsertDestinationRunIDPulledTagsSource: a pulled advance records the
// asserting peer on the live row and in history, while a locally-verified
// advance for a different origin stays untagged (NULL). The two classes
// are distinguishable as the residual of #104 requires.
func TestUpsertDestinationRunIDPulledTagsSource(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	peer, err := s.GetOrCreateOriginNode(ctx, "nas")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode(nas): %v", err)
	}
	origin, err := s.GetOrCreateOriginNode(ctx, "laptop")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode(laptop): %v", err)
	}

	if err := s.UpsertDestinationRunIDVerified(ctx, vID, "offsite", self.ID, 9, VerifyMethodBlake3, false); err != nil {
		t.Fatalf("local verified advance: %v", err)
	}
	if err := s.UpsertDestinationRunIDPulled(ctx, vID, "offsite", origin.ID, 5, VerifyMethodKopia, peer.ID, false); err != nil {
		t.Fatalf("pulled advance: %v", err)
	}

	local, err := s.GetDestinationRunID(ctx, vID, "offsite", self.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID(local): %v", err)
	}
	if local.SourceNodeID.Valid {
		t.Fatalf("locally-verified component source = %d, want NULL", local.SourceNodeID.Int64)
	}
	pulled, err := s.GetDestinationRunID(ctx, vID, "offsite", origin.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID(pulled): %v", err)
	}
	if !pulled.SourceNodeID.Valid || pulled.SourceNodeID.Int64 != peer.ID {
		t.Fatalf("pulled component source = %+v, want peer %d", pulled.SourceNodeID, peer.ID)
	}

	history, err := s.ListDestinationRunIDHistory(ctx, vID, "offsite")
	if err != nil {
		t.Fatalf("ListDestinationRunIDHistory: %v", err)
	}
	bySource := map[int64]sql.NullInt64{}
	for _, h := range history {
		bySource[h.OriginNodeID] = h.SourceNodeID
	}
	if src := bySource[self.ID]; src.Valid {
		t.Fatalf("history for local advance carries source %d, want NULL", src.Int64)
	}
	if src := bySource[origin.ID]; !src.Valid || src.Int64 != peer.ID {
		t.Fatalf("history for pulled advance source = %+v, want peer %d", src, peer.ID)
	}
}

// TestUpsertDestinationRunIDProvenanceTransitions: a peer re-confirmation
// at the recorded run never downgrades a locally-verified (NULL)
// component to peer-asserted, and a local re-confirmation upgrades a
// peer-tagged component back to locally-verified — so a peer cannot
// launder local provenance away, and a verified push reclaims a pulled
// component.
func TestUpsertDestinationRunIDProvenanceTransitions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	peer, err := s.GetOrCreateOriginNode(ctx, "nas")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode(nas): %v", err)
	}

	if err := s.UpsertDestinationRunIDVerified(ctx, vID, "offsite", self.ID, 10, VerifyMethodBlake3, false); err != nil {
		t.Fatalf("local advance: %v", err)
	}
	if err := s.UpsertDestinationRunIDPulled(ctx, vID, "offsite", self.ID, 10, VerifyMethodBlake3, peer.ID, false); err != nil {
		t.Fatalf("peer re-confirm at recorded run: %v", err)
	}
	got, err := s.GetDestinationRunID(ctx, vID, "offsite", self.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID: %v", err)
	}
	if got.SourceNodeID.Valid {
		t.Fatalf("local component downgraded to peer %d by an equal-run re-confirm", got.SourceNodeID.Int64)
	}

	if err := s.UpsertDestinationRunIDPulled(ctx, vID, "offsite", self.ID, 20, VerifyMethodKopia, peer.ID, false); err != nil {
		t.Fatalf("peer strict advance: %v", err)
	}
	got, _ = s.GetDestinationRunID(ctx, vID, "offsite", self.ID)
	if !got.SourceNodeID.Valid || got.SourceNodeID.Int64 != peer.ID {
		t.Fatalf("after peer strict advance source = %+v, want peer %d", got.SourceNodeID, peer.ID)
	}

	if err := s.UpsertDestinationRunIDVerified(ctx, vID, "offsite", self.ID, 20, VerifyMethodBlake3, false); err != nil {
		t.Fatalf("local re-confirm at peer run: %v", err)
	}
	got, _ = s.GetDestinationRunID(ctx, vID, "offsite", self.ID)
	if got.SourceNodeID.Valid {
		t.Fatalf("local re-confirm did not reclaim provenance, source = %d", got.SourceNodeID.Int64)
	}
}

// TestUpsertDestinationRunIDMethodProvenanceStayTogether pins the
// invariant that a component's verify_method and source_node_id always
// describe the same write, so a peer's verification claim can never be
// recorded under local provenance (nor the reverse). The method CASE and
// the source CASE moved in lockstep would otherwise diverge at an
// equal-run write, laundering evidence across the trust boundary.
func TestUpsertDestinationRunIDMethodProvenanceStayTogether(t *testing.T) {
	ctx := context.Background()

	// A peer upgrading the method at the recorded run adopts the method
	// AND the peer tag together: the row must not read "blake3, locally
	// verified" when only a presence+size check was ever local.
	t.Run("peer method upgrade at equal run is tagged to the peer", func(t *testing.T) {
		s := openTestStore(t)
		vID := makeVolume(t, s, "/v")
		self, err := s.GetSelfNode(ctx)
		if err != nil {
			t.Fatalf("GetSelfNode: %v", err)
		}
		peer, err := s.GetOrCreateOriginNode(ctx, "nas")
		if err != nil {
			t.Fatalf("GetOrCreateOriginNode: %v", err)
		}
		if err := s.UpsertDestinationRunIDVerified(ctx, vID, "offsite", self.ID, 10, VerifyMethodPresenceSize, false); err != nil {
			t.Fatalf("local presence+size advance: %v", err)
		}
		if err := s.UpsertDestinationRunIDPulled(ctx, vID, "offsite", self.ID, 10, VerifyMethodBlake3, peer.ID, false); err != nil {
			t.Fatalf("peer blake3 re-confirm at equal run: %v", err)
		}
		got, err := s.GetDestinationRunID(ctx, vID, "offsite", self.ID)
		if err != nil {
			t.Fatalf("GetDestinationRunID: %v", err)
		}
		if got.VerifyMethod != VerifyMethodBlake3 {
			t.Fatalf("method = %q, want %q adopted from the peer", got.VerifyMethod, VerifyMethodBlake3)
		}
		if !got.SourceNodeID.Valid || got.SourceNodeID.Int64 != peer.ID {
			t.Fatalf("source = %+v, want peer %d: the upgraded method must carry its peer provenance, not read as local", got.SourceNodeID, peer.ID)
		}
	})

	// A methodless local touch at the recorded run must not launder a
	// peer's recorded method into the local class: it changes nothing.
	t.Run("methodless local touch does not reclaim a peer method", func(t *testing.T) {
		s := openTestStore(t)
		vID := makeVolume(t, s, "/v")
		self, err := s.GetSelfNode(ctx)
		if err != nil {
			t.Fatalf("GetSelfNode: %v", err)
		}
		peer, err := s.GetOrCreateOriginNode(ctx, "nas")
		if err != nil {
			t.Fatalf("GetOrCreateOriginNode: %v", err)
		}
		if err := s.UpsertDestinationRunIDPulled(ctx, vID, "offsite", self.ID, 10, VerifyMethodBlake3, peer.ID, false); err != nil {
			t.Fatalf("peer blake3 advance: %v", err)
		}
		if err := s.UpsertDestinationRunID(ctx, vID, "offsite", self.ID, 10, false); err != nil {
			t.Fatalf("methodless local touch: %v", err)
		}
		got, err := s.GetDestinationRunID(ctx, vID, "offsite", self.ID)
		if err != nil {
			t.Fatalf("GetDestinationRunID: %v", err)
		}
		if got.VerifyMethod != VerifyMethodBlake3 {
			t.Fatalf("method = %q, want %q preserved", got.VerifyMethod, VerifyMethodBlake3)
		}
		if !got.SourceNodeID.Valid || got.SourceNodeID.Int64 != peer.ID {
			t.Fatalf("source = %+v, want peer %d: a methodless touch must not launder the peer method to local", got.SourceNodeID, peer.ID)
		}
	})

	// A local verified re-confirmation of the same method a peer asserted
	// reclaims the component to local — local evidence is dominant and
	// never left revocable by the peer that echoed it.
	t.Run("local re-verification of the same method reclaims", func(t *testing.T) {
		s := openTestStore(t)
		vID := makeVolume(t, s, "/v")
		self, err := s.GetSelfNode(ctx)
		if err != nil {
			t.Fatalf("GetSelfNode: %v", err)
		}
		peer, err := s.GetOrCreateOriginNode(ctx, "nas")
		if err != nil {
			t.Fatalf("GetOrCreateOriginNode: %v", err)
		}
		if err := s.UpsertDestinationRunIDPulled(ctx, vID, "offsite", self.ID, 10, VerifyMethodBlake3, peer.ID, false); err != nil {
			t.Fatalf("peer blake3 advance: %v", err)
		}
		if err := s.UpsertDestinationRunIDVerified(ctx, vID, "offsite", self.ID, 10, VerifyMethodBlake3, false); err != nil {
			t.Fatalf("local blake3 re-verify at equal run: %v", err)
		}
		got, err := s.GetDestinationRunID(ctx, vID, "offsite", self.ID)
		if err != nil {
			t.Fatalf("GetDestinationRunID: %v", err)
		}
		if got.SourceNodeID.Valid {
			t.Fatalf("source = %+v, want NULL: a local re-verification must reclaim ownership from the peer", got.SourceNodeID)
		}
	})
}

// TestRevokeDestinationRunIDsFromSource: revoking a peer drops the live
// components it asserted while leaving locally-verified components and a
// second peer's assertions in place, and the append-only history is
// untouched — revocation is a forward act, not a rewrite of the audit
// trail or the verified vector.
func TestRevokeDestinationRunIDsFromSource(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	badPeer, err := s.GetOrCreateOriginNode(ctx, "nas")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode(nas): %v", err)
	}
	goodPeer, err := s.GetOrCreateOriginNode(ctx, "mirror")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode(mirror): %v", err)
	}
	originA, err := s.GetOrCreateOriginNode(ctx, "laptop")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode(laptop): %v", err)
	}

	if err := s.UpsertDestinationRunIDVerified(ctx, vID, "offsite", self.ID, 9, VerifyMethodBlake3, false); err != nil {
		t.Fatalf("local advance: %v", err)
	}
	if err := s.UpsertDestinationRunIDPulled(ctx, vID, "offsite", originA.ID, 5, VerifyMethodKopia, badPeer.ID, false); err != nil {
		t.Fatalf("bad-peer advance: %v", err)
	}
	if err := s.UpsertDestinationRunIDPulled(ctx, vID, "offsite", goodPeer.ID, 3, VerifyMethodKopia, goodPeer.ID, false); err != nil {
		t.Fatalf("good-peer advance: %v", err)
	}

	n, err := s.RevokeDestinationRunIDsFromSource(ctx, badPeer.ID)
	if err != nil {
		t.Fatalf("RevokeDestinationRunIDsFromSource: %v", err)
	}
	if n != 1 {
		t.Fatalf("revoked %d components, want 1", n)
	}

	if _, err := s.GetDestinationRunID(ctx, vID, "offsite", originA.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoked component still present (err=%v)", err)
	}
	if local, err := s.GetDestinationRunID(ctx, vID, "offsite", self.ID); err != nil || local.SourceNodeID.Valid {
		t.Fatalf("locally-verified component disturbed by revocation: %+v err=%v", local, err)
	}
	if good, err := s.GetDestinationRunID(ctx, vID, "offsite", goodPeer.ID); err != nil || good.SourceNodeID.Int64 != goodPeer.ID {
		t.Fatalf("other peer's component disturbed by revocation: %+v err=%v", good, err)
	}

	history, err := s.ListDestinationRunIDHistory(ctx, vID, "offsite")
	if err != nil {
		t.Fatalf("ListDestinationRunIDHistory: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history rows = %d after revocation, want 3 (audit trail untouched)", len(history))
	}
}

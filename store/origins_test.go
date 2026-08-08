package store

import (
	"context"
	"testing"
)

// seedOriginFile upserts one file row with the given status and optional
// provenance, the fixture both origin-coordinate queries read.
func seedOriginFile(t *testing.T, s *Store, volumeID int64, path string, b byte, status string, runID int64, prov *Provenance) {
	t.Helper()
	if err := s.Upsert(context.Background(), FileRow{
		VolumeID: volumeID, Path: path, Blake3: digest(b),
		SizeBytes: 1, MtimeNs: 1, Status: status,
		FirstSeenRunID: runID, LastSeenRunID: runID, IndexedAtNs: 1,
	}, prov); err != nil {
		t.Fatalf("Upsert %s: %v", path, err)
	}
}

// TestPresentFilesByOrigin: the buckets count present *files* per origin
// coordinate — two paths sharing one content count twice, because the
// question the fleet row asks ("how many files are missing there") is asked
// about the tree the operator sees. Locally-introduced content counts under
// the self node at its introduction run, non-present rows and the reserved
// sync subtrees not at all, matching PresentOriginMaxima.
func TestPresentFilesByOrigin(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run1, run2 := makeRun(t, s, vID), makeRun(t, s, vID)
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	ext, err := s.CreateNode(ctx, "ext", "peer://ext")
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	seedOriginFile(t, s, vID, "a.txt", 0xA1, StatusPresent, run1, nil)
	seedOriginFile(t, s, vID, "a-dup.txt", 0xA1, StatusPresent, run2, nil)
	seedOriginFile(t, s, vID, "b.txt", 0xA2, StatusPresent, run2, nil)
	seedOriginFile(t, s, vID, "c.txt", 0xA3, StatusPresent, run1, &Provenance{NodeID: ext.ID, RunID: 50})
	seedOriginFile(t, s, vID, "gone.txt", 0xA4, StatusMissing, run2, nil)
	seedOriginFile(t, s, vID, ".squirrel-conflicts/run-1/x.bin", 0xA5, StatusPresent, run2, &Provenance{NodeID: ext.ID, RunID: 999})

	buckets, err := s.PresentFilesByOrigin(ctx, vID, self.ID)
	if err != nil {
		t.Fatalf("PresentFilesByOrigin: %v", err)
	}
	got := map[[2]int64]int64{}
	for _, b := range buckets {
		got[[2]int64{b.OriginNodeID, b.OriginRunID}] = b.Files
	}
	// a.txt and its duplicate share one content introduced at run1, so
	// both files land in the run1 bucket; b.txt is its own at run2.
	want := map[[2]int64]int64{
		{self.ID, run1}: 2,
		{self.ID, run2}: 1,
		{ext.ID, 50}:    1,
	}
	if len(got) != len(want) {
		t.Fatalf("buckets = %+v, want %+v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("bucket %v = %d files, want %d", k, got[k], n)
		}
	}
}

// TestKnownOriginMaximaSpansEveryStatus is the difference that matters
// against PresentOriginMaxima: content this node received and later
// offloaded or deleted has still been *seen*, so it counts toward the floor
// the "ahead" inference reads. Otherwise every offloading edge machine
// would look permanently outrun by its peers.
func TestKnownOriginMaximaSpansEveryStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	run1 := makeRun(t, s, vID)
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	ext, err := s.CreateNode(ctx, "ext", "peer://ext")
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	seedOriginFile(t, s, vID, "here.txt", 0xB1, StatusPresent, run1, &Provenance{NodeID: ext.ID, RunID: 10})
	seedOriginFile(t, s, vID, "offloaded.txt", 0xB2, StatusOffloaded, run1, &Provenance{NodeID: ext.ID, RunID: 40})

	present, err := s.PresentOriginMaxima(ctx, vID, self.ID)
	if err != nil {
		t.Fatalf("PresentOriginMaxima: %v", err)
	}
	if len(present) != 1 || present[0].OriginNodeID != ext.ID || present[0].OriginRunID != 10 {
		t.Fatalf("present maxima = %+v, want ext→10 (the offloaded row is not present)", present)
	}
	known, err := s.KnownOriginMaxima(ctx, vID, self.ID)
	if err != nil {
		t.Fatalf("KnownOriginMaxima: %v", err)
	}
	if len(known) != 1 || known[0].OriginNodeID != ext.ID || known[0].OriginRunID != 40 {
		t.Fatalf("known maxima = %+v, want ext→40 (the offloaded row was still seen)", known)
	}
}

// TestListVolumePeerSyncStates: the listing resolves peer names and is
// scoped to the volume, so a hub can enumerate the edge machines a volume
// has met without joining the nodes table itself.
func TestListVolumePeerSyncStates(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	photos, docs := makeVolume(t, s, "/photos"), makeVolume(t, s, "/docs")
	laptop, err := s.CreateNode(ctx, "laptop", "peer://laptop")
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	homepc, err := s.CreateNode(ctx, "homepc", "peer://homepc")
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if err := s.UpsertPeerSyncState(ctx, photos, laptop.ID, 7, false); err != nil {
		t.Fatalf("UpsertPeerSyncState laptop: %v", err)
	}
	if err := s.UpsertPeerSyncState(ctx, photos, homepc.ID, 3, false); err != nil {
		t.Fatalf("UpsertPeerSyncState homepc: %v", err)
	}
	if err := s.UpsertPeerSyncState(ctx, docs, laptop.ID, 9, false); err != nil {
		t.Fatalf("UpsertPeerSyncState docs: %v", err)
	}

	got, err := s.ListVolumePeerSyncStates(ctx, photos)
	if err != nil {
		t.Fatalf("ListVolumePeerSyncStates: %v", err)
	}
	if len(got) != 2 || got[0].PeerName != "homepc" || got[1].PeerName != "laptop" {
		t.Fatalf("peers = %+v, want homepc then laptop (by name)", got)
	}
	if got[1].LastSharedRunID.Int64 != 7 || got[1].LastSyncedAtNs == 0 {
		t.Errorf("laptop state = %+v, want the shared run and a stamp", got[1])
	}
}

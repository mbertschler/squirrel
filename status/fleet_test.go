package status

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/store"
)

// fleetPlace finds a place by name in a report's first volume.
func fleetPlace(t *testing.T, rep Report, name string) FleetPlace {
	t.Helper()
	for _, p := range rep.Volumes[0].Fleet {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("fleet has no place %q: %+v", name, rep.Volumes[0].Fleet)
	return FleetPlace{}
}

// TestFleetSameWhenCovered: a place whose durability vector reaches every
// present file is caught up, with nothing missing and all three ages
// answered — the "you may close the laptop" row.
func TestFleetSameWhenCovered(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, idx := indexTree(t, s, "photos")
	v, _ := s.GetVolumeByName(ctx, "photos")
	self, _ := s.GetSelfNode(ctx)
	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "nas", self.ID, idx.RunID, store.VerifyMethodPeer, false); err != nil {
		t.Fatalf("seed vector: %v", err)
	}
	recordSuccessfulSync(t, s, v.ID, "nas")

	rep, err := Build(ctx, s, cfgFor("photos", root, []string{"nas"}, nil, nil, []string{"nas"}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p := fleetPlace(t, rep, "nas")
	if p.State != FleetSame || p.Missing != 0 || !p.MissingKnown {
		t.Errorf("place = %+v, want same with nothing missing", p)
	}
	if p.Kind != KindNode || !p.SyncTarget {
		t.Errorf("place role wrong: %+v", p)
	}
	if p.LastChangeAgo == nil || p.LastVerifiedAgo == nil || p.AsOfAgo == nil {
		t.Errorf("a caught-up place must answer all three ages: %+v", p)
	}
	if p.Stale || p.Level != LevelOK {
		t.Errorf("level = %v stale = %v, want ok and current", p.Level, p.Stale)
	}
	if got := FleetStateLabel(p); got != "same" {
		t.Errorf("FleetStateLabel = %q, want same", got)
	}
	if rep.Level() != LevelOK {
		t.Errorf("report level = %v, want ok", rep.Level())
	}
}

// TestFleetBehindCountsFiles: content indexed after the last push has not
// reached the place, and the row says how many files that is — the headline
// number the fleet view exists for.
func TestFleetBehindCountsFiles(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, idx := indexTree(t, s, "photos")
	v, _ := s.GetVolumeByName(ctx, "photos")
	self, _ := s.GetSelfNode(ctx)
	// The vector covers the run before the index, so all three files of
	// the tree sit above it.
	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "nas", self.ID, idx.RunID-1, store.VerifyMethodPeer, false); err != nil {
		t.Fatalf("seed vector: %v", err)
	}
	recordSuccessfulSync(t, s, v.ID, "nas")

	rep, err := Build(ctx, s, cfgFor("photos", root, []string{"nas"}, nil, nil, []string{"nas"}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p := fleetPlace(t, rep, "nas")
	if p.State != FleetBehind || p.Missing != 3 {
		t.Errorf("place = %+v, want behind by 3 files", p)
	}
	if got := FleetMissingLabel(p); got != "3" {
		t.Errorf("FleetMissingLabel = %q, want 3", got)
	}
	// A backlog between two syncs is the normal state; the cadence, not
	// the backlog, decides whether that is a problem.
	if p.Level != LevelOK || rep.Level() != LevelOK {
		t.Errorf("levels = place %v report %v, want ok — a fresh sync with a backlog is not a fault", p.Level, rep.Level())
	}
}

// TestFleetAheadFromRelayedEvidence: evidence pulled from a peer names an
// origin node whose content this machine has never seen, so the place holds
// something this one does not. It is reported and never scored — a
// watermark is not an inventory.
func TestFleetAheadFromRelayedEvidence(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, idx := indexTree(t, s, "photos")
	v, _ := s.GetVolumeByName(ctx, "photos")
	self, _ := s.GetSelfNode(ctx)
	nas, err := s.GetOrCreateOriginNode(ctx, "nas")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode: %v", err)
	}
	homepc, err := s.GetOrCreateOriginNode(ctx, "homepc")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode: %v", err)
	}
	// The archive holds this machine's content and, relayed via nas,
	// homepc's — which never reached this machine at all.
	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "s3archive", self.ID, idx.RunID, store.VerifyMethodBlake3, false); err != nil {
		t.Fatalf("seed self component: %v", err)
	}
	if err := s.UpsertDestinationRunIDPulled(ctx, v.ID, "s3archive", homepc.ID, 42, store.VerifyMethodBlake3, nas.ID, time.Now().UnixNano(), false); err != nil {
		t.Fatalf("seed pulled component: %v", err)
	}

	rep, err := Build(ctx, s, cfgFor("photos", root, nil, []string{"s3archive"}, nil, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p := fleetPlace(t, rep, "s3archive")
	if !p.Ahead || p.State != FleetAhead {
		t.Errorf("place = %+v, want ahead", p)
	}
	if p.Missing != 0 {
		t.Errorf("missing = %d, want 0 — this machine's own content is covered", p.Missing)
	}
	if p.Level != LevelOK {
		t.Errorf("level = %v, want ok — an inference about content elsewhere is not an alarm", p.Level)
	}
	if got := FleetStateLabel(p); got != "ahead" {
		t.Errorf("FleetStateLabel = %q, want ahead", got)
	}
}

// TestFleetInboundPeerIsInformational is the hub's seat: an edge machine
// that pushes here is named in no target list and no durability vector, so
// the only trace of it is the exchange record. It must still appear — it is
// a place this volume lives — and must not colour the hub amber for content
// the hub was never responsible for putting there.
func TestFleetInboundPeerIsInformational(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, _ := indexTree(t, s, "photos")
	v, _ := s.GetVolumeByName(ctx, "photos")
	laptop, err := s.GetOrCreateOriginNode(ctx, "laptop")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode: %v", err)
	}
	if err := s.UpsertPeerSyncState(ctx, v.ID, laptop.ID, 7, false); err != nil {
		t.Fatalf("UpsertPeerSyncState: %v", err)
	}

	rep, err := Build(ctx, s, cfgFor("photos", root, nil, nil, nil, []string{"laptop"}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(rep.Volumes[0].Targets) != 0 {
		t.Fatalf("the volume declares no targets: %+v", rep.Volumes[0].Targets)
	}
	p := fleetPlace(t, rep, "laptop")
	if p.SyncTarget || p.Required || p.Level != LevelNeutral {
		t.Errorf("place = %+v, want an informational row", p)
	}
	// Nothing is known about what it holds, and the row says so rather
	// than rendering a zero.
	if p.MissingKnown || FleetMissingLabel(p) != "—" || FleetStateLabel(p) != "unknown" {
		t.Errorf("place = %+v, want unknown coverage", p)
	}
	if p.LastChangeAgo == nil || p.AsOfAgo == nil {
		t.Errorf("the exchange is on record, so both ages are answerable: %+v", p)
	}
	if code := rep.Level().ExitCode(); code != 0 {
		t.Errorf("report exit code = %d, want 0 — a peer this node pushes nothing to is not a fault", code)
	}
}

// TestFleetStaleReadsUnknown is the property the whole view stands on: a
// place gone dark keeps its last known figures and stops claiming they are
// current. Anything else would present month-old hearsay as fact
// (ux-principles §3).
func TestFleetStaleReadsUnknown(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, idx := indexTree(t, s, "photos")
	v, _ := s.GetVolumeByName(ctx, "photos")
	self, _ := s.GetSelfNode(ctx)
	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "htpc", self.ID, idx.RunID, store.VerifyMethodPeer, false); err != nil {
		t.Fatalf("seed vector: %v", err)
	}
	recordSuccessfulSync(t, s, v.ID, "htpc")
	cfg := cfgFor("photos", root, []string{"htpc"}, nil, nil, []string{"htpc"})
	// A cadence this short makes everything already recorded ancient,
	// without waiting or rewriting timestamps.
	cfg.Volumes["photos"].SyncEvery = time.Nanosecond

	rep, err := Build(ctx, s, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p := fleetPlace(t, rep, "htpc")
	if !p.Stale {
		t.Fatalf("place = %+v, want stale past three cadences", p)
	}
	if got := FleetStateLabel(p); got != "unknown" {
		t.Errorf("FleetStateLabel = %q, want unknown — the figures are too old to vouch for", got)
	}
	// The figures themselves survive; saying what was last known is not
	// the same as claiming it still holds.
	if p.State != FleetSame || FleetMissingLabel(p) != "0" {
		t.Errorf("place = %+v, want the last known figures kept", p)
	}
	if p.Level != LevelCritical {
		t.Errorf("level = %v, want red — the place has stopped answering", p.Level)
	}
}

// TestFleetUnverifiablePairStaysGreen guards the regression that would make
// the fleet view unusable in the reference household: a crypt destination
// (or any shallow sync) verifies by size+mtime and so records no durability
// component at all. The row reads unknown, but a destination behaving
// exactly as configured must not hold the report off green.
func TestFleetUnverifiablePairStaysGreen(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, _ := indexTree(t, s, "docs")
	v, _ := s.GetVolumeByName(ctx, "docs")
	recordSuccessfulSync(t, s, v.ID, "cloudbox")

	rep, err := Build(ctx, s, cfgFor("docs", root, []string{"cloudbox"}, nil, []string{"cloudbox"}, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p := fleetPlace(t, rep, "cloudbox")
	if p.MissingKnown || FleetStateLabel(p) != "unknown" {
		t.Errorf("place = %+v, want unknown coverage — nothing recorded what it holds", p)
	}
	if p.LastChangeAgo == nil || p.AsOfAgo == nil {
		t.Errorf("the push is on record, so the ages are answerable: %+v", p)
	}
	if p.Level != LevelOK || rep.Level() != LevelOK {
		t.Errorf("levels = place %v report %v, want ok", p.Level, rep.Level())
	}
}

// TestFleetPlaceBeyondConfig: a destination the config no longer names
// still holds copies of this content, and squirrel never loses track of
// content. It keeps its row, informational, below the configured places.
func TestFleetPlaceBeyondConfig(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, idx := indexTree(t, s, "photos")
	v, _ := s.GetVolumeByName(ctx, "photos")
	self, _ := s.GetSelfNode(ctx)
	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "nas", self.ID, idx.RunID, store.VerifyMethodPeer, false); err != nil {
		t.Fatalf("seed vector: %v", err)
	}
	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "old-usb", self.ID, idx.RunID, store.VerifyMethodBlake3, false); err != nil {
		t.Fatalf("seed dropped destination: %v", err)
	}
	recordSuccessfulSync(t, s, v.ID, "nas")

	rep, err := Build(ctx, s, cfgFor("photos", root, []string{"nas"}, nil, nil, []string{"nas"}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	fleet := rep.Volumes[0].Fleet
	if len(fleet) != 2 || fleet[0].Name != "nas" || fleet[1].Name != "old-usb" {
		t.Fatalf("fleet = %+v, want the configured place first then the dropped one", fleet)
	}
	dropped := fleet[1]
	if dropped.SyncTarget || dropped.Required || dropped.Level != LevelNeutral {
		t.Errorf("dropped place = %+v, want an informational row", dropped)
	}
	if dropped.State != FleetSame || FleetChangeLabel(dropped) != "unknown" {
		t.Errorf("dropped place = %+v, want its recorded coverage with an unknown last change", dropped)
	}
}

// TestFleetAbsentWithoutAnIndex: a volume that has never been indexed lives
// nowhere yet, so it grows no fleet block — its targets already say it has
// never been synced.
func TestFleetAbsentWithoutAnIndex(t *testing.T) {
	s := setupStore(t)
	rep, err := Build(context.Background(), s, cfgFor("photos", "/nope", []string{"nas"}, nil, nil, []string{"nas"}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(rep.Volumes[0].Fleet) != 0 {
		t.Errorf("fleet = %+v, want none for a never-indexed volume", rep.Volumes[0].Fleet)
	}
}

// TestFleetVerifiedIsTheWeakestLink: a place with one freshly verified
// component and one that has not been checked in a long time reports the
// old one. A place must not read freshly checked because part of it was.
func TestFleetVerifiedIsTheWeakestLink(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, idx := indexTree(t, s, "photos")
	v, _ := s.GetVolumeByName(ctx, "photos")
	self, _ := s.GetSelfNode(ctx)
	nas, err := s.GetOrCreateOriginNode(ctx, "nas")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode: %v", err)
	}
	homepc, err := s.GetOrCreateOriginNode(ctx, "homepc")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode: %v", err)
	}
	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "s3archive", self.ID, idx.RunID, store.VerifyMethodBlake3, false); err != nil {
		t.Fatalf("seed fresh component: %v", err)
	}
	stale := time.Now().Add(-72 * time.Hour).UnixNano()
	if err := s.UpsertDestinationRunIDPulled(ctx, v.ID, "s3archive", homepc.ID, 42, store.VerifyMethodBlake3, nas.ID, stale, false); err != nil {
		t.Fatalf("seed stale component: %v", err)
	}

	rep, err := Build(ctx, s, cfgFor("photos", root, nil, []string{"s3archive"}, nil, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p := fleetPlace(t, rep, "s3archive")
	if p.LastVerifiedAgo == nil || *p.LastVerifiedAgo < 71*time.Hour {
		t.Errorf("last verified = %v, want the older of the two components", p.LastVerifiedAgo)
	}
	// The row's own currency is the freshest thing it learned, which is
	// a different question from how old the verification is.
	if p.AsOfAgo == nil || *p.AsOfAgo > time.Minute {
		t.Errorf("as of = %v, want the recent component's arrival", p.AsOfAgo)
	}
}

// TestFleetVerifiedUnknownWhenAComponentCarriesNone: a component whose
// verification instant is unknown — a pull from a peer that relayed none —
// makes the whole row's verification unknown, the same fail-closed reading
// the offload gate takes toward evidence that was never dated.
func TestFleetVerifiedUnknownWhenAComponentCarriesNone(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, idx := indexTree(t, s, "photos")
	v, _ := s.GetVolumeByName(ctx, "photos")
	self, _ := s.GetSelfNode(ctx)
	hub, _ := s.GetOrCreateOriginNode(ctx, "hub")
	homepc, _ := s.GetOrCreateOriginNode(ctx, "homepc")
	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "nas", self.ID, idx.RunID, store.VerifyMethodPeer, false); err != nil {
		t.Fatalf("seed verified component: %v", err)
	}
	if err := s.UpsertDestinationRunIDPulled(ctx, v.ID, "nas", homepc.ID, 5, store.VerifyMethodBlake3, hub.ID, 0, false); err != nil {
		t.Fatalf("seed undated component: %v", err)
	}
	recordSuccessfulSync(t, s, v.ID, "nas")

	rep, err := Build(ctx, s, cfgFor("photos", root, []string{"nas"}, nil, nil, []string{"nas"}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p := fleetPlace(t, rep, "nas")
	if p.LastVerifiedAgo != nil || FleetVerifiedLabel(p) != "unknown" {
		t.Errorf("place = %+v, want an unknown verification", p)
	}
	// Coverage is still known — the components say what arrived, just
	// not that anybody dated the check.
	if !p.MissingKnown || p.Missing != 0 {
		t.Errorf("place = %+v, want its coverage read", p)
	}
}

// TestFleetOrderFollowsTheGrid keeps the two blocks readable side by side:
// the fleet lists the volume's configured targets in the grid's own order
// before anything the index adds.
func TestFleetOrderFollowsTheGrid(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, _ := indexTree(t, s, "photos")
	cfg := cfgFor("photos", root, []string{"nas"}, []string{"s3archive", "nas"}, nil, []string{"nas"})

	rep, err := Build(ctx, s, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	vs := rep.Volumes[0]
	if len(vs.Fleet) != len(vs.Targets) {
		t.Fatalf("fleet = %+v, want one row per target", vs.Fleet)
	}
	for i := range vs.Targets {
		if vs.Fleet[i].Name != vs.Targets[i].Name || vs.Fleet[i].Kind != vs.Targets[i].Kind {
			t.Errorf("row %d = %q/%v, want %q/%v", i, vs.Fleet[i].Name, vs.Fleet[i].Kind, vs.Targets[i].Name, vs.Targets[i].Kind)
		}
	}
}

// TestFleetMissingCountsEveryOrigin: content forwarded here from another
// machine is content this machine holds, so a place that has not received it
// is missing it — the hub's copy of an edge machine's photos is exactly the
// case the coverage question is asked about.
func TestFleetMissingCountsEveryOrigin(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, idx := indexTree(t, s, "photos")
	v, _ := s.GetVolumeByName(ctx, "photos")
	self, _ := s.GetSelfNode(ctx)
	laptop, err := s.GetOrCreateOriginNode(ctx, "laptop")
	if err != nil {
		t.Fatalf("GetOrCreateOriginNode: %v", err)
	}
	// One more file, forwarded from the laptop.
	if err := os.WriteFile(filepath.Join(root, "laptop.jpg"), []byte("forwarded"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Upsert(ctx, store.FileRow{
		VolumeID: v.ID, Path: "laptop.jpg", Blake3: make([]byte, 32),
		SizeBytes: 9, MtimeNs: 1, Status: store.StatusPresent,
		FirstSeenRunID: idx.RunID, LastSeenRunID: idx.RunID, IndexedAtNs: 1,
	}, &store.Provenance{NodeID: laptop.ID, RunID: 12}); err != nil {
		t.Fatalf("Upsert forwarded row: %v", err)
	}
	// The archive has this machine's own content but nothing of the
	// laptop's.
	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "s3archive", self.ID, idx.RunID, store.VerifyMethodBlake3, false); err != nil {
		t.Fatalf("seed vector: %v", err)
	}

	rep, err := Build(ctx, s, cfgFor("photos", root, nil, []string{"s3archive"}, nil, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p := fleetPlace(t, rep, "s3archive")
	if p.State != FleetBehind || p.Missing != 1 {
		t.Errorf("place = %+v, want behind by the one forwarded file", p)
	}
}

// TestFleetHonoursEvidencePolicy: a place this node cannot push to is
// judged against the volume's own evidence policy, the only budget the
// operator declared for it.
func TestFleetHonoursEvidencePolicy(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, idx := indexTree(t, s, "photos")
	v, _ := s.GetVolumeByName(ctx, "photos")
	self, _ := s.GetSelfNode(ctx)
	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "s3archive", self.ID, idx.RunID, store.VerifyMethodBlake3, false); err != nil {
		t.Fatalf("seed vector: %v", err)
	}
	cfg := cfgFor("photos", root, nil, []string{"s3archive"}, nil, nil)
	vol := cfg.Volumes["photos"]

	rep, err := Build(ctx, s, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p := fleetPlace(t, rep, "s3archive"); p.Level != LevelOK || p.Stale {
		t.Errorf("place = %+v, want ok with no policy to judge it by", p)
	}

	vol.OffloadMaxEvidenceAge = time.Nanosecond
	rep, err = Build(ctx, s, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p := fleetPlace(t, rep, "s3archive")
	if p.Level != LevelWarn || !p.Stale || FleetStateLabel(p) != "unknown" {
		t.Errorf("place = %+v, want amber and unknown past the evidence policy", p)
	}
}

func TestFleetStateStrings(t *testing.T) {
	for _, tc := range []struct {
		state FleetState
		want  string
	}{
		{FleetUnknown, "unknown"},
		{FleetSame, "same"},
		{FleetBehind, "behind"},
		{FleetAhead, "ahead"},
		{FleetDiverged, "diverged"},
		{FleetState(99), "unknown"},
	} {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("FleetState(%d).String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// TestFleetDivergedWhenBothDirections: each side holds something the other
// does not, and the row names that rather than picking one direction.
func TestFleetDivergedWhenBothDirections(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()
	root, idx := indexTree(t, s, "photos")
	v, _ := s.GetVolumeByName(ctx, "photos")
	self, _ := s.GetSelfNode(ctx)
	nas, _ := s.GetOrCreateOriginNode(ctx, "nas")
	homepc, _ := s.GetOrCreateOriginNode(ctx, "homepc")
	if err := s.UpsertDestinationRunIDVerified(ctx, v.ID, "s3archive", self.ID, idx.RunID-1, store.VerifyMethodBlake3, false); err != nil {
		t.Fatalf("seed self component: %v", err)
	}
	if err := s.UpsertDestinationRunIDPulled(ctx, v.ID, "s3archive", homepc.ID, 42, store.VerifyMethodBlake3, nas.ID, time.Now().UnixNano(), false); err != nil {
		t.Fatalf("seed pulled component: %v", err)
	}

	rep, err := Build(ctx, s, cfgFor("photos", root, nil, []string{"s3archive"}, nil, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p := fleetPlace(t, rep, "s3archive")
	if p.State != FleetDiverged || p.Missing != 3 || !p.Ahead {
		t.Errorf("place = %+v, want diverged: 3 missing there, content here never seen", p)
	}
	if got := FleetStateLabel(p); got != "diverged" {
		t.Errorf("FleetStateLabel = %q, want diverged", got)
	}
}

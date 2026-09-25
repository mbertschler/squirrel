package sync

import (
	"context"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

func volumeComponents(t *testing.T, s *store.Store, volName, dest string) []store.DestinationRunID {
	t.Helper()
	v, err := s.GetVolumeByName(context.Background(), volName)
	if err != nil {
		t.Fatalf("GetVolumeByName: %v", err)
	}
	rows, err := s.ListVolumeDestinationRunIDs(context.Background(), v.ID)
	if err != nil {
		t.Fatalf("ListVolumeDestinationRunIDs: %v", err)
	}
	var out []store.DestinationRunID
	for _, r := range rows {
		if r.Destination == dest {
			out = append(out, r)
		}
	}
	return out
}

// TestKopiaPushAdvancesVector: a kopia push whose snapshot verify
// succeeds advances the vector exactly like a verified bucket push.
func TestKopiaPushAdvancesVector(t *testing.T) {
	installFakeKopia(t)
	f := setupKopiaFixture(t)

	rep, err := RunPair(context.Background(), f.store, f.tools, f.pair, Options{})
	if err != nil {
		t.Fatalf("RunPair: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusSuccess || !rep.Verification.Verified() {
		t.Fatalf("rep = %+v, want verified success backing the advance", rep)
	}
	comps := volumeComponents(t, f.store, f.pair.Volume.Name, f.pair.Destination.Name)
	if len(comps) != 1 {
		t.Fatalf("components = %+v, want exactly one self component", comps)
	}
	if comps[0].OriginRunID < 1 {
		t.Fatalf("origin_run_id = %d, want >= 1", comps[0].OriginRunID)
	}
}

// TestKopiaVerifyFailureLeavesVectorAlone: a failed verify means no
// durability claim, so the vector keeps its prior state.
func TestKopiaVerifyFailureLeavesVectorAlone(t *testing.T) {
	installFakeKopia(t)
	t.Setenv("KOPIA_FAKE_VERIFY_EXIT", "1")
	f := setupKopiaFixture(t)

	if _, err := RunPair(context.Background(), f.store, f.tools, f.pair, Options{}); err == nil {
		t.Fatalf("RunPair succeeded, want verify failure")
	}
	if comps := volumeComponents(t, f.store, f.pair.Volume.Name, f.pair.Destination.Name); len(comps) != 0 {
		t.Fatalf("components = %+v, want none after failed verify", comps)
	}
}

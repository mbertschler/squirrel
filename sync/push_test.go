package sync

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

// TestPushRefusesWipedRootWithUploadRecords: a root wiped without a
// `squirrel destination reset` and re-marked looks like a fresh start, but
// the upload records still claim its content. Starting over would skip
// every recorded object, write a segment mapping paths onto bytes that are
// gone, and close the run as success, so the push refuses instead. A reset
// clears the records and lifts the refusal.
func TestPushRefusesWipedRootWithUploadRecords(t *testing.T) {
	for layout, setup := range keyedArchiveFixtures() {
		t.Run(layout, func(t *testing.T) {
			f := setup(t)
			f.write(t, "a.txt", "alpha")
			f.index(t)
			if _, err := f.sync(t); err != nil {
				t.Fatalf("first push: %v", err)
			}

			f.wipeRemote(t)
			f.seedNamingMarker(t)
			f.seedMarker(t, "pics")
			markersOnly := f.remoteEntries(t)
			rep, err := f.sync(t)
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "destination reset") {
				t.Fatalf("push to a wiped root: err = %v, want a refusal naming `destination reset`", err)
			}
			if rep.Status != store.RunStatusRefused {
				t.Fatalf("Status = %q, want refused", rep.Status)
			}
			if after := f.remoteEntries(t); !slices.Equal(after, markersOnly) {
				t.Fatalf("the refused push wrote to the root: %v, want only the markers %v", after, markersOnly)
			}

			if _, _, err := f.store.ResetDestination(context.Background(), "offsite"); err != nil {
				t.Fatalf("ResetDestination: %v", err)
			}
			rep, err = f.sync(t)
			if err != nil || rep.Status != store.RunStatusSuccess {
				t.Fatalf("push after reset: status=%q err=%v", rep.Status, err)
			}
		})
	}
}

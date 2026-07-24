package tui

import (
	"database/sql"
	"testing"

	"github.com/mbertschler/squirrel/store"
)

// TestDestinationCell covers the F18 direction arrow in the runs table:
// bucket rows render the plain name, peer rows prefix the initiation arrow
// (→ outbound / ← inbound, keyed off runs.shallow).
func TestDestinationCell(t *testing.T) {
	bucket := store.Run{Destination: sql.NullString{String: "s3archive", Valid: true}}
	if got := destinationCell(bucket); got != "s3archive" {
		t.Errorf("bucket cell = %q, want %q", got, "s3archive")
	}
	outbound := store.Run{
		Destination: sql.NullString{String: "htpc", Valid: true},
		PeerNodeID:  sql.NullInt64{Int64: 3, Valid: true},
		Shallow:     sql.NullBool{Valid: true},
	}
	if got := destinationCell(outbound); got != "→ htpc" {
		t.Errorf("outbound cell = %q, want %q", got, "→ htpc")
	}
	inbound := store.Run{
		Destination: sql.NullString{String: "laptop", Valid: true},
		PeerNodeID:  sql.NullInt64{Int64: 4, Valid: true},
	}
	if got := destinationCell(inbound); got != "← laptop" {
		t.Errorf("inbound cell = %q, want %q", got, "← laptop")
	}
}

// TestRunIsNoOp covers the F19 fold rule: a clean success that touched no
// files is a no-op; anything else is not.
func TestRunIsNoOp(t *testing.T) {
	if !runIsNoOp(store.Run{Status: store.RunStatusSuccess, FileCount: 0}) {
		t.Error("clean 0-file success should be a no-op")
	}
	if runIsNoOp(store.Run{Status: store.RunStatusSuccess, FileCount: 3}) {
		t.Error("success that touched files is not a no-op")
	}
	if runIsNoOp(store.Run{Status: store.RunStatusFailed}) {
		t.Error("failed run is not a no-op")
	}
}

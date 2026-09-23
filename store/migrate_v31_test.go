package store

import (
	"context"
	"testing"
)

// TestMigrateV30ToV31RelabelsBlake3AsChecksum seeds the vector the way a
// v30 index holds it — a mirror component recorded as "blake3" beside a
// kopia one — and replays the v31 step. The mirror component becomes
// "checksum" with its run and timestamps untouched, the kopia component
// is left alone, and the history keeps the original "blake3" row and
// gains one "checksum" row recording the relabel (#211).
func TestMigrateV30ToV31RelabelsBlake3AsChecksum(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	vID := makeVolume(t, s, "/v")
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	seed := []string{
		`INSERT INTO destination_run_ids (volume_id, destination, origin_node_id, origin_run_id, updated_at_ns, verify_method, verified_at_ns)
			VALUES (?, 'usb', ?, 7, 100, 'blake3', 90)`,
		`INSERT INTO destination_run_ids (volume_id, destination, origin_node_id, origin_run_id, updated_at_ns, verify_method, verified_at_ns)
			VALUES (?, 'kopia-mirror', ?, 7, 100, 'kopia-verify', 90)`,
		`INSERT INTO destination_run_ids_history (volume_id, destination, origin_node_id, origin_run_id, at_ns, verify_method)
			VALUES (?, 'usb', ?, 7, 100, 'blake3')`,
	}
	for _, q := range seed {
		if _, err := s.db.ExecContext(ctx, q, vID, self.ID); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM schema_version WHERE version = 31`); err != nil {
		t.Fatalf("rewind schema_version: %v", err)
	}
	if err := migrateV30ToV31(ctx, s.db); err != nil {
		t.Fatalf("migrateV30ToV31: %v", err)
	}

	usb, err := s.GetDestinationRunID(ctx, vID, "usb", self.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID(usb): %v", err)
	}
	if usb.VerifyMethod != VerifyMethodChecksum || usb.OriginRunID != 7 || usb.UpdatedAtNs != 100 || usb.VerifiedAtNs.Int64 != 90 {
		t.Fatalf("usb component = %+v, want checksum at run 7 with timestamps 100/90 untouched", usb)
	}
	kopia, err := s.GetDestinationRunID(ctx, vID, "kopia-mirror", self.ID)
	if err != nil {
		t.Fatalf("GetDestinationRunID(kopia-mirror): %v", err)
	}
	if kopia.VerifyMethod != VerifyMethodKopia {
		t.Fatalf("kopia component method = %q, want %q", kopia.VerifyMethod, VerifyMethodKopia)
	}

	history, err := s.ListDestinationRunIDHistory(ctx, vID, "usb")
	if err != nil {
		t.Fatalf("ListDestinationRunIDHistory: %v", err)
	}
	if len(history) != 2 || history[0].VerifyMethod != "blake3" || history[1].VerifyMethod != VerifyMethodChecksum || history[1].OriginRunID != 7 {
		t.Fatalf("usb history = %+v, want the original blake3 row then one checksum relabel at run 7", history)
	}
	kopiaHistory, err := s.ListDestinationRunIDHistory(ctx, vID, "kopia-mirror")
	if err != nil {
		t.Fatalf("ListDestinationRunIDHistory(kopia-mirror): %v", err)
	}
	if len(kopiaHistory) != 0 {
		t.Fatalf("kopia-mirror history = %+v, want none appended", kopiaHistory)
	}
}

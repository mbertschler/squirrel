package sync

import (
	"context"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// RemoteVerifyReport summarises one verification pass over a
// destination's recorded content-addressed objects. Every recorded
// object lands in exactly one of Verified, Populated, Pending, Missing,
// or Mismatched.
type RemoteVerifyReport struct {
	Destination string
	RunID       int64
	// Objects is the number of recorded upload rows examined.
	Objects int
	// Verified objects matched their recorded fingerprint and had
	// verified_at_ns stamped.
	Verified int
	// Populated objects had no fingerprint yet (uploaded before capture
	// existed, or a capture that failed) and got one recorded on this
	// pass.
	Populated int
	// Pending objects still have no fingerprint: the backend exposes no
	// checksum for them.
	Pending int
	// Unrecorded counts objects present under the destination's objects/
	// directory with no upload record — orphans of runs that failed
	// before recording, harmless without a manifest mapping them.
	Unrecorded int
	// Missing lists recorded objects (hex hash keys) absent from the
	// remote listing.
	Missing    []string
	Mismatched []RemoteObjectMismatch
}

// RemoteObjectMismatch is one object whose provider checksum no longer
// matches the fingerprint recorded at upload time — potential corruption
// or tampering at the destination. The recorded fingerprint stays
// untouched so the evidence survives the pass.
type RemoteObjectMismatch struct {
	Hash     string
	Algo     string
	Recorded string
	// Actual is the provider's current value, or empty when the remote
	// no longer exposes the recorded algo for the object.
	Actual string
}

// Clean reports whether every recorded object was accounted for without
// a mismatch.
func (r RemoteVerifyReport) Clean() bool {
	return len(r.Missing) == 0 && len(r.Mismatched) == 0
}

// VerifyRemote re-reads the provider checksums of every object recorded
// on dest's underlying remote — one batched `lsjson --hash` over the
// destination-global objects/ directory — and compares them verbatim
// against the fingerprints recorded at upload time. Matches stamp
// verified_at_ns; objects with a pending fingerprint get one recorded;
// mismatches and missing objects land loudly on the report. The pass
// reads destination metadata and updates local verification state only.
//
// The pass is recorded as a kind='audit' run: success when every object
// checked out, partial when objects mismatched or went missing, failed
// when the pass itself aborted. A 'verify-destination' runs_audit entry
// carries the destination name and counters.
func VerifyRemote(ctx context.Context, s *store.Store, rcl *Rclone, dest *config.Destination) (RemoteVerifyReport, error) {
	rep := RemoteVerifyReport{Destination: dest.Name}
	if dest.Layout != config.LayoutContentAddressed {
		return rep, fmt.Errorf("destination %q has layout %q — verify covers the recorded objects of content-addressed destinations", dest.Name, dest.Layout)
	}
	rows, err := s.ListRemoteObjects(ctx, dest.Name)
	if err != nil {
		return rep, fmt.Errorf("list recorded objects for %q: %w", dest.Name, err)
	}
	rep.Objects = len(rows)
	if len(rows) == 0 {
		return rep, nil
	}
	runID, err := s.BeginRemoteVerifyRun(ctx)
	if err != nil {
		return rep, fmt.Errorf("record verify run: %w", err)
	}
	rep.RunID = runID

	verifyErr := verifyRecordedObjects(ctx, s, rcl, dest, rows, &rep)
	if err := recordVerifyOutcome(ctx, s, &rep, verifyErr); err != nil {
		return rep, err
	}
	return rep, verifyErr
}

// verifyRecordedObjects compares the remote listing against the recorded
// rows and applies the per-object outcome to the store and the report.
func verifyRecordedObjects(ctx context.Context, s *store.Store, rcl *Rclone, dest *config.Destination, rows []store.RemoteObjectRecord, rep *RemoteVerifyReport) error {
	entries, err := rcl.listHashes(ctx, underlyingObjectsURI(dest), verifyHashTypes(dest, rows), checkersArgs(dest)...)
	if err != nil {
		return fmt.Errorf("read object checksums from %q: %w", dest.Name, err)
	}
	byName := make(map[string]map[string]string, len(entries))
	for _, e := range entries {
		byName[e.Name] = e.Hashes
	}
	matched := 0
	for _, row := range rows {
		hash := hex.EncodeToString(row.Blake3)
		hashes, ok := byName[hash]
		if !ok {
			rep.Missing = append(rep.Missing, hash)
			continue
		}
		matched++
		if !row.ChecksumAlgo.Valid {
			if err := populateFingerprint(ctx, s, dest, row, hashes, rep); err != nil {
				return err
			}
			continue
		}
		actual := hashes[algoHashType(row.ChecksumAlgo.String)]
		if actual == row.Checksum.String {
			if err := s.MarkRemoteObjectVerified(ctx, row.ContentID, dest.Name, store.NowNs()); err != nil {
				return fmt.Errorf("stamp verification of %s: %w", hash, err)
			}
			rep.Verified++
			continue
		}
		rep.Mismatched = append(rep.Mismatched, RemoteObjectMismatch{
			Hash:     hash,
			Algo:     row.ChecksumAlgo.String,
			Recorded: row.Checksum.String,
			Actual:   actual,
		})
	}
	rep.Unrecorded = len(entries) - matched
	return nil
}

// populateFingerprint records the first fingerprint for a row whose pair
// is still pending; a backend exposing no checksum keeps it pending.
func populateFingerprint(ctx context.Context, s *store.Store, dest *config.Destination, row store.RemoteObjectRecord, hashes map[string]string, rep *RemoteVerifyReport) error {
	cs, ok := extractChecksum(dest, hashes)
	if !ok {
		rep.Pending++
		return nil
	}
	if err := s.SetRemoteObjectChecksum(ctx, row.ContentID, dest.Name, cs.Algo, cs.Value); err != nil {
		return fmt.Errorf("record fingerprint for %s: %w", hex.EncodeToString(row.Blake3), err)
	}
	rep.Populated++
	return nil
}

// verifyHashTypes plans the --hash-type set one pass requests: the hash
// names behind every recorded algo, plus the capture set when pending
// rows need a first fingerprint. nil requests every hash the backend
// exposes — required when pending rows exist on a backend with no
// configured selection.
func verifyHashTypes(dest *config.Destination, rows []store.RemoteObjectRecord) []string {
	set := map[string]bool{}
	pending := false
	for _, row := range rows {
		if row.ChecksumAlgo.Valid {
			set[algoHashType(row.ChecksumAlgo.String)] = true
		} else {
			pending = true
		}
	}
	if pending {
		capture := captureHashTypes(dest)
		if capture == nil {
			return nil
		}
		for _, t := range capture {
			set[t] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// recordVerifyOutcome finishes the pass's audit run and appends the
// 'verify-destination' runs_audit entry naming the destination and
// counters.
func recordVerifyOutcome(ctx context.Context, s *store.Store, rep *RemoteVerifyReport, verifyErr error) error {
	status := store.RunStatusSuccess
	errMsg := ""
	switch {
	case verifyErr != nil:
		status = store.RunStatusFailed
		errMsg = verifyErr.Error()
	case !rep.Clean():
		status = store.RunStatusPartial
		errMsg = fmt.Sprintf("%d object(s) failed verification on %q", len(rep.Missing)+len(rep.Mismatched), rep.Destination)
	}
	note := fmt.Sprintf("destination=%s objects=%d verified=%d fingerprinted=%d pending=%d mismatched=%d missing=%d unrecorded=%d",
		rep.Destination, rep.Objects, rep.Verified, rep.Populated, rep.Pending, len(rep.Mismatched), len(rep.Missing), rep.Unrecorded)
	if err := s.AppendRunAudit(ctx, store.RunAuditEntry{
		RunID: rep.RunID, Transition: store.TransitionVerifyDestination, Note: note,
	}); err != nil {
		return err
	}
	if err := s.FinishRun(ctx, rep.RunID, status, errMsg, int64(rep.Objects)); err != nil {
		return fmt.Errorf("finish verify run %d: %w", rep.RunID, err)
	}
	return nil
}

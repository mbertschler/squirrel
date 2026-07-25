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

	// Pack counters mirror the object ones for a packed destination's
	// per-pack sweep — one fingerprint check per pack vouches for every
	// content it holds. Zero on a content-addressed destination.
	Packs int
	// PacksVerified matched their recorded fingerprint and had
	// verified_at_ns refreshed.
	PacksVerified int
	// PacksPopulated had no fingerprint yet and got one recorded.
	PacksPopulated int
	// PacksPending still have no fingerprint: the backend exposes none.
	PacksPending int
	// PacksMissing lists recorded packs (hex key) absent from the remote
	// listing.
	PacksMissing []string
	// PackMismatched lists packs whose provider checksum no longer matches
	// the recorded one (Hash holds the pack key hex).
	PackMismatched []RemoteObjectMismatch

	// AlarmRaised is true when this pass latched a new standing alarm on
	// the destination because it was not clean (#157, F30). A pass on an
	// already-alarmed destination leaves it false (the latch was already
	// there). AlarmCleared is true when a clean pass auto-cleared a
	// previously standing alarm. Both drive the CLI's loud surfacing; the
	// authoritative state lives in the destination_alarms latch.
	AlarmRaised  bool
	AlarmCleared bool
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

// Clean reports whether every recorded object and pack was accounted for
// without a mismatch.
func (r RemoteVerifyReport) Clean() bool {
	return len(r.Missing) == 0 && len(r.Mismatched) == 0 &&
		len(r.PacksMissing) == 0 && len(r.PackMismatched) == 0
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
	if dest.Layout != config.LayoutContentAddressed && dest.Layout != config.LayoutPacked {
		return rep, fmt.Errorf("destination %q has layout %q — verify covers the recorded objects and packs of content-addressed and packed destinations", dest.Name, dest.Layout)
	}
	rows, err := s.ListRemoteObjects(ctx, dest.Name)
	if err != nil {
		return rep, fmt.Errorf("list recorded objects for %q: %w", dest.Name, err)
	}
	rep.Objects = len(rows)
	// A packed destination also keeps per-pack fingerprints; a
	// content-addressed one has none, so this stays empty there.
	var packs []store.RemotePackRecord
	if dest.Layout == config.LayoutPacked {
		packs, err = s.ListRemotePacks(ctx, dest.Name)
		if err != nil {
			return rep, fmt.Errorf("list recorded packs for %q: %w", dest.Name, err)
		}
		rep.Packs = len(packs)
	}
	if len(rows) == 0 && len(packs) == 0 {
		return rep, nil
	}
	runID, err := s.BeginRemoteVerifyRun(ctx)
	if err != nil {
		return rep, fmt.Errorf("record verify run: %w", err)
	}
	rep.RunID = runID

	verifyErr := verifyRecorded(ctx, s, rcl, dest, rows, packs, &rep)
	if err := recordVerifyOutcome(ctx, s, &rep, verifyErr); err != nil {
		return rep, err
	}
	// A clean pass may have filled the last pending fingerprint of a
	// (volume, destination) pair: re-attempt the durability-vector upgrade
	// now so the vector no longer stalls until the next content-writing
	// sync (friction-log F13). A dirty pass (mismatch or missing artifact)
	// leaves the recorded fingerprints as found, so it must not advance
	// evidence. Like RunPair's advance, an upgrade failure surfaces as the
	// command's error even though the audit run already closed.
	if verifyErr == nil && rep.Clean() {
		if err := upgradeFingerprintVectors(ctx, s, dest.Name); err != nil {
			return rep, err
		}
	}
	return rep, verifyErr
}

// upgradeFingerprintVectors re-stamps the durability vector of every
// volume as fingerprint-verified where the destination now carries a
// verified fingerprint behind all its present content
// (UpgradeDestinationVectorToFingerprintVerified is a no-op for a volume
// that was never pushed to the destination or still has a pending
// artifact, so iterating every volume is safe and self-limiting).
func upgradeFingerprintVectors(ctx context.Context, s *store.Store, destination string) error {
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		return fmt.Errorf("verify %q: self node: %w", destination, err)
	}
	volumes, err := s.ListVolumes(ctx)
	if err != nil {
		return fmt.Errorf("verify %q: list volumes: %w", destination, err)
	}
	for _, v := range volumes {
		if _, err := s.UpgradeDestinationVectorToFingerprintVerified(ctx, v.ID, destination, self.ID); err != nil {
			return fmt.Errorf("verify %q: upgrade vector for volume %q: %w", destination, v.Name, err)
		}
	}
	return nil
}

// verifyRecorded sweeps a destination's recorded content objects (the
// large-file per-object sweep, shared with content-addressed) and, for a
// packed destination, its recorded packs — one fingerprint check per pack
// vouching for all its members. Either sweep can be empty.
func verifyRecorded(ctx context.Context, s *store.Store, rcl *Rclone, dest *config.Destination, rows []store.RemoteObjectRecord, packs []store.RemotePackRecord, rep *RemoteVerifyReport) error {
	if len(rows) > 0 {
		if err := verifyRecordedObjects(ctx, s, rcl, dest, rows, rep); err != nil {
			return err
		}
	}
	if len(packs) > 0 {
		if err := verifyRecordedPacks(ctx, s, rcl, dest, packs, rep); err != nil {
			return err
		}
	}
	return nil
}

// verifyRecordedObjects compares the remote listing against the recorded
// rows and applies the per-object outcome to the store and the report.
func verifyRecordedObjects(ctx context.Context, s *store.Store, rcl *Rclone, dest *config.Destination, rows []store.RemoteObjectRecord, rep *RemoteVerifyReport) error {
	byName, err := readObjectChecksums(ctx, rcl, dest, rows)
	if err != nil {
		return fmt.Errorf("read object checksums from %q: %w", dest.Name, err)
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
	rep.Unrecorded = len(byName) - matched
	return nil
}

// readObjectChecksums reads the provider checksums verification compares,
// keyed by object basename (blake3 hex) then rclone hash name. s3 reads raw
// ETags straight from the S3 API — the only surface exposing a multipart
// composite ETag — and presents each under the "md5" slot so the shared
// comparison path (extractChecksum, algoHashType) treats it like any other
// backend hash. Every other backend reads one batched `rclone lsjson
// --hash` over the destination-global objects/ directory.
func readObjectChecksums(ctx context.Context, rcl *Rclone, dest *config.Destination, rows []store.RemoteObjectRecord) (map[string]map[string]string, error) {
	if dest.Type == "s3" {
		reader, err := newS3ETagReader(dest, ObjectsDirName)
		if err != nil {
			return nil, err
		}
		etags, err := reader.objectETags(ctx)
		if err != nil {
			return nil, err
		}
		byName := make(map[string]map[string]string, len(etags))
		for name, etag := range etags {
			byName[listedPlainName(dest, name)] = map[string]string{"md5": etag}
		}
		return byName, nil
	}
	entries, err := rcl.listHashes(ctx, underlyingDirURI(dest, ObjectsDirName), verifyObjectHashTypes(dest, rows), checkersArgs(dest)...)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]map[string]string, len(entries))
	for _, e := range entries {
		byName[listedPlainName(dest, e.Name)] = e.Hashes
	}
	return byName, nil
}

// populateFingerprint records the first fingerprint for a row whose pair
// is still pending, stamping verified_at_ns since this verify read is that
// object's first confirmation (mirrors populatePackFingerprint); a backend
// exposing no checksum keeps it pending.
func populateFingerprint(ctx context.Context, s *store.Store, dest *config.Destination, row store.RemoteObjectRecord, hashes map[string]string, rep *RemoteVerifyReport) error {
	cs, ok := extractChecksum(dest, hashes)
	if !ok {
		rep.Pending++
		return nil
	}
	if err := s.SetRemoteObjectFingerprint(ctx, row.ContentID, dest.Name, cs.Algo, cs.Value, store.NowNs()); err != nil {
		return fmt.Errorf("record fingerprint for %s: %w", hex.EncodeToString(row.Blake3), err)
	}
	rep.Populated++
	return nil
}

// verifyObjectHashTypes plans the --hash-type set the object sweep
// requests from the recorded object rows.
func verifyObjectHashTypes(dest *config.Destination, rows []store.RemoteObjectRecord) []string {
	var recorded []string
	pending := false
	for _, row := range rows {
		if row.ChecksumAlgo.Valid {
			recorded = append(recorded, row.ChecksumAlgo.String)
		} else {
			pending = true
		}
	}
	return verifyHashTypes(dest, recorded, pending)
}

// verifyRecordedPacks compares the remote packs/ listing against the
// recorded pack rows and applies the per-pack outcome. One fingerprint
// check per pack vouches for every content it holds, so a packed
// destination is swept per pack rather than per member.
func verifyRecordedPacks(ctx context.Context, s *store.Store, rcl *Rclone, dest *config.Destination, packs []store.RemotePackRecord, rep *RemoteVerifyReport) error {
	byName, err := readPackChecksums(ctx, rcl, dest, packs)
	if err != nil {
		return fmt.Errorf("read pack checksums from %q: %w", dest.Name, err)
	}
	for _, row := range packs {
		key := hex.EncodeToString(row.PackKey)
		hashes, ok := byName[key]
		if !ok {
			rep.PacksMissing = append(rep.PacksMissing, key)
			continue
		}
		if !row.ChecksumAlgo.Valid {
			if err := populatePackFingerprint(ctx, s, dest, row, hashes, rep); err != nil {
				return err
			}
			continue
		}
		actual := hashes[algoHashType(row.ChecksumAlgo.String)]
		if actual == row.Checksum.String {
			if err := s.MarkRemotePackVerified(ctx, row.PackID, dest.Name, store.NowNs()); err != nil {
				return fmt.Errorf("stamp verification of pack %s: %w", key, err)
			}
			rep.PacksVerified++
			continue
		}
		rep.PackMismatched = append(rep.PackMismatched, RemoteObjectMismatch{
			Hash:     key,
			Algo:     row.ChecksumAlgo.String,
			Recorded: row.Checksum.String,
			Actual:   actual,
		})
	}
	return nil
}

// readPackChecksums reads the provider checksums the pack sweep compares,
// keyed by pack key hex then rclone hash name. s3 reads raw ETags from the
// S3 API over the packs/ prefix — every pack is a multipart object, so its
// composite ETag is only visible here, not through rclone. Every other
// backend reads one batched `rclone lsjson --hash` over the packs/
// directory. The listing also returns the per-run placement maps under
// packs/; they key on their own names and never match a pack key, so they
// are harmlessly ignored.
func readPackChecksums(ctx context.Context, rcl *Rclone, dest *config.Destination, packs []store.RemotePackRecord) (map[string]map[string]string, error) {
	if dest.Type == "s3" {
		reader, err := newS3ETagReader(dest, PacksDirName)
		if err != nil {
			return nil, err
		}
		etags, err := reader.objectETags(ctx)
		if err != nil {
			return nil, err
		}
		byName := make(map[string]map[string]string, len(etags))
		for name, etag := range etags {
			byName[listedPlainName(dest, name)] = map[string]string{"md5": etag}
		}
		return byName, nil
	}
	entries, err := rcl.listHashes(ctx, underlyingDirURI(dest, PacksDirName), verifyPackHashTypes(dest, packs), checkersArgs(dest)...)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]map[string]string, len(entries))
	for _, e := range entries {
		byName[listedPlainName(dest, e.Name)] = e.Hashes
	}
	return byName, nil
}

// populatePackFingerprint records the first fingerprint for a pack whose
// pair is still pending; a backend exposing no checksum keeps it pending.
func populatePackFingerprint(ctx context.Context, s *store.Store, dest *config.Destination, row store.RemotePackRecord, hashes map[string]string, rep *RemoteVerifyReport) error {
	cs, ok := extractChecksum(dest, hashes)
	if !ok {
		rep.PacksPending++
		return nil
	}
	if err := s.SetRemotePackFingerprint(ctx, row.PackID, dest.Name, cs.Algo, cs.Value, store.NowNs()); err != nil {
		return fmt.Errorf("record fingerprint for pack %s: %w", hex.EncodeToString(row.PackKey), err)
	}
	rep.PacksPopulated++
	return nil
}

// verifyPackHashTypes plans the --hash-type set the pack sweep requests
// from the recorded pack rows.
func verifyPackHashTypes(dest *config.Destination, packs []store.RemotePackRecord) []string {
	var recorded []string
	pending := false
	for _, row := range packs {
		if row.ChecksumAlgo.Valid {
			recorded = append(recorded, row.ChecksumAlgo.String)
		} else {
			pending = true
		}
	}
	return verifyHashTypes(dest, recorded, pending)
}

// verifyHashTypes plans the --hash-type set one pass requests: the hash
// names behind every recorded algo, plus the capture set when pending
// rows need a first fingerprint. nil requests every hash the backend
// exposes — required when pending rows exist on a backend with no
// configured selection.
func verifyHashTypes(dest *config.Destination, recordedAlgos []string, pending bool) []string {
	set := map[string]bool{}
	for _, algo := range recordedAlgos {
		set[algoHashType(algo)] = true
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
		failed := len(rep.Missing) + len(rep.Mismatched) + len(rep.PacksMissing) + len(rep.PackMismatched)
		errMsg = fmt.Sprintf("%d object(s)/pack(s) failed verification on %q", failed, rep.Destination)
	}
	note := fmt.Sprintf("destination=%s objects=%d verified=%d fingerprinted=%d pending=%d mismatched=%d missing=%d unrecorded=%d packs=%d packs_verified=%d packs_fingerprinted=%d packs_pending=%d packs_mismatched=%d packs_missing=%d",
		rep.Destination, rep.Objects, rep.Verified, rep.Populated, rep.Pending, len(rep.Mismatched), len(rep.Missing), rep.Unrecorded,
		rep.Packs, rep.PacksVerified, rep.PacksPopulated, rep.PacksPending, len(rep.PackMismatched), len(rep.PacksMissing))
	if err := s.AppendRunAudit(ctx, store.RunAuditEntry{
		RunID: rep.RunID, Transition: store.TransitionVerifyDestination, Note: note,
	}); err != nil {
		return err
	}
	// A verification pass moves no content, so what it changes is the
	// fingerprints it recorded for the first time: the initial capture pass
	// over a destination stays visible, and the cadence passes that only
	// re-confirmed what was already recorded fold away as steady-state
	// noise (#182). A pass that found a mismatch or a missing object is
	// 'partial' or 'failed' and stays visible on status alone.
	changed := int64(rep.Populated + rep.PacksPopulated)
	if err := s.FinishRunChanged(ctx, rep.RunID, status, errMsg, int64(rep.Objects+rep.Packs), changed); err != nil {
		return fmt.Errorf("finish verify run %d: %w", rep.RunID, err)
	}
	return applyVerifyAlarm(ctx, s, rep, verifyErr)
}

// applyVerifyAlarm latches or clears the destination's standing alarm from
// this pass's outcome (#157, F30). A pass that detected a mismatch or a
// missing object/pack raises the alarm (idempotent — a re-detection keeps
// the original "in alarm since"); a clean pass auto-clears any standing
// alarm, recording the clear against this verify run. A pass that aborted
// (verifyErr != nil) proves nothing about the destination's integrity, so
// it neither raises nor clears — its failed run row is the record.
func applyVerifyAlarm(ctx context.Context, s *store.Store, rep *RemoteVerifyReport, verifyErr error) error {
	if verifyErr != nil {
		return nil
	}
	if !rep.Clean() {
		detail := fmt.Sprintf("objects mismatched=%d missing=%d, packs mismatched=%d missing=%d",
			len(rep.Mismatched), len(rep.Missing), len(rep.PackMismatched), len(rep.PacksMissing))
		// AlarmRaised reflects only a *newly* created latch, taken from the
		// atomic insert itself — so the CLI shouts on first detection but a
		// concurrent second raise (which found the latch already there)
		// stays quiet. No pre-read: that would be a TOCTOU race.
		raised, err := s.RaiseDestinationAlarm(ctx, rep.Destination, store.AlarmKindVerifyMismatch, detail, rep.RunID)
		if err != nil {
			return fmt.Errorf("raise standing alarm for %q: %w", rep.Destination, err)
		}
		rep.AlarmRaised = raised
		return nil
	}
	cleared, err := s.ClearDestinationAlarm(ctx, rep.Destination, rep.RunID, "")
	if err != nil {
		return fmt.Errorf("auto-clear standing alarm for %q: %w", rep.Destination, err)
	}
	rep.AlarmCleared = cleared
	return nil
}

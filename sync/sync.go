package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/runevents"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/syncproto"
	"github.com/mbertschler/squirrel/volmark"
)

// HistoryDirName is the directory at the destination, per volume, where
// overwritten files are moved to preserve destination immutability.
// rclone receives a path under this name as its --backup-dir argument.
// The dotfile prefix keeps it out of casual browsing without hiding it
// from `ls -a`.
const HistoryDirName = ".squirrel-history"

// ConflictsDirName is the agent-side equivalent of HistoryDirName for
// node syncs: when a peer-sync diff produces a `conflict` disposition,
// the receiver moves the losing version under
// .squirrel-conflicts/run-<id>/<path> and seeds an index row there.
// Mirrors agent.ConflictsDirName; duplicated rather than imported so
// non-sync packages (the CLI's `runs` listing, mainly) don't pull in
// the agent transitively.
const ConflictsDirName = ".squirrel-conflicts"

// Options shapes one Sync invocation.
type Options struct {
	// Shallow drops --checksum and --hash blake3 so rclone uses its default
	// size+mtime comparison. Faster but with no end-to-end integrity check.
	// Off by default — squirrel privileges integrity over speed.
	Shallow bool
	// DryRun forwards --dry-run to rclone. No bytes are transferred and no
	// runs row is written (the prerequisite check still happens).
	DryRun bool
	// Init authorises the writing of a fresh .squirrel-volume marker
	// when the destination's per-volume directory does not yet carry
	// one. Without Init, a missing marker is refused — the directory
	// might be a typo or an unrelated tree, and squirrel insists on
	// explicit consent before claiming it. Mismatching markers are
	// always refused regardless of Init.
	Init bool
	// OnRunID, if non-nil, is invoked once the runs row has been
	// allocated. Desktop callers use it to bridge the
	// "started a goroutine, want a runID" gap without polling. Not
	// invoked in DryRun mode (no row is written).
	OnRunID func(runID int64)
	// Progress, if non-nil, receives in-flight progress events derived
	// from rclone's periodic stats output. The callback is invoked
	// synchronously from rclone's stderr-reader goroutine; callers
	// must keep it cheap (non-blocking channel send is the canonical
	// shape). nil means no-op.
	Progress func(runevents.Progress)
}

// Report is the outcome of one Sync invocation. Volume and Destination
// echo the inputs for diagnostic clarity. RunID is the runs.id row that
// was inserted; zero in dry-run mode. RcloneResult is the parsed rclone
// summary, surfaced verbatim so callers can render whatever they need.
type Report struct {
	Volume       string
	Destination  string
	RunID        int64
	RcloneResult RunResult
	Status       string // success / partial / failed
	// FinishErr captures a failure to write the runs row's terminal state.
	// It is independent of rclone success — the bytes may have transferred
	// correctly but the audit-trail row got stuck in 'running'. Callers
	// should surface this distinctly from RcloneResult errors.
	FinishErr error
	// Warnings is a list of non-fatal advisories the CLI should surface.
	// Currently used for "source volume contains a reserved
	// .squirrel-history directory" so the user knows that content was
	// silently filtered from the upload.
	Warnings []string
	// NodeReceiverRunID is set on a successful node-sync handshake and
	// echoed in the CLI output so the operator can join the two halves
	// of one logical sync against the receiver's `squirrel runs`
	// listing. Zero for bucket syncs and for runs that failed before
	// /begin returned.
	NodeReceiverRunID int64
	// NodeVerify carries the receiver's verification report after a
	// node sync. Empty for bucket syncs.
	NodeVerify syncproto.VerifyResponse
	// NodeConflicts is non-empty when a node sync resolved one or more
	// `conflict` dispositions: paths where the receiver's prior bytes
	// were preserved under .squirrel-conflicts/run-<id>/ while the
	// initiator's bytes landed live. The records carry both the prior
	// and the new BLAKE3 plus the receiver-relative preserved path so
	// the CLI can render a meaningful "review at <path>" pointer.
	NodeConflicts []syncproto.ConflictDetail
	// NodePendingWarnings is the receiver's drift-detection advisory
	// from the handshake (#17): one line per audit run on the volume
	// since the last successful sync that flipped content
	// out-of-band. The CLI surfaces them prefixed with "peer reports"
	// so the operator can distinguish source-side warnings from
	// receiver-side ones.
	NodePendingWarnings []string
}

// RunPair is the single entry point for one sync invocation. It
// dispatches between bucket-destination and node-destination flows
// based on which slot of the Pair is populated. CLI callers use it
// directly so the per-Pair printing loop is a one-liner; the
// per-flavour functions (Sync, SyncNode) remain exported for tests
// and for callers that already have the typed destination in hand.
//
// Concurrency: both flows allocate the 'running' kind='sync' row via
// store.BeginSyncRunIfClear, which does the check + insert atomically
// inside a BEGIN IMMEDIATE transaction. Two concurrent RunPair calls
// against the same (volume, target) cannot both win — the loser sees
// the winner's row and returns the "already running" diagnostic from
// alreadyRunningErr. Stale 'running' rows from crashed runs keep
// blocking here until cleared by `squirrel runs fail` (#37).
func RunPair(ctx context.Context, s *store.Store, rcl *Rclone, p Pair, opts Options) (Report, error) {
	if p.IsNode() {
		return SyncNode(ctx, s, rcl, p.Volume, p.Node, opts)
	}
	return Sync(ctx, s, rcl, p.Volume, p.Destination, opts)
}

// alreadyRunningErr formats the diagnostic returned when a sync is
// refused because another run of the same (volume, target) is still in
// flight. Centralised so the bucket and peer paths surface identical
// wording.
func alreadyRunningErr(volName, target string, blocker *store.Run) error {
	return fmt.Errorf("sync of %s → %s already running (run=%d, started %s)",
		volName, target, blocker.ID,
		time.Unix(0, blocker.StartedAtNs).UTC().Format(time.RFC3339))
}

// Sync runs one (volume, destination) pair via rclone. It:
//  1. Checks the volume has been indexed (errors otherwise).
//  2. Atomically inserts a 'running' runs row (refusing if another sync
//     of the pair is already in flight) and defers its terminal update.
//  3. Composes rclone arguments: copy, integrity flags, backup-dir under
//     the destination's per-volume HistoryDirName/<run-id>/, and a filter
//     that hides .squirrel-history from rclone's comparison entirely.
//  4. Invokes rclone via the wrapper and finalises the run.
func Sync(ctx context.Context, s *store.Store, rcl *Rclone, vol *config.Volume, dest *config.Destination, opts Options) (rep Report, err error) {
	rep = Report{Volume: vol.Name, Destination: dest.Name}
	if w := historyDirInSourceWarning(vol); w != "" {
		rep.Warnings = append(rep.Warnings, w)
	}

	volID, err := requireIndexedVolume(ctx, s, vol)
	if err != nil {
		return rep, err
	}

	// Marker gate for local destinations. Remote destinations skip
	// this check for now — reading/writing a marker through rclone
	// adds out-of-band exec invocations that aren't worth the
	// complexity until we hit a real misconfiguration on a remote.
	// The dry-run path also skips: it never writes, and refusing a
	// dry-run on an uninitialised destination would prevent the
	// "preview what would happen" workflow.
	if !opts.DryRun && dest.Type == "local" {
		if err := ensureDestinationMarker(ctx, s, dest, vol.Name, opts.Init); err != nil {
			return rep, err
		}
	}

	runID, err := beginSyncRunGuarded(ctx, s, opts.DryRun, store.SyncRunSpec{
		VolumeID:    volID,
		Destination: dest.Name,
	}, vol.Name)
	if err != nil {
		return rep, err
	}
	if opts.OnRunID != nil && runID != 0 {
		opts.OnRunID(runID)
	}

	err = runRcloneOperation(ctx, s, rcl, opts.DryRun, runID, &rep, opts.Progress,
		func(runID int64) ([]string, error) {
			return buildRcloneArgs(vol, dest, runID, opts)
		})
	return rep, err
}

// beginSyncRunGuarded is the sync-allocator the bucket and peer paths
// share. It honours dry-run (returns 0 with no DB write) and delegates
// to store.BeginSyncRunIfClear for the atomic gate. A blocked attempt
// is rendered via alreadyRunningErr using the supplied volume name —
// the caller knows it, the store row only carries the destination
// string.
func beginSyncRunGuarded(ctx context.Context, s *store.Store, dryRun bool, spec store.SyncRunSpec, volName string) (int64, error) {
	if dryRun {
		return 0, nil
	}
	id, blocker, err := s.BeginSyncRunIfClear(ctx, spec)
	if err != nil {
		return 0, fmt.Errorf("begin sync run: %w", err)
	}
	if blocker != nil {
		return 0, alreadyRunningErr(volName, spec.Destination, blocker)
	}
	return id, nil
}

// runRcloneOperation is the shared scaffold for Sync and Restore: it
// invokes rclone via buildArgs, coerces a bare invocation failure into
// FatalError, and arranges the deferred FinishRun that mutates *rep
// before the helper returns. The runs row is allocated by the caller
// (Sync uses the guarded sync allocator; Restore uses beginRestoreRun)
// so the gate logic stays out of the rclone scaffold.
func runRcloneOperation(
	ctx context.Context,
	s *store.Store,
	rcl *Rclone,
	dryRun bool,
	runID int64,
	rep *Report,
	progress func(runevents.Progress),
	buildArgs func(runID int64) ([]string, error),
) (err error) {
	rep.RunID = runID
	defer func() {
		finishRun(ctx, s, dryRun, runID, rep)
	}()

	args, err := buildArgs(runID)
	if err != nil {
		// Synthesise a FailedFile entry so the deferred finishRun
		// writes a meaningful err message into the runs row — otherwise
		// `squirrel runs` shows "failed" with no reason, hiding the
		// (e.g.) runID=0 guard from forensic readers.
		rep.RcloneResult.FatalError = true
		rep.RcloneResult.FailedFiles = []FailedFile{{Message: err.Error()}}
		return err
	}
	rep.RcloneResult, err = rcl.RunWithProgress(ctx, progress, args...)
	if err != nil && rep.RcloneResult.Errors == 0 && !rep.RcloneResult.FatalError {
		// Invocation failed without a parseable error count: treat as fatal.
		rep.RcloneResult.FatalError = true
	}
	if err != nil {
		return fmt.Errorf("rclone: %w", err)
	}
	return nil
}

// requireIndexedVolume looks up the volume row by name and ensures at
// least one success-or-partial index run exists for it. Sync of an
// unindexed volume is refused: without an index, we have no record of
// what should be at the destination after the run.
func requireIndexedVolume(ctx context.Context, s *store.Store, vol *config.Volume) (int64, error) {
	v, err := s.GetVolumeByName(ctx, vol.Name)
	if err != nil {
		if store.IsNotFound(err) {
			return 0, fmt.Errorf("volume %q has never been indexed — run `squirrel index %s` first", vol.Name, vol.Name)
		}
		return 0, fmt.Errorf("lookup volume %q: %w", vol.Name, err)
	}
	if _, err := s.LatestSuccessfulIndexRun(ctx, v.ID); err != nil {
		if store.IsNotFound(err) {
			return 0, fmt.Errorf("volume %q has no successful index run — run `squirrel index %s` first", vol.Name, vol.Name)
		}
		return 0, fmt.Errorf("lookup latest index run: %w", err)
	}
	return v.ID, nil
}

// beginRestoreRun inserts a kind='restore' runs row, unless dryRun is
// set in which case it returns (0, nil) and no row is written. Restore
// is not gated against concurrency the way sync is — the destination is
// the read side here, and parallel restores into separate ToPath
// targets are a legitimate workflow.
func beginRestoreRun(ctx context.Context, s *store.Store, dryRun bool, volID int64, destName string) (int64, error) {
	if dryRun {
		return 0, nil
	}
	id, err := s.BeginRun(ctx, store.RunKindRestore, volID, destName)
	if err != nil {
		return 0, fmt.Errorf("begin restore run: %w", err)
	}
	return id, nil
}

// finishRun is the deferred terminal-state writer shared by Sync and
// Restore. A FinishRun failure would otherwise leave the run row stuck
// in 'running' and only surface during the next `squirrel runs`
// listing; recording it on rep.FinishErr lets the CLI surface it next
// to the rclone outcome on this very run.
func finishRun(ctx context.Context, s *store.Store, dryRun bool, runID int64, rep *Report) {
	rep.Status = deriveStatus(rep.RcloneResult)
	if dryRun || runID == 0 {
		return
	}
	errMsg := ""
	if rep.Status == store.RunStatusFailed && len(rep.RcloneResult.FailedFiles) > 0 {
		errMsg = rep.RcloneResult.FailedFiles[0].Message
	}
	fileCount := rep.RcloneResult.Transferred + rep.RcloneResult.Checked
	if err := s.FinishRun(ctx, runID, rep.Status, errMsg, fileCount); err != nil {
		rep.FinishErr = err
	}
}

// historyDirInSourceWarning returns a one-line advisory when the source
// volume already contains a literal .squirrel-history directory. Sync
// filters it out of the rclone transfer so it can't pollute the
// destination tree, but the user should know that some local content is
// being silently skipped under the reserved name.
func historyDirInSourceWarning(vol *config.Volume) string {
	if _, err := os.Stat(filepath.Join(vol.Path, HistoryDirName)); err != nil {
		return ""
	}
	return fmt.Sprintf("volume %q contains a reserved %s/ directory in its source tree — its contents will not be uploaded; rename or move the directory if you want it synced",
		vol.Name, HistoryDirName)
}

// validateLocalVolumeMarker is the restore-time guard against
// pointing the operation at the wrong local directory. Unlike sync's
// --init flag, restore deliberately offers no bootstrap: the safety
// property is "refuse to overwrite a tree we haven't previously
// claimed." A fresh local volume must run `squirrel index <vol>`
// first (which writes the marker) before restoring into it.
func validateLocalVolumeMarker(vol *config.Volume) error {
	err := volmark.Validate(vol.Path, vol.Name)
	if err == nil {
		return nil
	}
	if errors.Is(err, volmark.ErrMissing) {
		return fmt.Errorf("volume %q at %s has no %s marker — refusing to restore (run `squirrel index %s` first or use `--to <scratch>` to restore elsewhere)", vol.Name, vol.Path, volmark.MarkerName, vol.Name)
	}
	if _, ok := errors.AsType[*volmark.ErrMismatch](err); ok {
		return fmt.Errorf("volume %q: %w (refusing to restore over a different volume's tree)", vol.Name, err)
	}
	return fmt.Errorf("volume %q marker check: %w", vol.Name, err)
}

// ensureDestinationMarker validates (or, with Init, writes) the
// .squirrel-volume marker at <dest.Root>/<vol.Name>/. The directory
// is created on first --init so the marker can land even when the
// destination tree is empty; on every subsequent sync the marker must
// already match the volume name. A mismatched marker is always
// refused, regardless of Init: that path almost certainly points at a
// different squirrel volume, and overwriting its marker would erase
// the trail that distinguishes the two.
//
// Restricted to dest.Type=="local" for now; remote destinations need
// a separate rclone-mediated read/write path and are tracked as a
// follow-up.
func ensureDestinationMarker(ctx context.Context, s *store.Store, dest *config.Destination, volumeName string, init bool) error {
	root := filepath.Join(dest.Root, volumeName)
	err := volmark.Validate(root, volumeName)
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*volmark.ErrMismatch](err); ok {
		return fmt.Errorf("destination %q: %w (refuse to init over a different volume's tree)", dest.Name, err)
	}
	if !errors.Is(err, volmark.ErrMissing) {
		return fmt.Errorf("destination %q marker check: %w", dest.Name, err)
	}
	if !init {
		return fmt.Errorf("destination %q at %s has no %s marker — re-run with --init to bootstrap (refusing in case the root is a typo)", dest.Name, root, volmark.MarkerName)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("destination %q: mkdir %s: %w", dest.Name, root, err)
	}
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		return fmt.Errorf("destination %q: resolve self node: %w", dest.Name, err)
	}
	if err := volmark.Write(root, volmark.Marker{
		Volume:    volumeName,
		Node:      self.Name,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return fmt.Errorf("destination %q: %w", dest.Name, err)
	}
	return nil
}

func deriveStatus(r RunResult) string {
	if r.FatalError {
		return store.RunStatusFailed
	}
	if r.Errors > 0 {
		return store.RunStatusPartial
	}
	return store.RunStatusSuccess
}

// buildRcloneArgs composes the rclone command-line for one sync invocation.
// Layout reminder: the source is the absolute volume path; the destination
// is <dest>:<root>/<volume>/, with .squirrel-history living *inside* that
// per-volume subtree so the destination is fully self-describing.
//
// A real (non-dry-run) sync must carry a non-zero runID — the backup-dir
// uses it to bucket overwritten files into run-<id>/, and runID=0 would
// collide every overwrite into the run-dry-run/ placeholder bucket. The
// allocator guarantees this today, but we refuse here as a defensive
// guard against any future regression that bypasses the allocator.
func buildRcloneArgs(vol *config.Volume, dest *config.Destination, runID int64, opts Options) ([]string, error) {
	if !opts.DryRun && runID == 0 {
		return nil, fmt.Errorf("sync: refusing to build rclone args with runID=0 outside dry-run mode")
	}
	srcArg := withTrailingSlash(vol.Path)
	dstArg := destinationVolumeURI(dest, vol.Name)
	backupDir := backupDirURI(dest, vol.Name, runID, opts.DryRun)

	args := []string{
		"copy",
		"--backup-dir", backupDir,
		// The filter is bidirectional in rclone — both source and
		// destination listings have .squirrel-history hidden from
		// comparison, so a user volume that incidentally contains such a
		// directory is silently excluded rather than uploaded. (We also
		// warn at index time so the user isn't surprised.)
		"--filter", "- /" + HistoryDirName + "/**",
		// The .squirrel-volume marker is per-side metadata: source and
		// destination each carry their own to identify the tree as
		// squirrel-owned. Filtering it out of comparison ensures the
		// source's marker doesn't propagate over the destination's.
		"--filter", "- /" + volmark.MarkerName,
	}
	if !opts.Shallow {
		args = append(args, "--checksum", "--hash", "blake3")
	}
	if opts.DryRun {
		args = append(args, "--dry-run")
	}
	args = append(args, srcArg, dstArg)
	return args, nil
}

// withTrailingSlash ensures the path ends in '/'. rclone treats
// "src" and "src/" identically for copy, but explicitness avoids drift if
// we later switch to commands that distinguish (e.g. `move`).
func withTrailingSlash(p string) string {
	if len(p) == 0 || p[len(p)-1] == '/' {
		return p
	}
	return p + "/"
}

// destinationVolumeURI returns the rclone destination spec for the given
// volume under dest. For type=local this is an absolute filesystem path;
// for other types it is "<name>:<root>/<volume>/".
func destinationVolumeURI(dest *config.Destination, volumeName string) string {
	switch dest.Type {
	case "local":
		return filepath.ToSlash(filepath.Join(dest.Root, volumeName)) + "/"
	default:
		joined := path.Join(dest.Root, volumeName)
		return dest.Name + ":" + joined + "/"
	}
}

// backupDirURI returns the destination spec for rclone's --backup-dir for
// this run. Path is per-volume per-run; for dry-run we still pass a
// placeholder since rclone insists on the flag if --backup-dir is wanted
// in the real run (we just never write to it).
func backupDirURI(dest *config.Destination, volumeName string, runID int64, dryRun bool) string {
	id := strconv.FormatInt(runID, 10)
	if dryRun || runID == 0 {
		id = "dry-run"
	}
	subpath := path.Join(volumeName, HistoryDirName, "run-"+id)
	switch dest.Type {
	case "local":
		return filepath.ToSlash(filepath.Join(dest.Root, subpath))
	default:
		return dest.Name + ":" + path.Join(dest.Root, subpath)
	}
}

// EnsureMinVersion checks the installed rclone against MinRcloneVersion.
// When shallow=true the integrity flags are not used, so a below-floor
// rclone is acceptable and we only warn. When shallow=false we'd be
// about to invoke --hash blake3, which only exists in rclone ≥ 1.66;
// refuse rather than hand off a doomed invocation to rclone for a
// confusing stderr message. The actual decision lives in
// checkMinVersion so tests don't need a rclone-binary-that-lies fixture.
func EnsureMinVersion(ctx context.Context, rcl *Rclone, out io.Writer, shallow bool) error {
	v, err := rcl.Version(ctx)
	if err != nil {
		return err
	}
	return checkMinVersion(v, out, shallow)
}

func checkMinVersion(v Version, out io.Writer, shallow bool) error {
	if v.AtLeast(MinRcloneVersion) {
		return nil
	}
	if shallow {
		fmt.Fprintf(out, "warning: rclone %s is below the supported floor %s; --shallow keeps this run working but consider upgrading\n", v, MinRcloneVersion)
		return nil
	}
	return fmt.Errorf("rclone %s is below the supported floor %s — --hash blake3 is unavailable; upgrade rclone or pass --shallow", v, MinRcloneVersion)
}

// PairsFor builds the list of (volume, target) pairs to sync given
// optional volume-name and destination/node-name filters. An empty
// volumeName means "every volume with sync_to declared"; an empty
// destinationName means "every target on the matched volume(s)". The
// destinationName matches against both buckets and nodes — they share
// a flat namespace from the user's perspective.
func PairsFor(cfg *config.Config, volumeName, destinationName string) ([]Pair, error) {
	if volumeName != "" {
		if _, ok := cfg.Volumes[volumeName]; !ok {
			return nil, fmt.Errorf("unknown volume %q", volumeName)
		}
	}
	if destinationName != "" {
		_, bucketOK := cfg.Destinations[destinationName]
		_, nodeOK := cfg.Nodes[destinationName]
		if !bucketOK && !nodeOK {
			return nil, fmt.Errorf("unknown destination or node %q", destinationName)
		}
	}
	var out []Pair
	for vname, vol := range cfg.Volumes {
		if volumeName != "" && vname != volumeName {
			continue
		}
		for _, dname := range vol.SyncTo {
			if destinationName != "" && dname != destinationName {
				continue
			}
			if dest, ok := cfg.Destinations[dname]; ok {
				out = append(out, Pair{Volume: vol, Destination: dest})
				continue
			}
			if node, ok := cfg.Nodes[dname]; ok {
				out = append(out, Pair{Volume: vol, Node: node})
				continue
			}
			return nil, fmt.Errorf("volume %s references destination or node %q not in config (config validation should have caught this)", vname, dname)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no (volume, destination) pairs match the request — check sync_to in your config")
	}
	return out, nil
}

// Pair is one matched (volume, target) pair returned by PairsFor.
// Exactly one of Destination / Node is non-nil; callers dispatch
// accordingly.
type Pair struct {
	Volume      *config.Volume
	Destination *config.Destination
	Node        *config.Node
}

// TargetName returns the name of whichever target slot is filled.
// Used by the CLI for per-pair output framing.
func (p Pair) TargetName() string {
	if p.Destination != nil {
		return p.Destination.Name
	}
	if p.Node != nil {
		return p.Node.Name
	}
	return ""
}

// IsNode reports whether this pair targets a peer node (vs. a bucket).
func (p Pair) IsNode() bool { return p.Node != nil }

// RestoreOptions shape one Restore invocation. ToPath overrides the local
// target directory; when empty, the volume's declared path is used. The
// override is for "pull a recovery copy somewhere else" — squirrel won't
// silently clobber the live volume unless explicitly pointed at it.
// IncludeFromFile, when non-empty, is a path to a newline-delimited
// listing of volume-relative paths and gets forwarded to rclone as
// --files-from-raw. Used by `restore --from <node>` to ship only that
// node's source-attributed paths back to the local tree. We use the
// raw variant (matching sync/node.go's writeFilesFrom flow) so paths
// beginning with `#`/`;` or with surrounding whitespace are passed
// through verbatim — the processed --files-from mode would treat
// those as comments or trim them.
type RestoreOptions struct {
	ToPath          string
	Shallow         bool
	DryRun          bool
	IncludeFromFile string
}

// Restore reverses Sync: it copies from the destination's per-volume tree
// back to the local filesystem. Like Sync it records a runs row, but with
// kind='restore'. Restore is the opposite of additive — the rclone copy
// will overwrite whatever exists at the target path on a hash mismatch.
// Callers are expected to point ToPath at an empty / scratch directory
// unless they explicitly intend to restore in place.
func Restore(ctx context.Context, s *store.Store, rcl *Rclone, vol *config.Volume, dest *config.Destination, opts RestoreOptions) (rep Report, err error) {
	rep = Report{Volume: vol.Name, Destination: dest.Name}

	// Local target marker check: when restoring into the live volume
	// path (the dangerous, in-place direction), insist on a marker
	// that names this volume. A missing or mismatched marker is the
	// strongest signal we have that vol.Path is a typo or unrelated
	// tree, and overwriting it via rclone would be irreversible. The
	// --to override skips this check because the operator is
	// explicitly redirecting to a scratch directory and accepts
	// responsibility for the target.
	if !opts.DryRun && opts.ToPath == "" {
		if err := validateLocalVolumeMarker(vol); err != nil {
			return rep, err
		}
	}

	// We deliberately don't require an existing index for restore: the
	// destination is the source of truth in this direction, and a fresh
	// laptop may have no DB rows yet. We still create a volumes row so
	// the runs row's FK resolves.
	v, err := getOrCreateVolumeForRestore(ctx, s, vol)
	if err != nil {
		return rep, err
	}

	runID, err := beginRestoreRun(ctx, s, opts.DryRun, v.ID, dest.Name)
	if err != nil {
		return rep, err
	}

	err = runRcloneOperation(ctx, s, rcl, opts.DryRun, runID, &rep, nil,
		func(_ int64) ([]string, error) {
			return buildRestoreArgs(vol, dest, opts), nil
		})
	return rep, err
}

func getOrCreateVolumeForRestore(ctx context.Context, s *store.Store, vol *config.Volume) (store.Volume, error) {
	if v, err := s.GetVolumeByName(ctx, vol.Name); err == nil {
		// Mirror index.resolveNamedVolume's mismatch guard: a stale
		// volumes.path would otherwise cause the next `squirrel index`
		// to refuse with the same conflict, leaving the user wondering
		// when the drift was introduced. Surface it here at the moment
		// of writing into the run row.
		if v.Path != vol.Path {
			return store.Volume{}, fmt.Errorf("volume %q is at %q in the DB but config says %q — resolve the conflict before restoring", vol.Name, v.Path, vol.Path)
		}
		return v, nil
	} else if !store.IsNotFound(err) {
		return store.Volume{}, fmt.Errorf("lookup volume: %w", err)
	}
	v, err := s.CreateVolume(ctx, vol.Name, vol.Path)
	if err != nil {
		return store.Volume{}, fmt.Errorf("create volume %q: %w", vol.Name, err)
	}
	return v, nil
}

// buildRestoreArgs flips the source/destination of sync: source is
// <dest>:<root>/<volume>/, destination is the local volume path (or
// override). The .squirrel-history filter applies in the listing-based
// flow so the destination's historical snapshots don't land in the
// user's tree; the include-list flow doesn't need it because the
// list is the authoritative subset. rclone in fact rejects --filter
// alongside --files-from-raw ("the usage of --files-from-raw overrides
// all other filters") so we must pick one or the other.
//
// Restore does not pass --backup-dir: the local target is the recovery
// surface, and the user opted in to overwrites by invoking restore.
// Squirrel-side append-only semantics live in the destination tree, not
// on the local filesystem.
func buildRestoreArgs(vol *config.Volume, dest *config.Destination, opts RestoreOptions) []string {
	srcArg := destinationVolumeURI(dest, vol.Name)
	target := vol.Path
	if opts.ToPath != "" {
		target = opts.ToPath
	}
	dstArg := withTrailingSlash(target)

	args := []string{"copy"}
	if opts.IncludeFromFile != "" {
		args = append(args, "--files-from-raw", opts.IncludeFromFile)
	} else {
		args = append(args, "--filter", "- /"+HistoryDirName+"/**")
		args = append(args, "--filter", "- /"+volmark.MarkerName)
	}
	if !opts.Shallow {
		args = append(args, "--checksum", "--hash", "blake3")
	}
	if opts.DryRun {
		args = append(args, "--dry-run")
	}
	args = append(args, srcArg, dstArg)
	return args
}

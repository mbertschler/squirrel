package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// HistoryDirName is the directory at the destination, per volume, where
// overwritten files are moved to preserve destination immutability.
// rclone receives a path under this name as its --backup-dir argument.
// The dotfile prefix keeps it out of casual browsing without hiding it
// from `ls -a`.
const HistoryDirName = ".squirrel-history"

// Options shapes one Sync invocation.
type Options struct {
	// Shallow drops --checksum and --hash blake3 so rclone uses its default
	// size+mtime comparison. Faster but with no end-to-end integrity check.
	// Off by default — squirrel privileges integrity over speed.
	Shallow bool
	// DryRun forwards --dry-run to rclone. No bytes are transferred and no
	// runs row is written (the prerequisite check still happens).
	DryRun bool
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
}

// Sync runs one (volume, destination) pair via rclone. It:
//  1. Checks the volume has been indexed (errors otherwise).
//  2. Inserts a runs row and defers its terminal update.
//  3. Composes rclone arguments: copy, integrity flags, backup-dir under
//     the destination's per-volume HistoryDirName/<run-id>/, and a filter
//     that hides .squirrel-history from rclone's comparison entirely.
//  4. Invokes rclone via the wrapper and finalises the run.
func Sync(ctx context.Context, s *store.Store, rcl *Rclone, vol *config.Volume, dest *config.Destination, opts Options) (rep Report, err error) {
	rep = Report{Volume: vol.Name, Destination: dest.Name}

	volID, err := requireIndexedVolume(ctx, s, vol)
	if err != nil {
		return rep, err
	}

	runID, err := beginSyncRun(ctx, s, opts.DryRun, volID, dest.Name)
	if err != nil {
		return rep, err
	}
	rep.RunID = runID
	// Named returns let this deferred call mutate rep.Status (and persist
	// the FinishRun row) after the function body sets rep.RcloneResult.
	defer func() {
		finishSyncRun(ctx, s, opts.DryRun, runID, &rep)
	}()

	args := buildRcloneArgs(vol, dest, runID, opts)
	rep.RcloneResult, err = rcl.Run(ctx, args...)
	if err != nil && rep.RcloneResult.Errors == 0 && !rep.RcloneResult.FatalError {
		// Invocation failed without a parseable error count: treat as fatal.
		rep.RcloneResult.FatalError = true
	}
	if err != nil {
		return rep, fmt.Errorf("rclone: %w", err)
	}
	return rep, nil
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

func beginSyncRun(ctx context.Context, s *store.Store, dryRun bool, volID int64, destName string) (int64, error) {
	if dryRun {
		return 0, nil
	}
	id, err := s.BeginRun(ctx, store.RunKindSync, volID, destName)
	if err != nil {
		return 0, fmt.Errorf("begin sync run: %w", err)
	}
	return id, nil
}

// finishSyncRun is the deferred terminal-state writer. It is intentionally
// silent on its own errors: the caller already has rep.Status and the
// underlying rclone output; corrupting the report with a FinishRun failure
// helps no one. Surfacing via stderr is the CLI layer's job.
func finishSyncRun(ctx context.Context, s *store.Store, dryRun bool, runID int64, rep *Report) {
	rep.Status = deriveStatus(rep.RcloneResult)
	if dryRun || runID == 0 {
		return
	}
	errMsg := ""
	if rep.Status == store.RunStatusFailed && len(rep.RcloneResult.FailedFiles) > 0 {
		errMsg = rep.RcloneResult.FailedFiles[0].Message
	}
	fileCount := rep.RcloneResult.Transferred + rep.RcloneResult.Checked
	_ = s.FinishRun(ctx, runID, rep.Status, errMsg, fileCount)
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
func buildRcloneArgs(vol *config.Volume, dest *config.Destination, runID int64, opts Options) []string {
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

// EnsureMinVersion verifies the installed rclone is at MinRcloneVersion or
// above; below that, --hash blake3 won't work. A version below the floor
// is logged as a warning (not a hard error) so users can still attempt a
// shallow sync without the integrity flags.
func EnsureMinVersion(ctx context.Context, rcl *Rclone, out io.Writer) error {
	v, err := rcl.Version(ctx)
	if err != nil {
		return err
	}
	if !v.AtLeast(MinRcloneVersion) {
		fmt.Fprintf(out, "warning: rclone %s is below the supported floor %s — --hash blake3 will fail; consider --shallow or upgrade rclone\n", v, MinRcloneVersion)
	}
	return nil
}

// PairsFor builds the list of (volume, destination) pairs to sync given
// optional volume-name and destination-name filters. An empty volumeName
// means "every volume with sync_to declared"; an empty destinationName
// means "every destination on the matched volume(s)". Validation: every
// non-empty filter must reference a name that exists in cfg, and the
// pair must be declared in the volume's sync_to list.
func PairsFor(cfg *config.Config, volumeName, destinationName string) ([]Pair, error) {
	if volumeName != "" {
		if _, ok := cfg.Volumes[volumeName]; !ok {
			return nil, fmt.Errorf("unknown volume %q", volumeName)
		}
	}
	if destinationName != "" {
		if _, ok := cfg.Destinations[destinationName]; !ok {
			return nil, fmt.Errorf("unknown destination %q", destinationName)
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
			dest, ok := cfg.Destinations[dname]
			if !ok {
				return nil, fmt.Errorf("volume %s references destination %q not in config (config validation should have caught this)", vname, dname)
			}
			out = append(out, Pair{Volume: vol, Destination: dest})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no (volume, destination) pairs match the request — check sync_to in your config")
	}
	return out, nil
}

// Pair is one matched (volume, destination) pair returned by PairsFor.
type Pair struct {
	Volume      *config.Volume
	Destination *config.Destination
}

// RestoreOptions shape one Restore invocation. ToPath overrides the local
// target directory; when empty, the volume's declared path is used. The
// override is for "pull a recovery copy somewhere else" — squirrel won't
// silently clobber the live volume unless explicitly pointed at it.
type RestoreOptions struct {
	ToPath  string
	Shallow bool
	DryRun  bool
}

// Restore reverses Sync: it copies from the destination's per-volume tree
// back to the local filesystem. Like Sync it records a runs row, but with
// kind='restore'. Restore is the opposite of additive — the rclone copy
// will overwrite whatever exists at the target path on a hash mismatch.
// Callers are expected to point ToPath at an empty / scratch directory
// unless they explicitly intend to restore in place.
func Restore(ctx context.Context, s *store.Store, rcl *Rclone, vol *config.Volume, dest *config.Destination, opts RestoreOptions) (rep Report, err error) {
	rep = Report{Volume: vol.Name, Destination: dest.Name}

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
	rep.RunID = runID
	defer func() {
		finishSyncRun(ctx, s, opts.DryRun, runID, &rep)
	}()

	args := buildRestoreArgs(vol, dest, opts)
	rep.RcloneResult, err = rcl.Run(ctx, args...)
	if err != nil && rep.RcloneResult.Errors == 0 && !rep.RcloneResult.FatalError {
		rep.RcloneResult.FatalError = true
	}
	if err != nil {
		return rep, fmt.Errorf("rclone: %w", err)
	}
	return rep, nil
}

func getOrCreateVolumeForRestore(ctx context.Context, s *store.Store, vol *config.Volume) (store.Volume, error) {
	if v, err := s.GetVolumeByName(ctx, vol.Name); err == nil {
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

// buildRestoreArgs flips the source/destination of sync: source is
// <dest>:<root>/<volume>/, destination is the local volume path (or
// override). The same .squirrel-history filter applies — we don't want
// the destination's historical snapshots to land in the user's tree.
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

	args := []string{
		"copy",
		"--filter", "- /" + HistoryDirName + "/**",
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


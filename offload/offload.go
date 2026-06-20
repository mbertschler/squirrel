// Package offload deletes local file bytes whose content is provably
// durable on every target the volume's offload policy requires. It is
// the only place squirrel ever deletes user data: the gate is evaluated
// entirely offline against the local index (the durability version
// vectors in destination_run_ids, including components pulled from
// peers), every candidate is re-verified against the on-disk bytes
// immediately before the unlink, and the operation is recorded as a
// kind='offload' run with each touched row flipped present → offloaded.
package offload

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/volmark"
)

// Options shapes one Offload invocation.
type Options struct {
	// Name is the config-declared volume name.
	Name string
	// Paths are volume-relative path or prefix selectors. A selector
	// matches the file at exactly that path plus every file under it as
	// a directory prefix; "." selects the whole volume. Multiple
	// selectors are ORed.
	Paths []string
	// OlderThan, when positive, narrows the selection to files whose
	// indexed mtime is older than now − OlderThan. ANDed with Paths.
	OlderThan time.Duration
	// Require is the volume's offload policy: the target names whose
	// durability vectors must each cover a file's content origin before
	// its bytes may be deleted. Offload refuses to run when empty — the
	// policy is an explicit precondition, there is no default target
	// set.
	Require []string
	// DryRun evaluates and reports the per-file gate decisions from the
	// index alone: no runs row, no file reads, no deletions, no status
	// flips. Disk-drift checks only happen on a real run, immediately
	// before each unlink.
	DryRun bool
}

// Outcome classifies one file's result.
type Outcome int

const (
	// OutcomeOffloaded: the gate passed, the on-disk bytes matched the
	// index exactly, and the file was unlinked and recorded (in dry-run
	// mode: the gate passed and the file would be offloaded).
	OutcomeOffloaded Outcome = iota
	// OutcomeNotDurable: at least one required target's vector fails to
	// cover the file's content origin; nothing was touched.
	OutcomeNotDurable
	// OutcomeDrift: the on-disk state disagrees with the indexed row
	// (presence, type, size, mtime, or content hash) — the disk is
	// newer than the index, so the file was skipped. Re-index, re-sync,
	// and re-run to offload it.
	OutcomeDrift
	// OutcomeError: an operational failure (open, unlink, or the status
	// flip) left this file unprocessed; details in Reasons.
	OutcomeError
)

// FileResult is one per-file decision; Results are reported in path
// order.
type FileResult struct {
	Path    string
	Outcome Outcome
	// Reasons carries the per-target gate failures for
	// OutcomeNotDurable (one entry per failing target) and the single
	// drift or error detail otherwise. Empty for OutcomeOffloaded.
	Reasons []string
}

// Report summarises one Offload invocation. Offloaded + NotDurable +
// Drift + Errors equals len(Results).
type Report struct {
	// RunID is the kind='offload' runs row recorded for this
	// invocation. Zero in dry-run mode.
	RunID      int64
	Offloaded  int
	NotDurable int
	Drift      int
	Errors     int
	Results    []FileResult
	// SelectorMisses lists path selectors that matched no present file
	// — usually a typo'd path — so a no-op invocation explains itself.
	SelectorMisses []string
	// FinishErr is set when the terminal runs-row write failed; the
	// per-file work already happened and stands.
	FinishErr error
}

func (r *Report) record(res FileResult) {
	switch res.Outcome {
	case OutcomeOffloaded:
		r.Offloaded++
	case OutcomeNotDurable:
		r.NotDurable++
	case OutcomeDrift:
		r.Drift++
	case OutcomeError:
		r.Errors++
	}
	r.Results = append(r.Results, res)
}

// Offload runs the durability-gated deletion against the volume rooted
// at root. Per-file refusals (gate failures, disk drift) are reported
// on the Report and never abort the run; per-file operational errors
// are likewise reported and counted, leaving the runs row 'partial'. A
// returned error is fatal — preconditions failed or the run had to stop
// — and finalises the runs row as 'failed' with whatever per-file
// progress the Report carries.
func Offload(ctx context.Context, s *store.Store, root string, opts Options) (report Report, err error) {
	selectors, err := validateOptions(opts)
	if err != nil {
		return Report{}, err
	}
	vol, err := resolveVolume(ctx, s, opts.Name, root)
	if err != nil {
		return Report{}, err
	}
	g, err := loadGate(ctx, s, vol.ID, opts.Require)
	if err != nil {
		return Report{}, err
	}
	rows, err := s.LoadVolumeIndex(ctx, vol.ID)
	if err != nil {
		return Report{}, err
	}
	candidates, misses := selectCandidates(rows, selectors, opts.OlderThan)
	report.SelectorMisses = misses

	if opts.DryRun {
		err := evaluateOnly(ctx, g, candidates, &report)
		return report, err
	}

	runID, err := beginRun(ctx, s, vol.ID, opts.Name)
	if err != nil {
		return report, err
	}
	report.RunID = runID
	defer func() { finishRun(ctx, s, runID, &report, err) }()

	err = offloadFiles(ctx, s, g, root, vol.ID, runID, candidates, &report)
	return report, err
}

// validateOptions enforces the two invocation preconditions — an
// explicit policy and an explicit selector — and normalises the path
// selectors.
func validateOptions(opts Options) ([]string, error) {
	if len(opts.Require) == 0 {
		return nil, fmt.Errorf("volume %q declares no offload policy; offload refuses to delete without an explicit list of required targets (offload_requires)", opts.Name)
	}
	if opts.OlderThan < 0 {
		return nil, fmt.Errorf("--older-than %s is negative; the age cutoff must be a positive duration", opts.OlderThan)
	}
	if len(opts.Paths) == 0 && opts.OlderThan == 0 {
		return nil, errors.New(`offload needs a selector: volume-relative paths/prefixes ("." for the whole volume) and/or an --older-than age`)
	}
	return cleanSelectors(opts.Paths)
}

// cleanSelectors normalises the volume-relative selectors and refuses
// anything that could reach outside the volume root.
func cleanSelectors(paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			return nil, errors.New(`empty path selector (use "." to select the whole volume)`)
		}
		if filepath.IsAbs(p) {
			return nil, fmt.Errorf("selector %q must be volume-relative", p)
		}
		c := path.Clean(filepath.ToSlash(p))
		if c == ".." || strings.HasPrefix(c, "../") {
			return nil, fmt.Errorf("selector %q escapes the volume root", p)
		}
		if c == "." && strings.Contains(p, "..") {
			return nil, fmt.Errorf(`selector %q collapses to the whole volume; spell out "." to select everything`, p)
		}
		out = append(out, c)
	}
	return out, nil
}

// resolveVolume looks up the volume row by its config-declared name and
// cross-checks both identities the deletion is about to trust: the DB
// row's recorded path must equal the root the caller resolved from
// config, and the on-disk .squirrel-volume marker must name this
// volume. Either mismatch means config, index, and disk disagree about
// what tree this is — exactly the state in which a delete must refuse.
func resolveVolume(ctx context.Context, s *store.Store, name, root string) (store.Volume, error) {
	v, err := s.GetVolumeByName(ctx, name)
	if err != nil {
		if store.IsNotFound(err) {
			return store.Volume{}, fmt.Errorf("volume %q has no index rows; index it before offloading", name)
		}
		return store.Volume{}, fmt.Errorf("lookup volume %q: %w", name, err)
	}
	if v.Path != root {
		return store.Volume{}, fmt.Errorf("volume %q is at %q in the DB but config says %q — resolve the conflict before offloading", name, v.Path, root)
	}
	if err := volmark.Validate(root, name); err != nil {
		return store.Volume{}, fmt.Errorf("volume marker at %s: %w", root, err)
	}
	return v, nil
}

// reservedSubtrees are the squirrel-owned preservation directories.
// They are excluded from sync transfers and from
// AdvanceDestinationVector's present set, so a row under them can carry
// an origin coordinate the vectors cover through *other* paths' content
// even though these bytes never travelled — the gate math is unsound
// for them, and they hold preserved history besides. Offload never
// selects them.
var reservedSubtrees = []string{
	".squirrel-history",
	".squirrel-conflicts",
	".squirrel-restore-history",
	".squirrel-index",
}

func underReservedSubtree(p string) bool {
	for _, r := range reservedSubtrees {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// selectCandidates filters the volume's live rows down to the offload
// candidates: status 'present', outside the reserved subtrees, matching at
// least one path selector (none means every path), and — when olderThan
// is set — with an indexed mtime older than the cutoff. The age cutoff
// is applied after selector-hit tracking so a selector that only
// matched younger files still counts as matched. Candidates come back
// in path order for deterministic reports.
func selectCandidates(rows map[string]store.FileRow, selectors []string, olderThan time.Duration) ([]store.FileRow, []string) {
	ageFiltered := olderThan > 0
	var cutoffNs int64
	if ageFiltered {
		cutoffNs = time.Now().Add(-olderThan).UnixNano()
	}
	hit := make(map[string]bool, len(selectors))
	var out []store.FileRow
	for p, row := range rows {
		if row.Status != store.StatusPresent || underReservedSubtree(p) {
			continue
		}
		if len(selectors) > 0 {
			sel, ok := matchSelector(p, selectors)
			if !ok {
				continue
			}
			hit[sel] = true
		}
		if ageFiltered && row.MtimeNs >= cutoffNs {
			continue
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	var misses []string
	for _, sel := range selectors {
		if !hit[sel] {
			misses = append(misses, sel)
		}
	}
	return out, misses
}

// matchSelector returns the first selector that matches p: "." matches
// everything, an exact path matches itself, and any selector matches
// the files under it as a directory prefix.
func matchSelector(p string, selectors []string) (string, bool) {
	for _, sel := range selectors {
		if sel == "." || p == sel || strings.HasPrefix(p, sel+"/") {
			return sel, true
		}
	}
	return "", false
}

// evaluateOnly is the dry-run body: gate decisions straight from the
// index.
func evaluateOnly(ctx context.Context, g *gate, candidates []store.FileRow, report *Report) error {
	for _, row := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		failures, err := g.check(ctx, row)
		if err != nil {
			return err
		}
		if len(failures) > 0 {
			report.record(FileResult{Path: row.Path, Outcome: OutcomeNotDurable, Reasons: failures})
			continue
		}
		report.record(FileResult{Path: row.Path, Outcome: OutcomeOffloaded})
	}
	return nil
}

func beginRun(ctx context.Context, s *store.Store, volumeID int64, volumeName string) (int64, error) {
	runID, blocker, err := s.BeginOffloadRunIfClear(ctx, volumeID)
	if err != nil {
		return 0, fmt.Errorf("begin offload run: %w", err)
	}
	if blocker != nil {
		return 0, fmt.Errorf("offload of %s refused: %s run %d is already running (started %s)",
			volumeName, blocker.Kind, blocker.ID,
			time.Unix(0, blocker.StartedAtNs).UTC().Format(time.RFC3339))
	}
	return runID, nil
}

// finishRun finalises the kind='offload' runs row: 'failed' carrying
// the fatal error, 'partial' when any per-file operation errored, and
// 'success' otherwise (skipped files are decisions, so a run that only
// skipped is still a success). file_count records how many files were
// actually offloaded. A failed terminal write is surfaced on the report
// — the per-file flips already committed individually and stand.
func finishRun(ctx context.Context, s *store.Store, runID int64, report *Report, fatalErr error) {
	status, errMsg := store.RunStatusSuccess, ""
	switch {
	case fatalErr != nil:
		status, errMsg = store.RunStatusFailed, fatalErr.Error()
	case report.Errors > 0:
		status = store.RunStatusPartial
	}
	if err := s.FinishRun(ctx, runID, status, errMsg, int64(report.Offloaded)); err != nil {
		report.FinishErr = fmt.Errorf("finish offload run %d: %w", runID, err)
	}
}

// offloadFiles is the real-run body. Each candidate independently
// passes the gate, survives the pre-unlink verification, is unlinked,
// and only then has its row flipped present → offloaded with
// last_seen_run_id = the offload run. The unlink-then-flip order means
// an 'offloaded' row is always the record of a deletion that actually
// happened; a crash in the window between the two leaves a 'present'
// row whose file is gone, which the next index run surfaces as
// 'missing' — a loud, truthful drift signal for an unrecorded but
// durability-gated deletion.
func offloadFiles(ctx context.Context, s *store.Store, g *gate, root string, volumeID, runID int64, candidates []store.FileRow, report *Report) error {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("open volume root: %w", err)
	}
	defer func() { _ = dir.Close() }()

	buf := make([]byte, hashReadBufferSize)
	for _, row := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		failures, err := g.check(ctx, row)
		if err != nil {
			return fmt.Errorf("gate %s: %w", row.Path, err)
		}
		if len(failures) > 0 {
			report.record(FileResult{Path: row.Path, Outcome: OutcomeNotDurable, Reasons: failures})
			continue
		}
		drift, opErr := verifyAndRemove(dir, row, buf)
		switch {
		case opErr != nil:
			report.record(FileResult{Path: row.Path, Outcome: OutcomeError, Reasons: []string{opErr.Error()}})
			continue
		case drift != "":
			report.record(FileResult{Path: row.Path, Outcome: OutcomeDrift, Reasons: []string{drift}})
			continue
		}
		if err := s.MarkOffloaded(ctx, volumeID, row.Path, row.ContentID, runID); err != nil {
			report.record(FileResult{Path: row.Path, Outcome: OutcomeError, Reasons: []string{
				fmt.Sprintf("bytes removed but the status flip failed — the next index run will report the path as missing: %v", err),
			}})
			continue
		}
		report.record(FileResult{Path: row.Path, Outcome: OutcomeOffloaded})
	}
	return nil
}

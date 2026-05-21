// Package sync implements `squirrel sync` and `squirrel restore` on top of
// the rclone binary. rclone is treated as an opaque child process: squirrel
// generates its config, invokes it with a fixed set of flags, and parses
// the structured (--use-json-log) output. Callers never see rclone's INI
// config or run `rclone config` themselves.
package sync

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/runevents"
)

// MinRcloneVersion is the lowest rclone version this binary supports.
// 1.66 introduced BLAKE3 as a built-in hash type; below that, the
// --hash blake3 flag used by sync would be rejected by rclone.
var MinRcloneVersion = Version{Major: 1, Minor: 66}

// Rclone is a configured rclone wrapper. Binary is the resolved executable
// path; Config is the path to the squirrel-managed rclone.conf written by
// WriteRcloneConfig. All Run invocations pass --config Config so the user's
// real rclone configuration is never touched.
type Rclone struct {
	Binary string
	Config string
}

// Find locates the rclone binary on PATH. The returned Rclone has Config
// empty — callers fill it in via WriteRcloneConfig before invoking Run.
func Find() (*Rclone, error) {
	bin, err := exec.LookPath("rclone")
	if err != nil {
		return nil, fmt.Errorf("rclone not found on PATH (install rclone ≥ %s): %w", MinRcloneVersion, err)
	}
	return &Rclone{Binary: bin}, nil
}

// Version is a parsed semver from `rclone version`. Only major/minor/patch
// are kept; pre-release / build suffixes are intentionally dropped because
// they do not participate in the version-floor check.
type Version struct {
	Major, Minor, Patch int
	Raw                 string
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// AtLeast reports whether v is at or above min, comparing (major, minor,
// patch) lexicographically.
func (v Version) AtLeast(min Version) bool {
	switch {
	case v.Major != min.Major:
		return v.Major > min.Major
	case v.Minor != min.Minor:
		return v.Minor > min.Minor
	default:
		return v.Patch >= min.Patch
	}
}

// rcloneVersionRE matches the first line of `rclone version`, e.g.
// "rclone v1.74.1". The leading "v" is optional in case of unusual builds.
var rcloneVersionRE = regexp.MustCompile(`^rclone\s+v?(\d+)\.(\d+)(?:\.(\d+))?`)

// Version executes `rclone version` and parses the first line. Stderr is
// captured into the error for diagnostic value if invocation itself fails.
func (r *Rclone) Version(ctx context.Context) (Version, error) {
	cmd := exec.CommandContext(ctx, r.Binary, "version")
	out, err := cmd.Output()
	if err != nil {
		return Version{}, fmt.Errorf("rclone version: %w", err)
	}
	line, _, _ := lineSplit(out)
	m := rcloneVersionRE.FindStringSubmatch(line)
	if m == nil {
		return Version{}, fmt.Errorf("could not parse rclone version output: %q", line)
	}
	v := Version{Raw: line}
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		v.Patch, _ = strconv.Atoi(m[3])
	}
	return v, nil
}

// lineSplit returns the first line of data and whatever follows, splitting
// on the first newline. Used to keep version parsing in-package without
// importing strings just for one call.
func lineSplit(data []byte) (line string, rest []byte, ok bool) {
	for i, b := range data {
		if b == '\n' {
			return string(data[:i]), data[i+1:], true
		}
	}
	return string(data), nil, false
}

// WriteRcloneConfig renders the destinations into an rclone INI config at
// path with mode 0600 and returns it set on r.Config. The file is fully
// rewritten on every call so that squirrel's config remains the single
// source of truth for destinations.
func (r *Rclone) WriteRcloneConfig(path string, dests map[string]*config.Destination) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create rclone config dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open rclone config for write: %w", err)
	}
	defer f.Close()
	// OpenFile's perm only applies on create; if the file already exists
	// (e.g., created by a previous version that used 0644) the existing
	// mode is preserved. Force 0600 unconditionally — this file contains
	// resolved secrets.
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod rclone config: %w", err)
	}

	// Stable section order so repeat writes produce identical bytes.
	names := make([]string, 0, len(dests))
	for name := range dests {
		names = append(names, name)
	}
	sort.Strings(names)
	first := true
	for _, name := range names {
		section := dests[name].RcloneSection()
		if section == "" {
			continue // type=local — no rclone remote needed
		}
		if !first {
			if _, err := io.WriteString(f, "\n"); err != nil {
				return err
			}
		}
		first = false
		if _, err := io.WriteString(f, section); err != nil {
			return fmt.Errorf("write section %s: %w", name, err)
		}
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync rclone config: %w", err)
	}
	r.Config = path
	return nil
}

// RunResult summarises the outcome of one rclone invocation as parsed from
// the --use-json-log output stream. Transferred and Checked come from the
// final stats line; Errors counts per-file error events. FailedFiles lists
// the individual error messages (capped to avoid unbounded memory for a
// pathological run).
type RunResult struct {
	Transferred int64
	Checked     int64
	Errors      int64
	Bytes       int64
	FailedFiles []FailedFile
	// FatalError is true when the run failed in a way that did not produce
	// per-file errors — e.g. source root missing, auth failure.
	FatalError bool
}

// FailedFile is one per-object error from the JSON log. Object may be
// empty for errors that do not relate to a specific file (auth, listing).
type FailedFile struct {
	Object  string
	Message string
}

// maxFailedFiles caps the in-memory list of failed-file diagnostics so a
// run that explodes on millions of files cannot exhaust memory. The total
// error count in RunResult.Errors is exact regardless of this cap.
const maxFailedFiles = 100

// Run executes `rclone <args...> --config <r.Config> --use-json-log` with
// the given extra arguments. It streams both stdout (rclone writes to
// stderr by default but --use-json-log routes structured output to stderr
// too) and stderr line-by-line, parsing each JSON event into RunResult.
// The returned error is non-nil iff invocation itself failed or rclone
// exited non-zero; per-file errors land in RunResult.FailedFiles without
// failing the call so the caller can decide how to surface them.
func (r *Rclone) Run(ctx context.Context, args ...string) (RunResult, error) {
	return r.RunWithProgress(ctx, nil, args...)
}

// RunWithProgress is the variant that emits in-flight Progress events
// derived from rclone's periodic stats lines. The callback is invoked
// synchronously from the stderr-reader goroutine; it must not block.
// onProgress may be nil, in which case behaviour is identical to Run.
//
// --stats 1s is appended so rclone emits a stats event every second
// while the copy is in flight (its default cadence is 1 minute, which
// is uselessly coarse for the desktop's live-progress UI). The final
// stats line at end-of-run is unaffected.
func (r *Rclone) RunWithProgress(ctx context.Context, onProgress func(runevents.Progress), args ...string) (RunResult, error) {
	if r.Config == "" {
		return RunResult{}, errors.New("rclone wrapper: Config not set (call WriteRcloneConfig first)")
	}
	full := append([]string{
		"--config", r.Config,
		"--use-json-log",
		"--log-level", "INFO",
		"--stats", "1s",
	}, args...)
	cmd := exec.CommandContext(ctx, r.Binary, full...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return RunResult{}, fmt.Errorf("rclone stderr pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return RunResult{}, fmt.Errorf("rclone stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return RunResult{}, fmt.Errorf("rclone start: %w", err)
	}

	var (
		result RunResult
		wg     sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		parseJSONLog(stderr, &result, onProgress)
	}()
	go func() {
		defer wg.Done()
		// rclone does not usually write to stdout in copy mode, but drain
		// it anyway to avoid a deadlock if it ever does.
		_, _ = io.Copy(io.Discard, stdout)
	}()
	wg.Wait()

	waitErr := cmd.Wait()
	if waitErr != nil {
		result.FatalError = result.Errors == 0
		return result, fmt.Errorf("rclone exit: %w", waitErr)
	}
	return result, nil
}

// rcloneEvent captures the subset of rclone's JSON log we care about: the
// level (for error filtering), the per-object message and object name (for
// failed-file lists), and the stats object that rclone emits at the end of
// a run. Everything else is dropped silently.
type rcloneEvent struct {
	Level  string       `json:"level"`
	Msg    string       `json:"msg"`
	Object string       `json:"object"`
	Stats  *rcloneStats `json:"stats,omitempty"`
}

// rcloneStats matches the keys of the stats object rclone emits at the end
// of a copy run. Field names follow rclone's casing exactly.
type rcloneStats struct {
	Bytes          int64 `json:"bytes"`
	TotalTransfers int64 `json:"totalTransfers"`
	TotalChecks    int64 `json:"totalChecks"`
	Errors         int64 `json:"errors"`
	FatalError     bool  `json:"fatalError"`
}

// retrySummaryRE matches rclone's "Attempt N/M failed with K errors …"
// lines. rclone emits one of these per retry attempt; the underlying
// per-file error is already captured separately, so logging the
// per-attempt summary as well would produce N duplicates per failure.
var retrySummaryRE = regexp.MustCompile(`^Attempt \d+/\d+ failed`)

func isRetrySummary(msg string) bool { return retrySummaryRE.MatchString(msg) }

// parseJSONLog reads JSON-per-line events from r and updates result in
// place. Non-JSON lines (e.g. an early startup notice on an older rclone)
// are skipped — we cannot make decisions on them and surfacing them as
// errors would create false positives. onProgress, if non-nil, is
// invoked once per stats event so callers can drive a live UI.
func parseJSONLog(r io.Reader, result *RunResult, onProgress func(runevents.Progress)) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev rcloneEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Stats != nil {
			result.Transferred = ev.Stats.TotalTransfers
			result.Checked = ev.Stats.TotalChecks
			result.Bytes = ev.Stats.Bytes
			// Don't overwrite Errors with a smaller value from a
			// per-attempt stats line; take the maximum so retries that
			// fail then succeed still surface the failure count.
			if ev.Stats.Errors > result.Errors {
				result.Errors = ev.Stats.Errors
			}
			if ev.Stats.FatalError {
				result.FatalError = true
			}
			if onProgress != nil {
				onProgress(runevents.Progress{
					Stage:      runevents.StageUploading,
					Done:       result.Transferred,
					Total:      ev.Stats.TotalTransfers + ev.Stats.TotalChecks,
					BytesDone:  result.Bytes,
					BytesTotal: 0,
				})
			}
			continue
		}
		if ev.Level == "error" && !isRetrySummary(ev.Msg) {
			// Capture object-less errors too: auth failures, listing
			// errors, "Failed to copy: …" diagnostics carry no Object
			// but are exactly the messages we want in runs.error. Filter
			// out per-attempt retry summaries to avoid 3x noise on a
			// single underlying failure.
			if int64(len(result.FailedFiles)) < maxFailedFiles {
				result.FailedFiles = append(result.FailedFiles, FailedFile{
					Object: ev.Object, Message: ev.Msg,
				})
			}
		}
	}
}

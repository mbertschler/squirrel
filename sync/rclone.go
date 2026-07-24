// Package sync implements `squirrel sync` and `squirrel restore` on top of
// the rclone binary. rclone is treated as an opaque child process: squirrel
// generates its config, invokes it with a fixed set of flags, and parses
// the structured (--use-json-log) output. Callers never see rclone's INI
// config or run `rclone config` themselves.
package sync

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/runevents"
	"github.com/mbertschler/squirrel/volmark"
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
// path with mode 0600 and sets it on r.Config. squirrel's config is the
// single source of truth for destinations, but the file is rewritten only
// when its content actually changes: the rendered bytes are compared
// against the file already on disk, and an identical render is left
// untouched (no truncate, no mtime bump). This both avoids needless churn
// and turns an unexpected rewrite into a signal — a caller can log the one
// line so a buggy resolver silently regressing credentials is visible.
//
// wrote reports whether the file was (re)written: false means the existing
// content already matched. A genuine rewrite is atomic — the bytes land in
// a sibling temp file that is fsync'd and renamed over path — so a crash
// mid-write can never leave a partially-rendered config with live secrets.
func (r *Rclone) WriteRcloneConfig(path string, dests map[string]*config.Destination) (wrote bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("create rclone config dir: %w", err)
	}
	rendered := renderRcloneConfig(dests)
	if rcloneConfigUnchanged(path, rendered) {
		// Defend the 0600 invariant even on the skip path: a pre-existing
		// file from an older squirrel (which used 0644) must still be
		// tightened, since it holds resolved secrets.
		if err := os.Chmod(path, 0o600); err != nil {
			return false, fmt.Errorf("chmod rclone config: %w", err)
		}
		r.Config = path
		return false, nil
	}
	if err := writeRcloneConfigAtomic(path, rendered); err != nil {
		return false, err
	}
	r.Config = path
	return true, nil
}

// renderRcloneConfig produces the INI body for the given destinations in a
// stable section order so identical inputs render byte-for-byte identically
// (the content-comparison in WriteRcloneConfig relies on this). type=local
// destinations contribute no section — rclone addresses local paths
// directly.
func renderRcloneConfig(dests map[string]*config.Destination) []byte {
	names := make([]string, 0, len(dests))
	for name := range dests {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	first := true
	for _, name := range names {
		section := dests[name].RcloneSection()
		if section == "" {
			continue
		}
		if !first {
			b.WriteString("\n")
		}
		first = false
		b.WriteString(section)
	}
	return []byte(b.String())
}

// rcloneConfigUnchanged reports whether the file at path already holds
// exactly want. A read error (missing file, unreadable) is treated as
// "changed" so the caller falls through to a write.
func rcloneConfigUnchanged(path string, want []byte) bool {
	existing, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Equal(existing, want)
}

// writeRcloneConfigAtomic writes data to a temp file in path's directory,
// fsyncs it, and renames it over path so a reader never observes a
// half-written config. The temp file is created at 0600 (the config holds
// resolved secrets) and removed on any error before the rename succeeds.
func writeRcloneConfigAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".rclone.conf-*")
	if err != nil {
		return fmt.Errorf("create temp rclone config: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op once renamed
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("chmod temp rclone config: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write temp rclone config: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync temp rclone config: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp rclone config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename rclone config into place: %w", err)
	}
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
	// HashFallback is true when rclone reported that --checksum could not
	// use the requested hash because source and destination share none,
	// and silently fell back to a size-based comparison. A run that asked
	// for BLAKE3 verification but hit this path was not content-verified,
	// however rclone exited, so the caller must not record it as verified.
	HashFallback bool
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

// runPlain executes rclone with the wrapper's --config but without the
// JSON-log and stats flags Run uses, returning captured stdout. It backs
// the auxiliary commands the snapshot ride-along needs (copyto, lsf,
// deletefile), where we want a simple exit-code success/failure and, for
// lsf, a short listing — not the streamed copy stats parseJSONLog builds.
// Stderr is folded into the error on failure for diagnostics.
func (r *Rclone) runPlain(ctx context.Context, args ...string) ([]byte, error) {
	if r.Config == "" {
		return nil, errors.New("rclone wrapper: Config not set (call WriteRcloneConfig first)")
	}
	full := append([]string{"--config", r.Config}, args...)
	cmd := exec.CommandContext(ctx, r.Binary, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.Bytes(), fmt.Errorf("rclone %s: %w: %s", args[0], err, msg)
		}
		return stdout.Bytes(), fmt.Errorf("rclone %s: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}

// copyTo copies a single source file to a single destination path via
// `rclone copyto`, creating intermediate directories as needed. Used by
// the snapshot ride-along and the content-addressed push to land one
// file at a fixed destination name (copy, by contrast, would treat the
// destination as a directory). extraArgs carries per-destination flags
// such as the --checkers cap.
func (r *Rclone) copyTo(ctx context.Context, src, dst string, extraArgs ...string) error {
	args := append([]string{"copyto"}, extraArgs...)
	_, err := r.runPlain(ctx, append(args, src, dst)...)
	return err
}

// catRemote reads the full contents of the single object at uri via
// `rclone cat`. It is called only after statRemoteExists has confirmed
// the object is present, so any error here — including a race where the
// object vanished between the stat and the read — is surfaced verbatim
// and the caller refuses. Absence is deliberately decided by the stat,
// not by cat's exit: bucket backends (s3/b2/gcs) return an empty body on
// a zero exit for a missing key, which cat cannot tell apart from a
// genuinely empty object. extraArgs carries per-destination flags such
// as the --checkers cap.
func (r *Rclone) catRemote(ctx context.Context, uri string, extraArgs ...string) ([]byte, error) {
	args := append([]string{"cat"}, extraArgs...)
	return r.runPlain(ctx, append(args, uri)...)
}

// isNotFoundErr reports whether an rclone error denotes an absent path
// rather than a transient, network, or permission failure. It matches
// only rclone's canonical absence messages — fs.ErrorObjectNotFound
// ("object not found") and fs.ErrorDirNotFound ("directory not found"),
// surfaced across every backend the marker gate targets. Requiring the
// canonical phrasing rather than a bare "not found" substring keeps a
// DNS "host not found" or a connection error from being misread as
// absence: those propagate as hard errors and refuse the sync, per the
// refuse-over-wrong-write invariant.
func isNotFoundErr(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "object not found") ||
		strings.Contains(msg, "directory not found")
}

// statRemoteExists reports whether a single object exists at uri via
// `rclone lsjson --stat`, classifying the outcome three ways so the
// marker gate can tell a genuinely-absent marker from an unreachable
// one:
//
//   - (true, nil): the stat returned an object.
//   - (false, nil): the backend definitively reports absence — an empty
//     or "null" stat body on a zero exit (how the bucket backends
//     s3/b2/gcs report a missing key) or a canonical "not found" exit
//     (how sftp reports it).
//   - (false, err): any other failure (transient, auth, DNS). The caller
//     must refuse rather than mistake unreachability for absence.
//
// Unlike statRemote, which returns the object size and folds absence into
// an error, this preserves the absent/unreachable distinction the
// refuse-over-wrong-write invariant depends on. extraArgs carries
// per-destination flags such as the --checkers cap.
func (r *Rclone) statRemoteExists(ctx context.Context, uri string, extraArgs ...string) (bool, error) {
	args := append([]string{"lsjson", "--stat"}, extraArgs...)
	out, err := r.runPlain(ctx, append(args, uri)...)
	if err != nil {
		if isNotFoundErr(err) {
			return false, nil
		}
		return false, err
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return false, nil
	}
	return true, nil
}

// listSnapshots returns the snapshot filenames directly under dirURI via
// `rclone lsf`. A missing directory yields an empty list, not an error:
// the first ride-along to a volume legitimately finds nothing there yet,
// and rclone reports the absent directory on stderr with a non-zero exit.
// Only index-* .db entries are returned so an unrelated file in the tree
// is never a rotation candidate.
func (r *Rclone) listSnapshots(ctx context.Context, dirURI string) ([]string, error) {
	out, err := r.runPlain(ctx, "lsf", "--files-only", dirURI)
	if err != nil {
		// lsf against a not-yet-created directory exits non-zero; treat an
		// empty listing as "nothing to rotate" rather than a hard error.
		if len(bytes.TrimSpace(out)) == 0 {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if strings.HasPrefix(name, snapshotPrefix) && strings.HasSuffix(name, ".db") {
			names = append(names, name)
		}
	}
	return names, nil
}

// deleteFile removes a single file at fileURI via `rclone deletefile`.
func (r *Rclone) deleteFile(ctx context.Context, fileURI string) error {
	_, err := r.runPlain(ctx, "deletefile", fileURI)
	return err
}

// remoteRootEmpty reports whether rootURI holds no files, listing it
// recursively via `rclone lsf -R --files-only`. Only rclone's canonical
// "directory not found" (a root that was never created) — accepting the
// "object not found" phrasing too — counts as an empty/fresh root. Every
// other error (auth, network, permission) surfaces so the layout guards
// stay fail-closed and refuse rather than mistaking a broken probe for a
// wiped destination: runPlain carries stderr only in the returned error,
// not in stdout, so an empty stdout alone must never be read as "empty".
// The guards use the empty result to recognise a wiped or repointed
// destination — including one whose recorded state was cleared by
// `squirrel destination reset` — as a fresh start rather than a layout
// conflict.
//
// Per-volume markers (volmark.MarkerName) do not count as content. A root
// that was wiped and re-bootstrapped carries a marker again — the marker gate
// writes one on `--init` before any layout guard runs — so counting markers
// would make the fresh-start recognition unreachable in exactly the situation
// it exists for. Anything else present, including a single stray file, still
// reads as non-empty and keeps the caller's refusal.
func (r *Rclone) remoteRootEmpty(ctx context.Context, rootURI string, extraArgs ...string) (bool, error) {
	args := append([]string{"lsf", "-R", "--files-only"}, extraArgs...)
	out, err := r.runPlain(ctx, append(args, rootURI)...)
	if err != nil {
		if isRemoteNotFound(err) {
			return true, nil
		}
		return false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" && path.Base(name) != volmark.MarkerName {
			return false, nil
		}
	}
	return true, nil
}

// isRemoteNotFound reports whether err is rclone's canonical
// "directory not found" (or "object not found") — the only listing failure
// the layout guard reads as an empty, fresh root. Any other error must
// surface so the guard fails closed. #150's isNotFoundErr is not on this
// branch, so the canonical-phrase check lives here; runPlain formats
// rclone's stderr into the error string, so matching err.Error() catches it.
func isRemoteNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "directory not found") || strings.Contains(msg, "object not found")
}

// statRemote returns the size of the single object at uri via
// `rclone lsjson --stat`. The content-addressed push uses it to confirm
// presence and size of each uploaded object and manifest segment;
// through a crypt overlay the reported size is the decrypted length, so
// it compares directly against local byte counts. A missing object
// surfaces as an error (rclone exits non-zero; defensively, a `null`
// stat on a tolerant rclone build is mapped to the same outcome).
func (r *Rclone) statRemote(ctx context.Context, uri string, extraArgs ...string) (int64, error) {
	args := append([]string{"lsjson", "--stat"}, extraArgs...)
	out, err := r.runPlain(ctx, append(args, uri)...)
	if err != nil {
		return 0, err
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return 0, fmt.Errorf("rclone lsjson: no object at %s", uri)
	}
	var entry struct {
		Size  int64 `json:"Size"`
		IsDir bool  `json:"IsDir"`
	}
	if err := json.Unmarshal(trimmed, &entry); err != nil {
		return 0, fmt.Errorf("parse lsjson --stat output for %s: %w", uri, err)
	}
	if entry.IsDir {
		return 0, fmt.Errorf("%s is a directory, expected a single object", uri)
	}
	return entry.Size, nil
}

// lsjsonEntry is one object from an `rclone lsjson` listing: its name,
// size, and — when hashes were requested — the provider checksums keyed
// by rclone hash name.
type lsjsonEntry struct {
	Name   string            `json:"Name"`
	Size   int64             `json:"Size"`
	IsDir  bool              `json:"IsDir"`
	Hashes map[string]string `json:"Hashes"`
}

// listHashes runs `rclone lsjson --hash --files-only` over dirURI and
// returns the entries with their provider checksums. hashTypes narrows
// which hashes rclone computes (--hash-type, repeated); nil requests
// every hash the backend exposes. extraArgs carries per-destination
// flags such as the --checkers cap and --include filters scoping the
// listing.
func (r *Rclone) listHashes(ctx context.Context, dirURI string, hashTypes []string, extraArgs ...string) ([]lsjsonEntry, error) {
	args := []string{"lsjson", "--hash", "--files-only"}
	for _, ht := range hashTypes {
		args = append(args, "--hash-type", ht)
	}
	args = append(args, extraArgs...)
	out, err := r.runPlain(ctx, append(args, dirURI)...)
	if err != nil {
		return nil, err
	}
	var entries []lsjsonEntry
	if err := json.Unmarshal(bytes.TrimSpace(out), &entries); err != nil {
		return nil, fmt.Errorf("parse lsjson output for %s: %w", dirURI, err)
	}
	return entries, nil
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
	TotalBytes     int64 `json:"totalBytes"`
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

// hashFallbackRE matches rclone's notice that --checksum has no common
// hash to compare with and is degrading to a size-based check, e.g.
// "--checksum is in use but the source and destination have no hashes in
// common; falling back to --size-only". The trailing verb has varied
// across rclone versions ("falling back"/"failing back") so the match
// keys on the stable phrase "no hashes in common", at any log level.
var hashFallbackRE = regexp.MustCompile(`no hashes in common`)

func isHashFallback(msg string) bool { return hashFallbackRE.MatchString(msg) }

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
		if isHashFallback(ev.Msg) {
			// Emitted at NOTICE level (which the level filter below drops),
			// so it is detected here before that filter: a run that asked
			// for BLAKE3 but lost the hash must not be recorded as verified.
			result.HashFallback = true
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
					BytesTotal: ev.Stats.TotalBytes,
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

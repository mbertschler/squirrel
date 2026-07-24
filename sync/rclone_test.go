package sync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/runevents"
)

// TestStallGuardFiresWithoutProgress proves the no-progress guard cancels
// the run — and records that it fired — when no advance arrives within the
// timeout. This is the F25 wedge: rclone alive but transferring nothing.
// The test waits for the guard rather than racing a deadline, so the exact
// timeout is not timing-sensitive; a generous value keeps it robust under
// load.
func TestStallGuardFiresWithoutProgress(t *testing.T) {
	var cancelled atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newStallGuard(ctx, 100*time.Millisecond, func() {
		cancelled.Store(true)
		cancel()
	})
	g.wait() // blocks until watch returns; with no poke it fires first
	if !g.fired.Load() {
		t.Fatal("guard did not fire after the stall timeout elapsed with no progress")
	}
	if !cancelled.Load() {
		t.Fatal("guard fired but never cancelled the run context")
	}
}

// TestStallGuardResetsOnProgress proves a transfer that keeps advancing is
// never killed: pokes driven off a ticker at a fraction of the stall window
// keep resetting it across a span twice as long as one window, so only a
// working reset explains the guard staying quiet. The wide ratio between
// the stall timeout and the poke interval keeps scheduler jitter from
// faking a stall.
func TestStallGuardResetsOnProgress(t *testing.T) {
	var cancelled atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const stall = 400 * time.Millisecond
	g := newStallGuard(ctx, stall, func() {
		cancelled.Store(true)
		cancel()
	})
	tick := time.NewTicker(stall / 20) // pokes at 20x the resolution of the bound
	defer tick.Stop()
	deadline := time.After(2 * stall) // outlast a single window, so a reset is required
	for done := false; !done; {
		select {
		case <-tick.C:
			g.advance()
		case <-deadline:
			done = true
		}
	}
	if g.fired.Load() {
		t.Fatal("guard fired while progress was still arriving")
	}
	cancel() // finishing the run stops the guard cleanly
	g.wait()
	if cancelled.Load() {
		t.Fatal("guard cancelled the run despite steady progress")
	}
}

// requireRclone skips the test if rclone is not on PATH. The wrapper tests
// exercise the real binary against a local-filesystem destination; if a
// developer wants to run them they need rclone installed.
func requireRclone(t *testing.T) *Rclone {
	t.Helper()
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("rclone not on PATH; install rclone ≥ 1.66 to run these tests")
	}
	r, err := Find()
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	return r
}

func TestVersionParses(t *testing.T) {
	r := requireRclone(t)
	v, err := r.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if !v.AtLeast(MinRcloneVersion) {
		t.Logf("rclone version %s is below the documented floor %s; some tests will fail until you upgrade", v, MinRcloneVersion)
	}
	if v.Raw == "" || !strings.Contains(v.Raw, "rclone") {
		t.Fatalf("Raw = %q, expected first line of `rclone version`", v.Raw)
	}
}

// TestParseJSONLogCapturesObjectlessErrors confirms that level=error
// events without an Object field — auth failures, listing errors, the
// terminal "Failed to copy: ..." line — end up in FailedFiles so the
// runs row's error column can be populated from them. Retry-summary
// lines ("Attempt N/M failed ...") are filtered to avoid logging the
// same underlying failure three times.
func TestParseJSONLogCapturesObjectlessErrors(t *testing.T) {
	stream := strings.Join([]string{
		`{"level":"error","msg":"Failed to authenticate: 401 Unauthorized","source":"x"}`,
		`{"level":"error","msg":"error reading source root: nope","object":"FS at /x","source":"x"}`,
		`{"level":"error","msg":"Attempt 1/3 failed with 1 errors and: nope","source":"x"}`,
		`{"level":"error","msg":"Attempt 2/3 failed with 1 errors and: nope","source":"x"}`,
		`{"stats":{"errors":1,"fatalError":true,"totalTransfers":0,"totalChecks":0,"bytes":0}}`,
	}, "\n")
	var r RunResult
	parseJSONLog(strings.NewReader(stream), &r, nil, nil)

	if len(r.FailedFiles) != 2 {
		t.Fatalf("FailedFiles = %+v, want 2 (auth + reading; retry summaries dropped)", r.FailedFiles)
	}
	if r.FailedFiles[0].Object != "" || !strings.Contains(r.FailedFiles[0].Message, "authenticate") {
		t.Fatalf("first FailedFile = %+v, want object-less auth message", r.FailedFiles[0])
	}
	if r.FailedFiles[1].Object == "" {
		t.Fatalf("second FailedFile lost its object: %+v", r.FailedFiles[1])
	}
	if !r.FatalError {
		t.Fatalf("FatalError = false, want true")
	}
}

// TestParseJSONLogCapturesFatalLevelAndRawStderr: a fatal-level JSON event
// (previously dropped by the error-only filter) and a non-JSON pre-logger
// diagnostic are both captured, so a failing destination is diagnosable
// (#157, F6/F15) instead of leaving a blank ERROR column.
func TestParseJSONLogCapturesFatalLevelAndRawStderr(t *testing.T) {
	stream := strings.Join([]string{
		`Failed to create file system for "cloudbox:/x": didn't find backend`,
		`{"level":"fatal","msg":"NoCredentialProviders: no valid providers in chain"}`,
		`{"stats":{"errors":0,"fatalError":true,"totalTransfers":0,"totalChecks":0,"bytes":0}}`,
	}, "\n")
	var r RunResult
	parseJSONLog(strings.NewReader(stream), &r, nil, nil)

	if len(r.FailedFiles) != 1 || !strings.Contains(r.FailedFiles[0].Message, "NoCredentialProviders") {
		t.Fatalf("FailedFiles = %+v, want the fatal-level message captured", r.FailedFiles)
	}
	if !strings.Contains(r.Stderr, "didn't find backend") {
		t.Fatalf("Stderr = %q, want the non-JSON pre-logger diagnostic", r.Stderr)
	}
}

// TestParseJSONLogStderrBounded: a pathological non-JSON stream can't
// balloon RunResult.Stderr past the cap, and the capture keeps the tail
// (rclone's fatal line prints last) — the final line survives while an
// early one is trimmed from the front.
func TestParseJSONLogStderrBounded(t *testing.T) {
	lines := make([]string, 0, 2000)
	for i := 0; i < 2000; i++ {
		lines = append(lines, fmt.Sprintf("stderr line %04d", i))
	}
	var r RunResult
	parseJSONLog(strings.NewReader(strings.Join(lines, "\n")), &r, nil, nil)
	if len(r.Stderr) == 0 {
		t.Fatal("Stderr empty, want the tail of the stream")
	}
	if len(r.Stderr) > maxStderrCapture {
		t.Fatalf("Stderr len = %d, want <= %d", len(r.Stderr), maxStderrCapture)
	}
	if !strings.Contains(r.Stderr, "stderr line 1999") {
		t.Fatal("Stderr lost the final line — tail not kept")
	}
	if strings.Contains(r.Stderr, "stderr line 0000") {
		t.Fatal("Stderr kept the first line — want the tail only")
	}
}

// TestDisplayErrors pins the summary error count: rclone's per-file count
// wins, but a fatal invocation failure with no per-file count still
// reports at least one error so a status=failed run never claims errors=0.
func TestDisplayErrors(t *testing.T) {
	cases := []struct {
		name string
		r    RunResult
		want int64
	}{
		{"per-file errors", RunResult{Errors: 3}, 3},
		{"fatal, no count, no captured files", RunResult{FatalError: true}, 1},
		{"fatal, captured files", RunResult{FatalError: true, FailedFiles: []FailedFile{{Message: "a"}, {Message: "b"}}}, 2},
		{"fatal but per-file count wins", RunResult{FatalError: true, Errors: 5}, 5},
		{"clean", RunResult{}, 0},
	}
	for _, c := range cases {
		if got := c.r.DisplayErrors(); got != c.want {
			t.Errorf("%s: DisplayErrors() = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestParseJSONLogDetectsHashFallback: rclone's no-common-hash notice is
// emitted at NOTICE level, which the error filter drops; parseJSONLog
// still flags it so a flags-set, exit-0 run that silently degraded to a
// size comparison is not later recorded as content-verified.
func TestParseJSONLogDetectsHashFallback(t *testing.T) {
	stream := strings.Join([]string{
		`{"level":"notice","msg":"--checksum is in use but the source and destination have no hashes in common; falling back to --size-only","source":"x"}`,
		`{"stats":{"errors":0,"fatalError":false,"totalTransfers":2,"totalChecks":0,"bytes":10}}`,
	}, "\n")
	var r RunResult
	parseJSONLog(strings.NewReader(stream), &r, nil, nil)

	if !r.HashFallback {
		t.Fatalf("HashFallback = false, want true (no-common-hash notice should be detected)")
	}
	if len(r.FailedFiles) != 0 {
		t.Fatalf("FailedFiles = %+v, want none (the notice is not a per-file error)", r.FailedFiles)
	}
}

// TestParseJSONLogNoFalseHashFallback: an ordinary run never trips the
// fallback flag.
func TestParseJSONLogNoFalseHashFallback(t *testing.T) {
	stream := `{"stats":{"errors":0,"fatalError":false,"totalTransfers":2,"totalChecks":1,"bytes":10}}`
	var r RunResult
	parseJSONLog(strings.NewReader(stream), &r, nil, nil)
	if r.HashFallback {
		t.Fatalf("HashFallback = true on a clean run, want false")
	}
}

// TestParseJSONLogEmitsProgressWithByteTotal verifies that per-second stats
// lines drive the onProgress callback and that the announced byte total is
// surfaced (it backs the CLI's ETA). A nil callback path is exercised
// elsewhere; here we assert the event shape.
func TestParseJSONLogEmitsProgressWithByteTotal(t *testing.T) {
	stream := strings.Join([]string{
		`{"stats":{"errors":0,"fatalError":false,"totalTransfers":10,"totalChecks":0,"bytes":512,"totalBytes":2048}}`,
		`{"stats":{"errors":0,"fatalError":false,"totalTransfers":10,"totalChecks":0,"bytes":2048,"totalBytes":2048}}`,
	}, "\n")
	var r RunResult
	var events []runevents.Progress
	parseJSONLog(strings.NewReader(stream), &r, func(p runevents.Progress) {
		events = append(events, p)
	}, nil)
	if len(events) != 2 {
		t.Fatalf("progress events = %d, want 2", len(events))
	}
	first := events[0]
	if first.Stage != runevents.StageUploading {
		t.Errorf("stage = %q, want %q", first.Stage, runevents.StageUploading)
	}
	if first.BytesDone != 512 || first.BytesTotal != 2048 {
		t.Errorf("first event bytes = %d/%d, want 512/2048", first.BytesDone, first.BytesTotal)
	}
	if first.Total != 10 {
		t.Errorf("first event Total (transfers+checks) = %d, want 10", first.Total)
	}
}

func TestIsRetrySummary(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"Attempt 1/3 failed with 1 errors and: nope", true},
		{"Attempt 10/12 failed with 4 errors", true},
		{"Failed to copy: nope", false},
		{"error reading source root: nope", false},
		{"Attempt to do something but not a summary", false}, // missing N/M
	}
	for _, c := range cases {
		if got := isRetrySummary(c.in); got != c.want {
			t.Errorf("isRetrySummary(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	// Pure-logic check independent of the installed binary.
	v := func(major, minor, patch int) Version {
		return Version{Major: major, Minor: minor, Patch: patch}
	}
	cases := []struct {
		a, b Version
		want bool
	}{
		{v(1, 66, 0), v(1, 66, 0), true},
		{v(1, 66, 1), v(1, 66, 0), true},
		{v(1, 65, 9), v(1, 66, 0), false},
		{v(2, 0, 0), v(1, 99, 99), true},
		{v(0, 99, 99), v(1, 0, 0), false},
	}
	for _, c := range cases {
		if got := c.a.AtLeast(c.b); got != c.want {
			t.Errorf("%v.AtLeast(%v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// writeFakeConfig builds a minimal squirrel config with a single SFTP
// destination so WriteRcloneConfig has something non-empty to render.
// Tests that exercise rclone Run() use a separate "local" destination
// pattern that does not need an rclone config section.
func writeFakeConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestWriteRcloneConfigRendersSFTP(t *testing.T) {
	cfg := writeFakeConfig(t, `
[destinations.nas]
type = "sftp"
host = "nas.local"
user = "martin"
root = "/data"
password = "p"
`)
	r := &Rclone{}
	target := filepath.Join(t.TempDir(), "rclone.conf")
	if _, err := r.WriteRcloneConfig(target, cfg.Destinations); err != nil {
		t.Fatalf("WriteRcloneConfig: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat rclone.conf: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 0600 (secrets are inside this file)", info.Mode().Perm())
	}
	body, _ := os.ReadFile(target)
	for _, want := range []string{"[nas]", "type = sftp", "host = nas.local", "password = p"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("rclone.conf missing %q:\n%s", want, body)
		}
	}
}

// TestWriteRcloneConfigRendersSFTPHostKeyValidation confirms the optional
// sftp host-key params reach the written rclone.conf: known_hosts_file is
// what enables server host-key validation, and host_key_algorithms pins the
// accepted algorithms. Absent these, rclone does no host-key validation.
func TestWriteRcloneConfigRendersSFTPHostKeyValidation(t *testing.T) {
	cfg := writeFakeConfig(t, `
[destinations.nas]
type                = "sftp"
host                = "nas.local"
user                = "martin"
root                = "/data"
password            = "p"
known_hosts_file    = "~/.ssh/known_hosts"
host_key_algorithms = "ssh-ed25519 ssh-rsa"
`)
	r := &Rclone{}
	target := filepath.Join(t.TempDir(), "rclone.conf")
	if _, err := r.WriteRcloneConfig(target, cfg.Destinations); err != nil {
		t.Fatalf("WriteRcloneConfig: %v", err)
	}
	body, _ := os.ReadFile(target)
	for _, want := range []string{
		"known_hosts_file = ~/.ssh/known_hosts",
		"host_key_algorithms = ssh-ed25519 ssh-rsa",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("rclone.conf missing %q:\n%s", want, body)
		}
	}
}

// TestWriteRcloneConfigRendersS3StorageClass confirms the optional s3
// storage_class reaches the written rclone.conf.
func TestWriteRcloneConfigRendersS3StorageClass(t *testing.T) {
	cfg := writeFakeConfig(t, `
[destinations.archive]
type              = "s3"
provider          = "AWS"
bucket            = "squirrel"
root              = "/p"
storage_class     = "DEEP_ARCHIVE"
access_key_id     = "AK"
secret_access_key = "sk"
`)
	r := &Rclone{}
	target := filepath.Join(t.TempDir(), "rclone.conf")
	if _, err := r.WriteRcloneConfig(target, cfg.Destinations); err != nil {
		t.Fatalf("WriteRcloneConfig: %v", err)
	}
	body, _ := os.ReadFile(target)
	if !strings.Contains(string(body), "storage_class = DEEP_ARCHIVE") {
		t.Fatalf("rclone.conf missing storage_class:\n%s", body)
	}
}

// TestWriteRcloneConfigTightensExistingPermissions exercises the chmod
// path. OpenFile's perm argument is only honored on create, so a file
// that already exists with looser perms (e.g., 0644 from a previous
// version of squirrel) would otherwise keep those perms. We pre-create
// the file at 0644 and verify the write tightens it to 0600.
func TestWriteRcloneConfigTightensExistingPermissions(t *testing.T) {
	cfg := writeFakeConfig(t, `
[destinations.nas]
type = "sftp"
host = "nas.local"
user = "martin"
root = "/data"
password = "p"
`)
	target := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(target, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed loose-perm file: %v", err)
	}
	r := &Rclone{}
	if _, err := r.WriteRcloneConfig(target, cfg.Destinations); err != nil {
		t.Fatalf("WriteRcloneConfig: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 0600 (chmod on existing file failed)", info.Mode().Perm())
	}
}

func TestWriteRcloneConfigSkipsLocalDestinations(t *testing.T) {
	// type=local does not need an rclone remote — rclone addresses local
	// paths directly. Writing it into rclone.conf would create a section
	// with no body, which is harmless but noisy. Verify it's omitted.
	cfg := writeFakeConfig(t, `
[destinations.scratch]
type = "local"
root = "/tmp/scratch"
`)
	r := &Rclone{}
	target := filepath.Join(t.TempDir(), "rclone.conf")
	if _, err := r.WriteRcloneConfig(target, cfg.Destinations); err != nil {
		t.Fatalf("WriteRcloneConfig: %v", err)
	}
	body, _ := os.ReadFile(target)
	if strings.Contains(string(body), "scratch") {
		t.Fatalf("local destination should not appear in rclone.conf:\n%s", body)
	}
}

// TestWriteRcloneConfigRendersCryptOverlay is the file-level golden for a
// crypt destination: the underlying remote section exactly as without
// crypt, then the overlay section wrapping it at the destination root.
func TestWriteRcloneConfigRendersCryptOverlay(t *testing.T) {
	cfg := writeFakeConfig(t, `
[destinations.offsite]
type = "sftp"
host = "host.example"
user = "u"
root = "/data"
password = "transport-pw"

[destinations.offsite.crypt]
password  = "obscured-pw"
password2 = "obscured-salt"
`)
	r := &Rclone{}
	target := filepath.Join(t.TempDir(), "rclone.conf")
	if _, err := r.WriteRcloneConfig(target, cfg.Destinations); err != nil {
		t.Fatalf("WriteRcloneConfig: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read rclone.conf: %v", err)
	}
	want := `[offsite]
type = sftp
host = host.example
user = u
blake3sum_command = b3sum
password = transport-pw

[offsite-crypt]
type = crypt
remote = offsite:/data
filename_encryption = off
directory_name_encryption = false
password = obscured-pw
password2 = obscured-salt
`
	if string(body) != want {
		t.Fatalf("rclone.conf:\n%s\nwant:\n%s", body, want)
	}
}

// TestWriteRcloneConfigSkipsUnchanged verifies the content-comparison
// short-circuit: a second render of identical destinations reports
// wrote=false and leaves the file's mtime untouched. The mtime is pinned
// to a known past instant via Chtimes so the assertion can't race the
// filesystem clock — if the function rewrote the file, the rename would
// stamp a fresh mtime and the comparison would fail deterministically.
func TestWriteRcloneConfigSkipsUnchanged(t *testing.T) {
	cfg := writeFakeConfig(t, `
[destinations.nas]
type = "sftp"
host = "nas.local"
user = "martin"
root = "/data"
password = "p"
`)
	r := &Rclone{}
	target := filepath.Join(t.TempDir(), "rclone.conf")

	wrote, err := r.WriteRcloneConfig(target, cfg.Destinations)
	if err != nil {
		t.Fatalf("first WriteRcloneConfig: %v", err)
	}
	if !wrote {
		t.Fatalf("first write reported wrote=false; want a fresh file to be written")
	}

	pinned := time.Unix(1_000_000, 0)
	if err := os.Chtimes(target, pinned, pinned); err != nil {
		t.Fatalf("pin mtime: %v", err)
	}

	wrote, err = r.WriteRcloneConfig(target, cfg.Destinations)
	if err != nil {
		t.Fatalf("second WriteRcloneConfig: %v", err)
	}
	if wrote {
		t.Fatalf("identical render reported wrote=true; want the write skipped")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().Equal(pinned) {
		t.Fatalf("mtime changed to %v after identical render; want it left at %v (write was not skipped)", info.ModTime(), pinned)
	}
}

// TestRunCopyLocalRoundTrip is the end-to-end happy-path test: copy a
// directory tree to another directory using rclone's `local:` semantics
// (just absolute paths), and confirm parsed result counts.
func TestRunCopyLocalRoundTrip(t *testing.T) {
	r := requireRclone(t)
	r.Config = filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(r.Config, []byte{}, 0o600); err != nil {
		t.Fatalf("seed rclone.conf: %v", err)
	}

	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "b.txt"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := r.Run(context.Background(),
		"copy", "--checksum", "--hash", "blake3", src+"/", dst+"/")
	if err != nil {
		t.Fatalf("Run: %v (result=%+v)", err, res)
	}
	if res.Transferred != 2 {
		t.Fatalf("Transferred = %d, want 2: %+v", res.Transferred, res)
	}
	if res.Errors != 0 {
		t.Fatalf("Errors = %d, want 0: %+v", res.Errors, res)
	}
	if _, err := os.Stat(filepath.Join(dst, "a.txt")); err != nil {
		t.Fatalf("destination missing a.txt: %v", err)
	}
}

// TestRunCopySkipsUnchanged verifies that a second invocation with the
// same source/destination transfers nothing and instead reports the files
// as Checked (i.e. their BLAKE3 was verified to already match).
func TestRunCopySkipsUnchanged(t *testing.T) {
	r := requireRclone(t)
	r.Config = filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(r.Config, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(),
		"copy", "--checksum", "--hash", "blake3", src+"/", dst+"/"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	res, err := r.Run(context.Background(),
		"copy", "--checksum", "--hash", "blake3", src+"/", dst+"/")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Transferred != 0 {
		t.Fatalf("Transferred = %d, want 0 on idempotent re-run", res.Transferred)
	}
	if res.Checked < 1 {
		t.Fatalf("Checked = %d, want ≥ 1 (file was verified at destination)", res.Checked)
	}
}

// TestRunFatalErrorMissingSource confirms that an invocation against a
// nonexistent source root returns an error and marks the result fatal.
// We rely on this to decide between RunStatusFailed (no usable output) and
// RunStatusPartial (some files transferred, some failed).
func TestRunFatalErrorMissingSource(t *testing.T) {
	r := requireRclone(t)
	r.Config = filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(r.Config, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(),
		"copy", filepath.Join(t.TempDir(), "missing-src")+"/", t.TempDir()+"/")
	if err == nil {
		t.Fatalf("expected error from missing source, got nil; result=%+v", res)
	}
	if !res.FatalError && res.Errors == 0 {
		t.Fatalf("expected FatalError or Errors>0, got %+v", res)
	}
}

// TestRunBackupDirMoves overwrites a destination file and asserts the old
// content lands in the backup-dir. This proves we get destination
// immutability "for free" from rclone's --backup-dir flag.
func TestRunBackupDirMoves(t *testing.T) {
	r := requireRclone(t)
	r.Config = filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(r.Config, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(),
		"copy", "--checksum", "--hash", "blake3", src+"/", dst+"/"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Bump the content. The new bytes will overwrite a.txt; the old "v1"
	// must end up in the backup-dir to satisfy destination immutability.
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("v2 new content"), 0o644); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dst, ".squirrel-history", "run-2")
	if _, err := r.Run(context.Background(),
		"copy", "--checksum", "--hash", "blake3",
		"--backup-dir", backup,
		"--filter", "- /.squirrel-history/**",
		src+"/", dst+"/"); err != nil {
		t.Fatalf("second run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil || string(got) != "v2 new content" {
		t.Fatalf("destination a.txt = %q, err=%v; want new content", got, err)
	}
	old, err := os.ReadFile(filepath.Join(backup, "a.txt"))
	if err != nil {
		t.Fatalf("backup-dir missing prior content: %v", err)
	}
	if string(old) != "v1" {
		t.Fatalf("backup-dir a.txt = %q, want v1", old)
	}
}

// TestRemoteSubpathURIBucketBackend pins that plain (non-crypt) remote
// URIs include the bucket for bucket-addressed backends. rclone treats
// the first path segment as the bucket, so omitting it sends transfers
// to a bucket named after root's first segment or the volume itself.
func TestRemoteSubpathURIBucketBackend(t *testing.T) {
	t.Setenv("K", "k")
	cfg := writeFakeConfig(t, `
[destinations.arch]
type              = "s3"
provider          = "Other"
bucket            = "bkt"
root              = "/"
access_key_id     = { env = "K" }
secret_access_key = { env = "K" }

[destinations.box]
type     = "sftp"
host     = "h"
user     = "u"
root     = "/abs"
password = { env = "K" }
`)
	if got, want := remoteSubpathURI(cfg.Destinations["arch"], "docs"), "arch:bkt/docs"; got != want {
		t.Errorf("s3 URI = %q, want %q", got, want)
	}
	if got, want := remoteSubpathURI(cfg.Destinations["box"], "docs"), "box:/abs/docs"; got != want {
		t.Errorf("sftp URI = %q, want %q", got, want)
	}
}

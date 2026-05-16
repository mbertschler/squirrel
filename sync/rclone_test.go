package sync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/config"
)

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
	parseJSONLog(strings.NewReader(stream), &r)

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
	if err := r.WriteRcloneConfig(target, cfg.Destinations); err != nil {
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
	if err := r.WriteRcloneConfig(target, cfg.Destinations); err != nil {
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
	if err := r.WriteRcloneConfig(target, cfg.Destinations); err != nil {
		t.Fatalf("WriteRcloneConfig: %v", err)
	}
	body, _ := os.ReadFile(target)
	if strings.Contains(string(body), "scratch") {
		t.Fatalf("local destination should not appear in rclone.conf:\n%s", body)
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

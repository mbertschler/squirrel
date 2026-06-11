package sync

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
)

// fakeKopiaScript is the PATH-shim stand-in for the kopia binary. It
// appends one argv line and one env line per invocation to
// $KOPIA_FAKE_LOG, then plays back behaviour keyed on the subcommand
// via KOPIA_FAKE_*_EXIT variables, so tests can assert exactly what
// squirrel asked kopia to do without a real repository.
const fakeKopiaScript = `#!/bin/sh
{
  printf 'argv:'
  for a in "$@"; do printf ' %s' "$a"; done
  printf '\n'
  printf 'env:KOPIA_PASSWORD=%s\n' "$KOPIA_PASSWORD"
} >> "$KOPIA_FAKE_LOG"
case "$1 $2" in
"repository connect") exit "${KOPIA_FAKE_CONNECT_EXIT:-0}" ;;
"repository create") exit "${KOPIA_FAKE_CREATE_EXIT:-0}" ;;
"snapshot create")
  if [ "${KOPIA_FAKE_SNAPSHOT_EXIT:-0}" != 0 ]; then
    echo "fake snapshot failure" >&2
    exit "${KOPIA_FAKE_SNAPSHOT_EXIT}"
  fi
  cat "$KOPIA_FAKE_SNAPSHOT_JSON"
  ;;
"snapshot verify")
  if [ "${KOPIA_FAKE_VERIFY_EXIT:-0}" != 0 ]; then
    echo "fake verify failure" >&2
    exit "${KOPIA_FAKE_VERIFY_EXIT}"
  fi
  ;;
*) echo "unexpected kopia subcommand: $*" >&2; exit 64 ;;
esac
`

// fakeSnapshotJSON mirrors the manifest shape `kopia snapshot create
// --json` prints (trimmed to the fields squirrel reads, captured from
// kopia 0.23).
const fakeSnapshotJSON = `{"id":"snap123","source":{"host":"h","userName":"u","path":"/v"},` +
	`"rootEntry":{"name":"src","type":"d","obj":"k1","summ":{"size":1234,"files":3,"dirs":1,"numFailed":0}}}` + "\n"

// installFakeKopia puts a fake kopia shim at the head of PATH and
// returns the log file it records invocations into. Behaviour knobs are
// plain env vars so individual tests tune them with t.Setenv.
func installFakeKopia(t *testing.T) (logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake kopia shim is a POSIX shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kopia"), []byte(fakeKopiaScript), 0o755); err != nil {
		t.Fatalf("write fake kopia: %v", err)
	}
	jsonPath := filepath.Join(dir, "snapshot.json")
	if err := os.WriteFile(jsonPath, []byte(fakeSnapshotJSON), 0o644); err != nil {
		t.Fatalf("write fake snapshot json: %v", err)
	}
	logPath = filepath.Join(dir, "calls.log")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KOPIA_FAKE_LOG", logPath)
	t.Setenv("KOPIA_FAKE_SNAPSHOT_JSON", jsonPath)
	return logPath
}

// kopiaFixture is the kopia analogue of syncFixture: a store, a config
// with one volume syncing to one kopia destination, and the Tools built
// the way the CLI builds them. No rclone involved.
type kopiaFixture struct {
	store *store.Store
	cfg   *config.Config
	tools Tools
	pair  Pair
}

func setupKopiaFixture(t *testing.T) *kopiaFixture {
	t.Helper()
	root := t.TempDir()
	volPath := filepath.Join(root, "src")
	if err := os.MkdirAll(volPath, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(volPath, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(root, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	cfgPath := filepath.Join(root, "config.toml")
	cfgBody := "[destinations.mirror]\ntype = \"kopia\"\nroot = \"" + filepath.Join(root, "repo") + "\"\npassword = \"hunter2\"\n\n" +
		"[volumes.pics]\npath = \"" + volPath + "\"\nsync_to = [\"mirror\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	pairs, err := PairsFor(cfg, "", "")
	if err != nil {
		t.Fatalf("PairsFor: %v", err)
	}
	if len(pairs) != 1 || pairs[0].Destination == nil || pairs[0].Destination.Type != "kopia" {
		t.Fatalf("pairs = %+v, want one kopia pair", pairs)
	}
	tools, err := ToolsFor(cfg, pairs, nil)
	if err != nil {
		t.Fatalf("ToolsFor: %v", err)
	}
	if tools.Kopia == nil {
		t.Fatalf("ToolsFor left Kopia nil for a kopia pair")
	}

	if _, err := index.Index(context.Background(), s, volPath, index.Options{Name: "pics"}); err != nil {
		t.Fatalf("index.Index: %v", err)
	}
	return &kopiaFixture{store: s, cfg: cfg, tools: tools, pair: pairs[0]}
}

// readCallLog splits the fake binary's log into argv lines and env
// lines for assertion.
func readCallLog(t *testing.T, logPath string) (argv, env []string) {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake kopia log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		switch {
		case strings.HasPrefix(line, "argv: "):
			argv = append(argv, strings.TrimPrefix(line, "argv: "))
		case strings.HasPrefix(line, "env:"):
			env = append(env, strings.TrimPrefix(line, "env:"))
		}
	}
	return argv, env
}

func TestKopiaPushHappyPath(t *testing.T) {
	logPath := installFakeKopia(t)
	// A stale parent-shell export must not shadow the configured
	// password: the wrapper strips it before appending its own.
	t.Setenv("KOPIA_PASSWORD", "stale-parent-value")
	f := setupKopiaFixture(t)

	rep, err := RunPair(context.Background(), f.store, f.tools, f.pair, Options{})
	if err != nil {
		t.Fatalf("RunPair: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusSuccess {
		t.Fatalf("Status = %q, want success", rep.Status)
	}
	if !rep.Verification.Verified() || rep.Verification.Method != VerifyMethodKopia {
		t.Fatalf("Verification = %+v, want verified kopia result", rep.Verification)
	}
	if rep.Verification.SnapshotID != "snap123" || rep.Verification.Files != 3 || rep.Verification.Bytes != 1234 {
		t.Fatalf("Verification = %+v, want snap123 / 3 files / 1234 bytes", rep.Verification)
	}

	run, err := f.store.GetRun(context.Background(), rep.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Kind != store.RunKindSync || !run.Destination.Valid || run.Destination.String != "mirror" {
		t.Fatalf("run = %+v, want kind=sync destination=mirror", run)
	}
	if run.Status != store.RunStatusSuccess || run.FileCount != 3 {
		t.Fatalf("run = %+v, want success with file_count=3", run)
	}
	if !run.Shallow.Valid || run.Shallow.Bool {
		t.Fatalf("run.Shallow = %+v, want false (kopia verifies its own hashes)", run.Shallow)
	}

	argv, env := readCallLog(t, logPath)
	cfgFile := filepath.Join(filepath.Dir(f.cfg.Path), "kopia-mirror.config")
	repo := f.pair.Destination.Root
	wantArgv := []string{
		"repository connect filesystem --path " + repo + " --no-persist-credentials --config-file " + cfgFile,
		"snapshot create " + f.pair.Volume.Path + " --json --config-file " + cfgFile,
		"snapshot verify snap123 --config-file " + cfgFile,
	}
	if len(argv) != len(wantArgv) {
		t.Fatalf("argv lines = %q, want %q", argv, wantArgv)
	}
	for i := range wantArgv {
		if argv[i] != wantArgv[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, argv[i], wantArgv[i])
		}
		if strings.Contains(argv[i], "hunter2") {
			t.Fatalf("argv[%d] leaks the repository password: %q", i, argv[i])
		}
	}
	for i, e := range env {
		if e != "KOPIA_PASSWORD=hunter2" {
			t.Fatalf("env[%d] = %q, want the password via KOPIA_PASSWORD", i, e)
		}
	}
}

func TestKopiaConnectFallsBackToCreate(t *testing.T) {
	logPath := installFakeKopia(t)
	t.Setenv("KOPIA_FAKE_CONNECT_EXIT", "1")
	f := setupKopiaFixture(t)

	rep, err := RunPair(context.Background(), f.store, f.tools, f.pair, Options{})
	if err != nil {
		t.Fatalf("RunPair: %v (rep=%+v)", err, rep)
	}
	argv, _ := readCallLog(t, logPath)
	if len(argv) != 4 || !strings.HasPrefix(argv[1], "repository create filesystem --path ") {
		t.Fatalf("argv = %q, want connect, create, snapshot create, snapshot verify", argv)
	}
}

func TestKopiaCreateFailureRecordsFailedRun(t *testing.T) {
	installFakeKopia(t)
	t.Setenv("KOPIA_FAKE_CONNECT_EXIT", "1")
	t.Setenv("KOPIA_FAKE_CREATE_EXIT", "1")
	f := setupKopiaFixture(t)

	rep, err := RunPair(context.Background(), f.store, f.tools, f.pair, Options{})
	if err == nil {
		t.Fatalf("expected error, got rep=%+v", rep)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaks the repository password: %v", err)
	}
	if rep.Status != store.RunStatusFailed {
		t.Fatalf("Status = %q, want failed", rep.Status)
	}
	if rep.Verification.Method != VerifyMethodKopia {
		t.Fatalf("Method = %q on early failure, want %q so output renders kopia-shaped", rep.Verification.Method, VerifyMethodKopia)
	}
	run, getErr := f.store.GetRun(context.Background(), rep.RunID)
	if getErr != nil {
		t.Fatalf("GetRun: %v", getErr)
	}
	if run.Status != store.RunStatusFailed || !run.Error.Valid || run.Error.String == "" {
		t.Fatalf("run = %+v, want failed with an error message", run)
	}
	if strings.Contains(run.Error.String, "hunter2") {
		t.Fatalf("runs row leaks the repository password: %q", run.Error.String)
	}
}

func TestKopiaSnapshotWithFailedFilesIsPartial(t *testing.T) {
	installFakeKopia(t)
	f := setupKopiaFixture(t)
	partial := `{"id":"snap123","rootEntry":{"summ":{"size":1000,"files":2,"numFailed":1}}}`
	if err := os.WriteFile(os.Getenv("KOPIA_FAKE_SNAPSHOT_JSON"), []byte(partial), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := RunPair(context.Background(), f.store, f.tools, f.pair, Options{})
	if err != nil {
		t.Fatalf("RunPair: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusPartial {
		t.Fatalf("Status = %q, want partial", rep.Status)
	}
	if rep.Verification.Verified() {
		t.Fatalf("a snapshot with failed files must stay unverified")
	}
	run, err := f.store.GetRun(context.Background(), rep.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunStatusPartial {
		t.Fatalf("run status = %q, want partial", run.Status)
	}
}

func TestKopiaVerifyFailureFailsRun(t *testing.T) {
	installFakeKopia(t)
	t.Setenv("KOPIA_FAKE_VERIFY_EXIT", "1")
	f := setupKopiaFixture(t)

	rep, err := RunPair(context.Background(), f.store, f.tools, f.pair, Options{})
	if err == nil || !strings.Contains(err.Error(), "snapshot verify") {
		t.Fatalf("expected snapshot-verify error, got %v", err)
	}
	if rep.Status != store.RunStatusFailed {
		t.Fatalf("Status = %q, want failed", rep.Status)
	}
	if rep.Verification.Verified() {
		t.Fatalf("verification must stay unverified when kopia's verify fails")
	}
	if rep.Verification.SnapshotID != "snap123" {
		t.Fatalf("SnapshotID = %q, want the created snapshot for forensics", rep.Verification.SnapshotID)
	}
}

func TestKopiaDryRunRefused(t *testing.T) {
	installFakeKopia(t)
	f := setupKopiaFixture(t)

	rep, err := RunPair(context.Background(), f.store, f.tools, f.pair, Options{DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "dry-run") {
		t.Fatalf("expected dry-run refusal, got err=%v rep=%+v", err, rep)
	}
	runs, _ := f.store.ListRuns(context.Background(), store.ListRunsOpts{})
	for _, r := range runs {
		if r.Kind == store.RunKindSync {
			t.Fatalf("dry-run wrote a sync runs row: %+v", r)
		}
	}
}

func TestKopiaRequiresIndexedVolume(t *testing.T) {
	installFakeKopia(t)
	f := setupKopiaFixture(t)
	// A second volume in config but never indexed.
	vol := &config.Volume{Name: "fresh", Path: t.TempDir()}
	pair := Pair{Volume: vol, Destination: f.pair.Destination}

	_, err := RunPair(context.Background(), f.store, f.tools, pair, Options{})
	if err == nil || !strings.Contains(err.Error(), "never been indexed") {
		t.Fatalf("expected unindexed-volume refusal, got %v", err)
	}
}

func TestRestoreRefusesKopiaDestination(t *testing.T) {
	installFakeKopia(t)
	f := setupKopiaFixture(t)
	_, err := Restore(context.Background(), f.store, nil, f.pair.Volume, f.pair.Destination, RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "kopia") {
		t.Fatalf("expected kopia-restore refusal, got %v", err)
	}
}

// TestKopiaIntegrationRealBinary exercises the full
// connect→create→snapshot→verify cycle against a real kopia repository
// in a temp directory. Runs only where the kopia binary is installed.
func TestKopiaIntegrationRealBinary(t *testing.T) {
	if _, err := exec.LookPath("kopia"); err != nil {
		t.Skip("kopia not on PATH; install kopia to run this test")
	}
	f := setupKopiaFixture(t)

	rep, err := RunPair(context.Background(), f.store, f.tools, f.pair, Options{})
	if err != nil {
		t.Fatalf("RunPair: %v (rep=%+v)", err, rep)
	}
	if rep.Status != store.RunStatusSuccess || !rep.Verification.Verified() {
		t.Fatalf("rep = %+v, want verified success", rep)
	}
	if rep.Verification.SnapshotID == "" || rep.Verification.Files < 1 {
		t.Fatalf("Verification = %+v, want a snapshot id and file count", rep.Verification)
	}
	cfgFile := filepath.Join(filepath.Dir(f.cfg.Path), "kopia-mirror.config")
	if _, err := os.Stat(cfgFile + ".kopia-password"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("kopia persisted the repository password to %s (stat err=%v); --no-persist-credentials must prevent the sidecar", cfgFile+".kopia-password", err)
	}

	// Second push re-connects and snapshots again without error.
	rep2, err := RunPair(context.Background(), f.store, f.tools, f.pair, Options{})
	if err != nil {
		t.Fatalf("second RunPair: %v (rep=%+v)", err, rep2)
	}
	if rep2.Status != store.RunStatusSuccess {
		t.Fatalf("second push status = %q, want success", rep2.Status)
	}
}

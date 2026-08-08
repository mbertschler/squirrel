package agent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

const driftConfigBody = `
[agent]
listen = "127.0.0.1:0"
[agent.auth]
token = "tok"

[volumes.pictures]
path = "/tmp/pictures"
`

// driftFixture is a monitor wired to a real store and a real config file
// on disk, plus the handles a test pokes at: the file to edit, the store to
// read the latch from, and the agent's log.
type driftFixture struct {
	t       *testing.T
	monitor *configMonitor
	store   *store.Store
	path    string
	logBuf  *bytes.Buffer
}

func newDriftFixture(t *testing.T) *driftFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(driftConfigBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	logBuf := &bytes.Buffer{}
	srv, err := New(Config{
		Listen:       "127.0.0.1:0",
		Token:        "tok",
		Version:      "test",
		ConfigPath:   cfg.Path,
		ConfigDigest: cfg.Digest,
		Logger:       slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, s)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	monitor := newConfigMonitor(srv)
	if monitor == nil {
		t.Fatal("newConfigMonitor returned nil for a server that knows its config")
	}
	return &driftFixture{t: t, monitor: monitor, store: s, path: path, logBuf: logBuf}
}

// rewrite replaces the config file's contents and gives it a distinct
// mtime, so a test that expects "no drift" can't pass merely because the
// timestamp stayed put.
func (f *driftFixture) rewrite(body string) {
	f.t.Helper()
	if err := os.WriteFile(f.path, []byte(body), 0o600); err != nil {
		f.t.Fatalf("rewrite config: %v", err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(f.path, future, future); err != nil {
		f.t.Fatalf("chtimes: %v", err)
	}
}

func (f *driftFixture) latch() (store.ConfigDrift, bool) {
	f.t.Helper()
	d, err := f.store.GetConfigDrift(context.Background())
	if store.IsNotFound(err) {
		return store.ConfigDrift{}, false
	}
	if err != nil {
		f.t.Fatalf("GetConfigDrift: %v", err)
	}
	return d, true
}

// TestConfigMonitorRaisesOnEdit is the core F9 path: the operator edits the
// config and does not restart, so the agent latches a standing state naming
// the file.
func TestConfigMonitorRaisesOnEdit(t *testing.T) {
	f := newDriftFixture(t)
	ctx := context.Background()

	f.monitor.check(ctx)
	if _, ok := f.latch(); ok {
		t.Fatal("latched with the config untouched")
	}

	f.rewrite(driftConfigBody + "\n[volumes.docs]\npath = \"/tmp/docs\"\n")
	f.monitor.check(ctx)
	d, ok := f.latch()
	if !ok {
		t.Fatal("no latch after the config changed on disk")
	}
	if d.Path != f.path {
		t.Fatalf("latch path = %q, want %q", d.Path, f.path)
	}
	if !strings.Contains(f.logBuf.String(), store.ConfigDriftMessage) {
		t.Fatalf("agent log does not carry the drift message:\n%s", f.logBuf.String())
	}
}

// TestConfigMonitorIgnoresSameBytesRewrite is the explicit "compare
// content, not mtime" guarantee: a rewrite producing identical bytes is not
// a change and must latch nothing.
func TestConfigMonitorIgnoresSameBytesRewrite(t *testing.T) {
	f := newDriftFixture(t)
	f.rewrite(driftConfigBody)
	f.monitor.check(context.Background())
	if _, ok := f.latch(); ok {
		t.Fatal("latched on a same-bytes rewrite — the check is comparing timestamps, not content")
	}
}

// TestConfigMonitorRaisesOnceThenClearsOnRevert covers the rest of the
// episode: repeated detections leave the latch alone, and putting the file
// back the way the agent loaded it clears the latch without a restart.
func TestConfigMonitorRaisesOnceThenClearsOnRevert(t *testing.T) {
	f := newDriftFixture(t)
	ctx := context.Background()

	f.rewrite(driftConfigBody + "\n# edited\n")
	f.monitor.check(ctx)
	first, ok := f.latch()
	if !ok {
		t.Fatal("no latch after the config changed")
	}
	f.monitor.check(ctx)
	again, ok := f.latch()
	if !ok {
		t.Fatal("latch disappeared on a second check")
	}
	if again.RaisedRunID != first.RaisedRunID || again.RaisedAtNs != first.RaisedAtNs {
		t.Fatalf("second check restarted the episode: %+v then %+v", first, again)
	}

	f.rewrite(driftConfigBody)
	f.monitor.check(ctx)
	if _, ok := f.latch(); ok {
		t.Fatal("latch survived the config being reverted to the loaded bytes")
	}
}

// TestConfigMonitorUnreadableFileDoesNotLatch: an editor that writes via a
// rename makes the file briefly absent, which answers neither "changed" nor
// "unchanged". The check logs and waits for a settled file rather than
// raising an episode for an edit that may never land.
func TestConfigMonitorUnreadableFileDoesNotLatch(t *testing.T) {
	f := newDriftFixture(t)
	if err := os.Remove(f.path); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	f.monitor.check(context.Background())
	if _, ok := f.latch(); ok {
		t.Fatal("latched while the config file was missing")
	}
	if !strings.Contains(f.logBuf.String(), "config drift check failed") {
		t.Fatalf("unreadable config was not logged:\n%s", f.logBuf.String())
	}
}

// TestConfigMonitorRunClearsStaleLatch is the restart path: the latch a
// previous process raised is cleared before the first tick, because this
// process has just loaded the file the latch was complaining about.
func TestConfigMonitorRunClearsStaleLatch(t *testing.T) {
	f := newDriftFixture(t)
	ctx := context.Background()
	if _, err := f.store.RaiseConfigDrift(ctx, f.path, f.monitor.loaded, bytes.Repeat([]byte{7}, config.DigestLen)); err != nil {
		t.Fatalf("plant a previous process's latch: %v", err)
	}

	// A cadence far longer than the test: run() must clear before its
	// first tick, so nothing here waits on the ticker.
	f.monitor.every = time.Hour
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.monitor.run(runCtx)
	}()
	waitFor(t, func() bool {
		_, ok := f.latch()
		return !ok
	}, "the stale latch to be cleared at monitor startup")
	cancel()
	<-done
}

// waitFor polls cond until it holds or the test's patience runs out. The
// monitor's own clock is a real ticker, so a poll is the honest way to wait
// for its startup work without sleeping a fixed amount.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

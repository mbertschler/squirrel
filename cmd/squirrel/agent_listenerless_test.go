package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCLIAgentListenerLessRunsScheduler covers F35 end-to-end: an [agent]
// block with no `listen` starts the scheduler without binding an HTTP
// listener, logs the scheduler-only banner, and shuts down cleanly on
// context cancel. The volume declares an index cadence (no sync_to), so no
// rclone binary is needed to bring the agent up.
func TestCLIAgentListenerLessRunsScheduler(t *testing.T) {
	dir := t.TempDir()
	vol := filepath.Join(dir, "photos")
	if err := os.MkdirAll(vol, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(vol, "a.jpg"), "x")
	dbPath := filepath.Join(dir, "index.db")
	cfgPath := filepath.Join(dir, "config.toml")
	body := fmt.Sprintf("db = %q\n\n[agent]\n\n[volumes.photos]\npath = %q\nindex_every = \"1h\"\n", dbPath, vol)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	isolateConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	buf := &syncBuf{}
	root := newRootCmd()
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"--config", cfgPath, "agent"})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(buf.String(), "agent scheduler running") {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "agent scheduler running") {
		cancel()
		t.Fatalf("scheduler-only banner never appeared:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "agent listening") {
		cancel()
		t.Fatalf("listener-less agent unexpectedly bound a listener:\n%s", buf.String())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("agent exited with error: %v\noutput:\n%s", err, buf.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("agent did not shut down within timeout\noutput:\n%s", buf.String())
	}
}

// TestCLIAgentListenerLessNoWork guards the fail-fast: a listener-less
// agent with no cadence and no scan has nothing to run and must refuse to
// start rather than idle silently.
func TestCLIAgentListenerLessNoWork(t *testing.T) {
	dir := t.TempDir()
	vol := filepath.Join(dir, "photos")
	if err := os.MkdirAll(vol, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "index.db")
	cfgPath := filepath.Join(dir, "config.toml")
	body := fmt.Sprintf("db = %q\n\n[agent]\n\n[volumes.photos]\npath = %q\n", dbPath, vol)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runCLIExpectErr(t, "--config", cfgPath, "agent")
	if !strings.Contains(err.Error(), "nothing to run") {
		t.Fatalf("expected nothing-to-run error, got %v", err)
	}
}

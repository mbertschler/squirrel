package hook

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunSuccess(t *testing.T) {
	out := Run(context.Background(), Spec{
		Command: []string{"sh", "-c", "exit 0"},
		Timeout: 5 * time.Second,
	})
	if !out.Succeeded() {
		t.Fatalf("Succeeded = false, err = %v", out.Err)
	}
	if !out.HasExitCode || out.ExitCode != 0 {
		t.Fatalf("ExitCode = %d (has=%v), want 0", out.ExitCode, out.HasExitCode)
	}
	if out.EndedAtNs < out.StartedAtNs {
		t.Fatalf("EndedAtNs %d < StartedAtNs %d", out.EndedAtNs, out.StartedAtNs)
	}
}

func TestRunNonZeroExit(t *testing.T) {
	out := Run(context.Background(), Spec{
		Command: []string{"sh", "-c", "exit 7"},
		Timeout: 5 * time.Second,
	})
	if out.Succeeded() {
		t.Fatalf("Succeeded = true, want failure")
	}
	if !out.HasExitCode || out.ExitCode != 7 {
		t.Fatalf("ExitCode = %d (has=%v), want 7", out.ExitCode, out.HasExitCode)
	}
	if out.TimedOut {
		t.Fatalf("TimedOut = true, want false")
	}
}

func TestRunPassesEnv(t *testing.T) {
	out := Run(context.Background(), Spec{
		Command: []string{"sh", "-c", `printf '%s|%s|%s|%s|%s' "$SQUIRREL_VOLUME" "$SQUIRREL_PATH" "$SQUIRREL_RUN_ID" "$SQUIRREL_CHANGED" "$SQUIRREL_TRIGGER" 1>&2`},
		Volume:  "photos",
		Path:    "/mnt/hdd1/photos",
		RunID:   42,
		Changed: true,
		Trigger: TriggerChange,
		Timeout: 5 * time.Second,
	})
	if !out.Succeeded() {
		t.Fatalf("Succeeded = false, err = %v", out.Err)
	}
	want := "photos|/mnt/hdd1/photos|42|true|change"
	if out.Stderr != want {
		t.Fatalf("env = %q, want %q", out.Stderr, want)
	}
}

func TestRunIntervalRunIDEmpty(t *testing.T) {
	out := Run(context.Background(), Spec{
		Command: []string{"sh", "-c", `printf '%s' "$SQUIRREL_RUN_ID" 1>&2`},
		RunID:   0,
		Trigger: TriggerInterval,
		Timeout: 5 * time.Second,
	})
	if out.Stderr != "" {
		t.Fatalf("SQUIRREL_RUN_ID = %q, want empty for interval hook", out.Stderr)
	}
}

func TestRunTimeout(t *testing.T) {
	start := time.Now()
	out := Run(context.Background(), Spec{
		Command: []string{"sh", "-c", "sleep 10"},
		Timeout: 50 * time.Millisecond,
	})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run took %s, timeout was not enforced", elapsed)
	}
	if !out.TimedOut {
		t.Fatalf("TimedOut = false, want true")
	}
	if out.Succeeded() {
		t.Fatalf("Succeeded = true, want failure on timeout")
	}
}

func TestRunSpawnFailure(t *testing.T) {
	out := Run(context.Background(), Spec{
		Command: []string{"squirrel-no-such-binary-xyzzy"},
		Timeout: 5 * time.Second,
	})
	if out.Succeeded() {
		t.Fatalf("Succeeded = true, want failure")
	}
	if out.HasExitCode {
		t.Fatalf("HasExitCode = true, want false for a process that never ran")
	}
	if out.TimedOut {
		t.Fatalf("TimedOut = true, want false for spawn failure")
	}
}

func TestRunParentCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := Run(ctx, Spec{
		Command: []string{"sh", "-c", "sleep 10"},
		Timeout: 5 * time.Second,
	})
	if out.Succeeded() {
		t.Fatalf("Succeeded = true, want failure on cancellation")
	}
	if out.TimedOut {
		t.Fatalf("TimedOut = true, want a cancellation (not a timeout)")
	}
	if !strings.Contains(out.Err.Error(), "cancelled") {
		t.Fatalf("Err = %v, want a cancellation message", out.Err)
	}
}

func TestRunEmptyCommand(t *testing.T) {
	out := Run(context.Background(), Spec{Timeout: time.Second})
	if out.Succeeded() {
		t.Fatalf("Succeeded = true, want failure for empty command")
	}
}

func TestRunNonPositiveTimeout(t *testing.T) {
	out := Run(context.Background(), Spec{
		Command: []string{"sh", "-c", "exit 0"},
		Timeout: 0,
	})
	if out.Succeeded() {
		t.Fatalf("Succeeded = true, want failure for a non-positive timeout")
	}
	if out.TimedOut {
		t.Fatalf("TimedOut = true, want a clear config error, not a phantom timeout")
	}
	if !strings.Contains(out.Err.Error(), "timeout must be positive") {
		t.Fatalf("Err = %v, want a 'timeout must be positive' error", out.Err)
	}
}

func TestBoundedBufferKeepsTail(t *testing.T) {
	// Single oversized write keeps the tail.
	var b boundedBuffer
	b.limit = 4
	n, err := b.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = (%d, %v), want (5, nil) — overflow is reported as written, not failed", n, err)
	}
	if b.String() != "ello" {
		t.Fatalf("buffer = %q, want %q (the tail, not the head)", b.String(), "ello")
	}

	// Tail is maintained across multiple writes.
	var b2 boundedBuffer
	b2.limit = 4
	b2.Write([]byte("ab"))
	b2.Write([]byte("cdef"))
	if b2.String() != "cdef" {
		t.Fatalf("buffer = %q, want %q (last 4 bytes across writes)", b2.String(), "cdef")
	}
}

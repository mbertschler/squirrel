// Package hook runs per-volume, best-effort external commands on behalf
// of the squirrel agent (#84). It is deliberately tool-agnostic: it exec's
// a user-configured command WITHOUT a shell, passes context through
// SQUIRREL_* environment variables, bounds the run with a timeout, and
// reports only the generic outcome (exit code, timestamps, a stderr tail).
// It never interprets what the command does — backup, verify, anything.
//
// The package is a pure library: it writes to no global stdout/stderr and
// surfaces everything via the returned Outcome. The caller (agent) decides
// how to record and log it.
package hook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// Trigger discriminates why a hook fired. It is passed to the command as
// SQUIRREL_TRIGGER so a single command can branch (e.g. back up on change,
// verify on interval) without squirrel modelling either concept.
type Trigger string

const (
	TriggerChange   Trigger = "change"
	TriggerInterval Trigger = "interval"
)

// Env var names forming the hook contract. Documented here so the one
// place that sets them and any future reader agree on the spelling.
const (
	EnvVolume  = "SQUIRREL_VOLUME"
	EnvPath    = "SQUIRREL_PATH"
	EnvRunID   = "SQUIRREL_RUN_ID"
	EnvChanged = "SQUIRREL_CHANGED"
	EnvTrigger = "SQUIRREL_TRIGGER"
)

// maxStderrCapture bounds how much of the command's stderr we keep for the
// recorded diagnostic. A chatty hook must not let squirrel buffer
// unbounded memory; the tail is all an operator needs to see why it failed.
const maxStderrCapture = 8 << 10

// waitDelay bounds the grace period cmd.Wait gives in-flight I/O after the
// process is killed (timeout or shutdown). Small: by the time we are
// killing, we only want to unblock — not to wait on a lingering grandchild.
const waitDelay = 2 * time.Second

// Spec is one resolved hook invocation. Command is exec'd verbatim (no
// shell); the SQUIRREL_* values are derived from the remaining fields.
type Spec struct {
	// Command is argv — Command[0] is the program. Must be non-empty.
	Command []string
	// Volume and Path populate SQUIRREL_VOLUME / SQUIRREL_PATH.
	Volume string
	Path   string
	// RunID populates SQUIRREL_RUN_ID — the run that triggered the hook.
	// Zero renders as empty (interval hooks have no triggering run).
	RunID int64
	// Changed populates SQUIRREL_CHANGED ("true"/"false").
	Changed bool
	// Trigger populates SQUIRREL_TRIGGER.
	Trigger Trigger
	// Timeout bounds the invocation. Must be positive.
	Timeout time.Duration
}

// Outcome is the generic result of one invocation. ExitCode is meaningful
// only when HasExitCode is true (the process ran to completion and
// returned a code); a spawn failure or a timeout leaves it unset. Err is
// nil exactly when the command exited 0.
type Outcome struct {
	StartedAtNs int64
	EndedAtNs   int64
	ExitCode    int
	HasExitCode bool
	TimedOut    bool
	// Stderr is a bounded tail of what the command wrote to stderr.
	Stderr string
	// Err is non-nil on spawn failure, timeout, or non-zero exit. It is a
	// generic description — squirrel never parses the command's output.
	Err error
}

// Succeeded reports whether the command exited 0.
func (o Outcome) Succeeded() bool { return o.Err == nil }

// Run exec's spec.Command without a shell, bounded by spec.Timeout (and by
// ctx — agent shutdown cancels in-flight hooks). It always returns a
// populated Outcome; a non-nil Outcome.Err describes the failure rather
// than being returned separately, because every caller wants to record the
// outcome regardless. ctx cancellation and the timeout both kill the
// process; the timeout is reported as TimedOut, a parent cancellation as a
// plain error.
func Run(ctx context.Context, spec Spec) Outcome {
	out := Outcome{StartedAtNs: time.Now().UnixNano()}
	if len(spec.Command) == 0 {
		out.EndedAtNs = time.Now().UnixNano()
		out.Err = errors.New("hook: empty command")
		return out
	}

	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, spec.Command[0], spec.Command[1:]...)
	cmd.Env = append(os.Environ(), spec.env()...)
	var stderr boundedBuffer
	stderr.limit = maxStderrCapture
	cmd.Stderr = &stderr
	// The command's stdout is the consumer's business, not squirrel's —
	// discard it so a chatty hook can't flood the agent's own streams.
	cmd.Stdout = nil
	// WaitDelay bounds how long Run blocks after the context is cancelled
	// (timeout or shutdown). Without it, a hook that spawns a grandchild
	// which inherits the stderr pipe would keep cmd.Wait blocked until that
	// grandchild exits — exactly the "hung hook wedges the scheduler" case
	// the contract forbids. After the delay the pipe is force-closed and
	// Run returns; any orphaned grandchild is the consumer's problem, not
	// squirrel's.
	cmd.WaitDelay = waitDelay

	runErr := cmd.Run()
	out.EndedAtNs = time.Now().UnixNano()
	out.Stderr = stderr.String()
	classify(&out, runCtx, ctx, spec.Timeout, runErr)
	return out
}

// classify folds the raw exec error into the Outcome's generic shape. The
// timeout check comes first because a deadline-killed process surfaces as
// an ExitError with code -1, which must not be mistaken for a real exit.
func classify(out *Outcome, runCtx, parentCtx context.Context, timeout time.Duration, runErr error) {
	if runErr == nil {
		out.ExitCode = 0
		out.HasExitCode = true
		return
	}
	// Parent cancellation (agent shutdown) takes precedence over the
	// timeout: the run was cut short by us, not by exceeding its budget.
	if parentCtx.Err() != nil {
		out.Err = fmt.Errorf("hook cancelled: %w", parentCtx.Err())
		return
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		out.TimedOut = true
		out.Err = fmt.Errorf("hook timed out after %s", timeout)
		return
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		out.ExitCode = exitErr.ExitCode()
		out.HasExitCode = true
		out.Err = fmt.Errorf("hook exited %d", out.ExitCode)
		return
	}
	// Spawn failure: program not found, not executable, etc. No exit code.
	out.Err = fmt.Errorf("hook failed to run: %w", runErr)
}

// env renders the SQUIRREL_* contract for one invocation. SQUIRREL_RUN_ID
// is empty for interval hooks (RunID == 0) so the consumer can tell "fired
// by a run" from "fired by the clock" without inspecting the trigger.
func (s Spec) env() []string {
	runID := ""
	if s.RunID != 0 {
		runID = strconv.FormatInt(s.RunID, 10)
	}
	return []string{
		EnvVolume + "=" + s.Volume,
		EnvPath + "=" + s.Path,
		EnvRunID + "=" + runID,
		EnvChanged + "=" + strconv.FormatBool(s.Changed),
		EnvTrigger + "=" + string(s.Trigger),
	}
}

// boundedBuffer is an io.Writer that keeps at most limit bytes, dropping
// the overflow. It lets us capture a stderr tail for diagnostics without
// risking unbounded memory from a misbehaving hook.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) <= room {
			b.buf.Write(p)
		} else {
			b.buf.Write(p[:room])
		}
	}
	// Report the full length as written: we are intentionally discarding
	// the overflow, not failing the command's write.
	return len(p), nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }

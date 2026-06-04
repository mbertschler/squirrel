package agent

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/hook"
	"github.com/mbertschler/squirrel/store"
)

// hookRunner owns the lifecycle of per-volume external-tool hooks (#84):
// the don't-stack guard, the spawn/bound/reap goroutine, and the generic
// outcome recording. One per scheduler. All firing is best-effort — every
// failure is logged via the agent logger and never propagated, so a hook
// can neither fail nor block the run that triggered it.
//
// Hooks run in their own goroutine (not on the scheduler tick) so a
// long-running or wedged command can't stall cadence evaluation; the tick
// only ever pays for the synchronous BeginHookRun insert. wait() lets the
// scheduler drain in-flight hooks on shutdown.
type hookRunner struct {
	store  *store.Store
	logger *slog.Logger

	mu      sync.Mutex
	running map[int64]struct{} // volume ids with an in-flight invocation
	wg      sync.WaitGroup
}

func newHookRunner(s *store.Store, logger *slog.Logger) *hookRunner {
	return &hookRunner{
		store:   s,
		logger:  logger,
		running: make(map[int64]struct{}),
	}
}

// fire launches the volume's hook for the given trigger if one is
// configured and no invocation is already in flight for that volume. It
// returns immediately: the command runs in a tracked goroutine bounded by
// the hook's timeout and by ctx (agent shutdown). triggeringRunID is the
// index run that fired an on-change hook (zero for interval hooks);
// changed is the SQUIRREL_CHANGED value to pass.
//
// A nil receiver is a no-op so tests can construct a bare scheduler
// without wiring a runner.
func (h *hookRunner) fire(ctx context.Context, vol *config.Volume, volumeID int64, trigger string, triggeringRunID int64, changed bool) {
	if h == nil || vol.Hook == nil {
		return
	}
	if !h.tryStart(volumeID) {
		// Don't stack: a previous invocation for this volume is still
		// running. The next trigger (or the external tool's own schedule)
		// catches up — skipping is the specified behaviour, not an error.
		h.logger.Info("hook.skipped",
			"volume", vol.Name, "trigger", trigger,
			"reason", "previous invocation still running")
		return
	}
	id, err := h.store.BeginHookRun(ctx, store.HookRunSpec{
		VolumeID:        volumeID,
		Trigger:         trigger,
		TriggeringRunID: triggeringRunID,
		Changed:         changed,
	})
	if err != nil {
		h.logger.Error("hook.error",
			"volume", vol.Name, "trigger", trigger,
			"err", fmt.Sprintf("begin hook run: %v", err))
		h.done(volumeID)
		return
	}
	h.logger.Info("hook.kicked",
		"volume", vol.Name, "trigger", trigger,
		"hook_run_id", id, "run_id", triggeringRunID, "changed", changed)

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer h.done(volumeID)
		h.execute(ctx, vol, id, trigger, triggeringRunID, changed)
	}()
}

// execute runs the command, then records the generic outcome. It runs on
// the hook goroutine; the recording uses a detached context so the outcome
// still lands even when ctx was cancelled by agent shutdown (which is what
// killed the command in the first place).
func (h *hookRunner) execute(ctx context.Context, vol *config.Volume, hookRunID int64, trigger string, triggeringRunID int64, changed bool) {
	outcome := hook.Run(ctx, hook.Spec{
		Command: vol.Hook.Command,
		Volume:  vol.Name,
		Path:    vol.Path,
		RunID:   triggeringRunID,
		Changed: changed,
		Trigger: hook.Trigger(trigger),
		Timeout: vol.Hook.Timeout,
	})
	status, exitCode, errMsg := classifyOutcome(outcome)

	finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.store.FinishHookRun(finishCtx, hookRunID, status, exitCode, errMsg); err != nil {
		h.logger.Error("hook.error",
			"volume", vol.Name, "trigger", trigger, "hook_run_id", hookRunID,
			"err", fmt.Sprintf("finish hook run: %v", err))
	}
	h.logFinished(vol.Name, trigger, hookRunID, outcome, status)
}

// logFinished emits the terminal hook log line. Failures additionally log
// at error level with the diagnostic so an operator tailing the agent sees
// them without scanning the hook_runs table.
func (h *hookRunner) logFinished(volume, trigger string, hookRunID int64, outcome hook.Outcome, status string) {
	duration := time.Duration(outcome.EndedAtNs - outcome.StartedAtNs)
	attrs := []any{
		"volume", volume, "trigger", trigger, "hook_run_id", hookRunID,
		"status", status, "duration_ms", duration.Milliseconds(),
	}
	if outcome.HasExitCode {
		attrs = append(attrs, "exit_code", outcome.ExitCode)
	}
	if outcome.TimedOut {
		attrs = append(attrs, "timed_out", true)
	}
	h.logger.Info("hook.finished", attrs...)
	if !outcome.Succeeded() {
		h.logger.Error("hook.error",
			"volume", volume, "trigger", trigger, "hook_run_id", hookRunID,
			"err", outcome.Err.Error())
	}
}

// classifyOutcome maps a hook.Outcome onto the store's generic columns. A
// process that produced an exit code records it (even on failure); a
// timeout or spawn failure leaves exit_code NULL. The error message folds
// in a stderr tail so the recorded row explains the failure on its own.
func classifyOutcome(outcome hook.Outcome) (status string, exitCode sql.NullInt64, errMsg string) {
	if outcome.HasExitCode {
		exitCode = sql.NullInt64{Int64: int64(outcome.ExitCode), Valid: true}
	}
	if outcome.Succeeded() {
		return store.HookStatusSuccess, exitCode, ""
	}
	msg := outcome.Err.Error()
	if tail := strings.TrimSpace(outcome.Stderr); tail != "" {
		msg = msg + ": " + tail
	}
	return store.HookStatusFailed, exitCode, msg
}

func (h *hookRunner) tryStart(volumeID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.running[volumeID]; ok {
		return false
	}
	h.running[volumeID] = struct{}{}
	return true
}

func (h *hookRunner) done(volumeID int64) {
	h.mu.Lock()
	delete(h.running, volumeID)
	h.mu.Unlock()
}

// wait blocks until every in-flight hook goroutine has finished. The
// scheduler calls it on shutdown; because hooks are timeout-bounded and
// ctx cancellation kills the command, it returns promptly.
func (h *hookRunner) wait() {
	if h == nil {
		return
	}
	h.wg.Wait()
}

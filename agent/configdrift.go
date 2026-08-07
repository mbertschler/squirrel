package agent

import (
	"bytes"
	"context"
	"log/slog"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// DefaultConfigCheckInterval is how often the agent re-hashes its config
// file to notice that the operator edited it (#191, F9). A minute is far
// inside any realistic "edit the file, mean to restart, get distracted"
// window and costs one small-file read and hash per tick, so the cadence
// needs no user-visible knob — an operator has nothing to gain by tuning
// it, and Config.ConfigCheckEvery already lets a test pin it.
const DefaultConfigCheckInterval = time.Minute

// configMonitor answers one question on a cadence: is the config file on
// disk still the file this agent is running? It compares the BLAKE3 of the
// file's bytes against the digest config.Load computed for the bytes it
// parsed at startup, and on a difference raises the standing config-drift
// latch every squirrel surface renders (ux-principle 4's latch shape, the
// one `verify` alarms and contested paths already use).
//
// Content, not mtime: a rewrite producing identical bytes — a `touch`, an
// editor saving an unmodified buffer, a configuration-management tool
// re-rendering the same template — leaves the digest unchanged and raises
// nothing.
//
// Detection only. The agent never swaps its live config: re-arming
// cadences under in-flight runs is a separate, far more delicate change,
// and a reload that can wedge the agent would be worse than the restart it
// replaces. The latch says "restart to apply" and means it.
type configMonitor struct {
	store  *store.Store
	logger *slog.Logger
	// path is the config file to watch and loaded the digest of the bytes
	// this process parsed from it.
	path   string
	loaded []byte
	every  time.Duration
	// latched mirrors the latch this monitor raised, so a steady state (no
	// drift, nothing latched) touches the database not at all and a
	// standing drift is not re-raised every tick. It starts false because
	// run() clears any latch left behind by a previous process before its
	// first tick. A failed raise leaves it false so the next tick retries.
	latched bool
}

// newConfigMonitor builds the monitor for a server, or returns nil when the
// server was not told which config file it came from (an embedder or a test
// constructing agent.Config by hand). A digest of the wrong length is
// treated the same way — there is nothing meaningful to compare against.
func newConfigMonitor(srv *Server) *configMonitor {
	if srv.cfg.ConfigPath == "" || len(srv.cfg.ConfigDigest) != config.DigestLen {
		return nil
	}
	every := srv.cfg.ConfigCheckEvery
	if every <= 0 {
		every = DefaultConfigCheckInterval
	}
	return &configMonitor{
		store:  srv.store,
		logger: srv.cfg.Logger,
		path:   srv.cfg.ConfigPath,
		loaded: srv.cfg.ConfigDigest,
		every:  every,
	}
}

// run clears any latch a previous agent process left standing — this one
// has just loaded the file on disk, so the restart the latch asked for has
// happened — and then re-checks on the cadence until ctx is cancelled. If
// the file changed again between load and startup, the first tick raises a
// fresh latch for that newer edit.
func (m *configMonitor) run(ctx context.Context) {
	m.clear(ctx, store.ConfigDriftClearedByRestart)
	t := time.NewTicker(m.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.check(ctx)
		}
	}
}

// check performs one comparison. A read failure answers neither "changed"
// nor "unchanged" — an editor writing via a rename makes the file briefly
// absent, and latching on that would raise a drift episode for an edit that
// never happened — so it is logged and skipped. The next tick, reading a
// settled file, decides.
func (m *configMonitor) check(ctx context.Context) {
	disk, err := config.FileDigest(m.path)
	if err != nil {
		m.logger.Warn("config drift check failed",
			"config", m.path, "err", err.Error())
		return
	}
	if bytes.Equal(disk, m.loaded) {
		if m.latched {
			m.clear(ctx, store.ConfigDriftClearedByRevert)
		}
		return
	}
	m.raise(ctx, disk)
}

// raise latches the drift once per episode. The store's insert is
// idempotent on its own; the in-memory flag keeps the steady standing state
// from issuing a write transaction every tick.
func (m *configMonitor) raise(ctx context.Context, disk []byte) {
	if m.latched {
		return
	}
	raised, err := m.store.RaiseConfigDrift(ctx, m.path, m.loaded, disk)
	if err != nil {
		m.logger.Error("config drift raise failed",
			"config", m.path, "err", err.Error())
		return
	}
	m.latched = true
	if raised {
		m.logger.Warn(store.ConfigDriftMessage, "config", m.path)
	}
}

// clear drops a standing latch. The store call is safe when nothing is
// latched (it reports cleared=false without writing), which is what the
// startup call relies on; the tick path takes the in-memory shortcut and
// does not call it at all in the healthy steady state.
func (m *configMonitor) clear(ctx context.Context, reason string) {
	cleared, err := m.store.ClearConfigDrift(ctx, reason)
	if err != nil {
		m.logger.Error("config drift clear failed",
			"config", m.path, "err", err.Error())
		return
	}
	m.latched = false
	if cleared {
		m.logger.Info("config drift cleared", "config", m.path, "reason", reason)
	}
}

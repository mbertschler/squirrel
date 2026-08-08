package agent

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
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

// configMonitor answers one question on a cadence — is the config file on
// disk still the file this agent is running? — and, when it can, does
// something about the answer. It compares the BLAKE3 of the file's bytes
// against the digest config.Load computed for the bytes the agent is
// running; on a difference it reloads what it can and latches what it
// cannot, so every squirrel surface renders the remainder (ux-principle 4's
// latch shape, the one `verify` alarms and contested paths already use).
//
// Content, not mtime: a rewrite producing identical bytes — a `touch`, an
// editor saving an unmodified buffer, a configuration-management tool
// re-rendering the same template — leaves the digest unchanged and does
// nothing at all.
//
// The reload is automatic and needs no trigger, because a routine action
// that must be typed by hand is a design bug (ux-principle 1) and a
// `squirrel agent reload` command would be exactly the third kind of
// command principle 2 rules out — neither a change nor a question, but a
// chore the agent should have done. What it will not do is guess: a file
// that no longer loads, or one whose derived state cannot be rebuilt,
// leaves the running agent untouched and raises a latch saying why. The
// agent keeps serving the last configuration it knows works.
//
// A monitor built without a reloader (an embedder, a test) keeps the
// original detect-and-surface behaviour: the latch says "restart to apply"
// and means it.
type configMonitor struct {
	store  *store.Store
	logger *slog.Logger
	// path is the config file to watch and loaded the digest of the bytes
	// the agent is currently running — advanced by every applied reload, so
	// the comparison always asks "is what I am running still what is on
	// disk", never "is it still what I started with".
	path   string
	loaded []byte
	every  time.Duration
	// reload applies an edit in place. Nil disables reloading entirely.
	reload *reloader
	// standing mirrors the latch this monitor last wrote, so a steady state
	// touches the database not at all and an unchanged finding is not
	// re-written every tick. It starts nil because run() clears any latch
	// left behind by a previous process before its first tick, and a failed
	// write leaves it nil so the next tick retries.
	standing *store.ConfigDriftState
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
	m := &configMonitor{
		store:  srv.store,
		logger: srv.cfg.Logger,
		path:   srv.cfg.ConfigPath,
		loaded: srv.cfg.ConfigDigest,
		every:  every,
	}
	if srv.reloadable() {
		m.reload = &reloader{
			live:    srv.cfg.Live,
			booted:  srv.cfg.Live.Get(),
			prepare: srv.cfg.ConfigReloadPrepare,
		}
	}
	return m
}

// run clears any latch a previous agent process left standing — this one
// has just loaded the file on disk, so the restart the latch asked for has
// happened — and then re-checks on the cadence until ctx is cancelled. If
// the file changed again between load and startup, the first tick reloads
// or latches for that newer edit.
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
// absent, and acting on that would reload or latch for an edit that never
// happened — so it is logged and skipped. The next tick, reading a settled
// file, decides.
func (m *configMonitor) check(ctx context.Context) {
	disk, err := config.FileDigest(m.path)
	if err != nil {
		m.logger.Warn("config drift check failed",
			"config", m.path, "err", err.Error())
		return
	}
	if bytes.Equal(disk, m.loaded) {
		// The file is the one the agent is running, so any latch about it
		// being ahead is resolved — unless the latch is a pending restart,
		// which by construction stands *while* the agent runs the file on
		// disk. That one is resolved by a restart, or by a later edit
		// putting the process-shaped keys back, and both are decided
		// elsewhere.
		if m.standing != nil && len(m.standing.PendingKeys) == 0 {
			m.clear(ctx, store.ConfigDriftClearedByRevert)
		}
		return
	}
	if m.reload == nil {
		m.raise(ctx, store.ConfigDriftState{Path: m.path, Loaded: m.loaded, Disk: disk})
		return
	}
	m.applyOnDisk(ctx, disk)
}

// applyOnDisk adopts the edit on disk, or explains why it could not. The
// three outcomes are the three things the operator needs told apart: the
// edit is live, part of it waits for a restart, or none of it took and the
// agent is still running what it had.
func (m *configMonitor) applyOnDisk(ctx context.Context, disk []byte) {
	res, err := m.reload.apply(ctx, m.path)
	if err != nil {
		m.logger.Error("config reload refused", "config", m.path, "err", err.Error())
		m.raise(ctx, store.ConfigDriftState{
			Path: m.path, Loaded: m.loaded, Disk: disk, ApplyError: err.Error(),
		})
		return
	}
	// From here the agent is running the file on disk, whatever else is
	// still owed — so the comparison baseline moves with it. The digest is
	// the one Load computed for the bytes it actually parsed, not the one
	// this tick hashed, in case the file changed between the two reads.
	m.loaded = res.cfg.Digest
	m.record(ctx, res)
	if len(res.pending) == 0 {
		m.logger.Info("config reloaded", "config", m.path, "applied", res.applied)
		m.clear(ctx, store.ConfigDriftClearedByReload)
		return
	}
	m.logger.Warn(store.ConfigDriftMessageFor(res.pending, ""),
		"config", m.path, "applied", res.applied, "pending", res.pending)
	m.raise(ctx, store.ConfigDriftState{
		Path: m.path, Loaded: m.loaded, Disk: disk, PendingKeys: res.pending,
	})
}

// record writes the audit run for one applied reload — the agent changed
// its own operating configuration, and automatic work is never invisible
// (ux-principle 5). A reload that resolved to no change at all (a comment,
// a reformat) writes nothing: there is no work to be visible about, and a
// run per cosmetic edit would be noise in a trail that is never pruned.
func (m *configMonitor) record(ctx context.Context, res reloadResult) {
	if len(res.applied) == 0 && len(res.pending) == 0 {
		return
	}
	if _, err := m.store.RecordConfigReload(ctx, m.path, res.applied, res.pending); err != nil {
		m.logger.Error("config reload record failed",
			"config", m.path, "err", err.Error())
	}
}

// raise latches the finding, writing only when it differs from what this
// monitor last wrote. The store call is idempotent per episode on its own;
// the in-memory mirror keeps a standing, unchanged state from issuing a
// write transaction every tick.
func (m *configMonitor) raise(ctx context.Context, want store.ConfigDriftState) {
	if m.standing != nil && sameDriftState(*m.standing, want) {
		return
	}
	raised, err := m.store.RaiseConfigDrift(ctx, want)
	if err != nil {
		m.logger.Error("config drift raise failed",
			"config", m.path, "err", err.Error())
		return
	}
	m.standing = &want
	if raised && want.ApplyError == "" && len(want.PendingKeys) == 0 {
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
	m.standing = nil
	if cleared {
		m.logger.Info("config drift cleared", "config", m.path, "reason", reason)
	}
}

// sameDriftState reports whether two findings say the same thing, so an
// unchanged one is not rewritten on every tick.
func sameDriftState(a, b store.ConfigDriftState) bool {
	return a.Path == b.Path &&
		bytes.Equal(a.Loaded, b.Loaded) &&
		bytes.Equal(a.Disk, b.Disk) &&
		a.ApplyError == b.ApplyError &&
		slices.Equal(a.PendingKeys, b.PendingKeys)
}

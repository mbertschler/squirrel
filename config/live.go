package config

import (
	"sync/atomic"
	"time"
)

// Live is the configuration a long-running process is currently running,
// held so it can be replaced without a restart (F9). Readers take a whole
// *Config at a time; a writer replaces the whole thing at once. That is the
// entire point: half a config must never be observable, so no reader may
// ever hold one field from the old file and another from the new.
//
// The discipline that makes it work is on the reader's side: call Get once
// per logical operation and use that snapshot throughout — a scheduler tick,
// one HTTP handler, one scan pass — rather than calling Get per field. The
// returned *Config is immutable by convention; a writer builds a fresh one
// (config.Load) instead of mutating the one in place.
type Live struct {
	cur atomic.Pointer[Config]
}

// NewLive returns a holder seeded with cfg. A nil cfg seeds an empty
// configuration — no volumes, destinations, or nodes — so Get never returns
// nil and readers need no nil check.
func NewLive(cfg *Config) *Live {
	if cfg == nil {
		cfg = &Config{}
	}
	l := &Live{}
	l.cur.Store(cfg)
	return l
}

// Get returns the configuration in force right now. Never nil.
func (l *Live) Get() *Config { return l.cur.Load() }

// Store replaces the configuration in force. The swap is atomic: every
// reader observes either the whole previous config or the whole new one.
// Callers must have fully loaded and validated cfg first — this is the
// point of no return, and it cannot fail.
func (l *Live) Store(cfg *Config) { l.cur.Store(cfg) }

// AgentVerifyEvery returns the `[agent] verify_every` fleet-wide default, or
// zero when the config declares no `[agent]` block at all. Readers of the
// verify cadence want the number, not the block, and a config without an
// agent block is a perfectly ordinary one (every non-agent command loads
// it), so the nil check belongs here rather than at each call site.
func (c *Config) AgentVerifyEvery() time.Duration {
	if c == nil || c.Agent == nil {
		return 0
	}
	return c.Agent.VerifyEvery
}

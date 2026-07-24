package status

import (
	"fmt"
	"time"
)

// This file holds the neutral text labels every surface renders from a
// Report. They live in the query layer so the CLI grid and the TUI
// dashboard show identical words for identical facts (the "build the
// shared layer once, consume it from both" contract); each surface adds
// only its own colour and framing. The functions return strings and never
// write to any stream, keeping the library free of I/O.

// HumanAge renders a duration in the largest useful whole unit (5s, 3m,
// 2h, 4d), the compact form both the CLI grid and the TUI use for ages.
func HumanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// IndexLabel renders a volume's index freshness or the never-indexed gap
// (friction-log F4 — a configured-but-unindexed volume must say so, not
// render blank).
func IndexLabel(v VolumeStatus) string {
	if !v.Indexed || v.LastIndexAgo == nil {
		return "never indexed"
	}
	return HumanAge(*v.LastIndexAgo) + " ago"
}

// RoleLabel names why a target is in the grid: a sync destination, an
// offload-gating target, or both.
func RoleLabel(t TargetStatus) string {
	switch {
	case t.SyncTarget && t.Required:
		return "sync+offload"
	case t.SyncTarget:
		return "sync"
	case t.Required:
		return "offload"
	default:
		return "—"
	}
}

// LastSyncLabel renders the last successful sync's age, "never" for a
// configured-but-unsynced target, or "relayed" for a target this node
// never pushes to (its evidence arrives via a peer durability pull).
func LastSyncLabel(t TargetStatus) string {
	if !t.SyncTarget {
		return "relayed"
	}
	if t.LastSyncAgo == nil {
		return "never"
	}
	return HumanAge(*t.LastSyncAgo) + " ago"
}

// StateLabel names the coverage state, most-specific first: standing states
// outrank a failed transfer, which outranks freshness.
func StateLabel(t TargetStatus) string {
	switch t.Standing {
	case StandingAlarm:
		return "alarm"
	case StandingRefused:
		return "refused"
	case StandingNeedsBootstrap:
		return "needs-init"
	}
	if t.LastOutcome == "failed" {
		return "failed"
	}
	if !t.SyncTarget {
		return "—"
	}
	if t.LastSyncAgo == nil {
		return "never-synced"
	}
	switch t.SyncLevel {
	case LevelWarn:
		return "late"
	case LevelCritical:
		return "stalled"
	default:
		return "ok"
	}
}

// DurableLabel answers "is my local content durable here": yes/no for a
// tracked target, n/a when this node originates no content in the volume,
// and — when nothing is tracked at all.
func DurableLabel(t TargetStatus) string {
	if t.Durability == nil {
		return "—"
	}
	if !t.Durability.LocalContent {
		return "n/a"
	}
	if t.Durability.Covered {
		return "yes"
	}
	return "no"
}

// MethodLabel renders the verify method behind the covering component.
func MethodLabel(t TargetStatus) string {
	if t.Durability == nil || t.Durability.Method == "" {
		return "—"
	}
	return t.Durability.Method
}

// EvidenceLabel renders the evidence age, and — when the volume sets
// offload_max_evidence_age — the age against that budget, flagging stale
// evidence loudly.
func EvidenceLabel(t TargetStatus) string {
	d := t.Durability
	if d == nil || !d.LocalContent {
		return "—"
	}
	age := "unknown"
	if d.EvidenceAge != nil {
		age = HumanAge(*d.EvidenceAge)
	}
	if d.MaxEvidenceAge > 0 {
		age = fmt.Sprintf("%s / %s", age, HumanAge(d.MaxEvidenceAge))
	}
	if d.Stale {
		age += " STALE"
	}
	return age
}

// OffloadLabel renders the "N offloadable now" decision-support line
// (friction-log F17) or a note that the volume has no offload policy.
func OffloadLabel(o OffloadReadiness) string {
	if !o.Applicable {
		return "offload: no policy"
	}
	return fmt.Sprintf("offloadable now: %s (%d files) of %s present (%d files)",
		HumanBytes(o.OffloadableBytes), o.OffloadableFiles,
		HumanBytes(o.PresentBytes), o.PresentFiles)
}

// TrafficLight maps a level to the green/amber/red word the CLI summary
// line and the surfaces' headers use. Neutral reads as green — no problem
// to report.
func TrafficLight(l Level) string {
	switch l {
	case LevelWarn:
		return "amber"
	case LevelCritical:
		return "red"
	default:
		return "green"
	}
}

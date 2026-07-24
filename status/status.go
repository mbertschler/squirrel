// Package status is squirrel's shared "am I safe?" query layer. It reads
// the local index and config and produces, per configured volume and per
// (volume × target), the coverage and durability facts both the CLI
// `squirrel status` command and the TUI dashboard render — friction-log
// F16 (per-target sync coverage), F17 and F23 (durability and offload
// readiness). It is read-only introspection (ux-principle 2): it opens no
// run, reads no disk bytes, and mutates nothing.
//
// The package returns data and a severity per cell; it does not format for
// any particular surface beyond the neutral HumanBytes helper. Each
// consumer renders the Report its own way while sharing one source of
// truth for the facts and their severities.
package status

import (
	"fmt"
	"time"
)

// Level is the "am I safe?" severity of a cell, a volume, or the whole
// report. It is ordered so the worst of a set is its maximum, and it maps
// directly onto the CLI's scriptable exit code via ExitCode.
type Level int

const (
	// LevelNeutral is an informational state that says nothing about
	// safety: a relayed target this node never pushes to, a durability
	// figure for a volume with no offload policy, content that does not
	// originate locally. It never escalates the worst-of aggregation and
	// contributes exit code 0.
	LevelNeutral Level = iota
	// LevelOK is an affirmatively-healthy state — caught up within cadence,
	// durable and freshly verified. "You may close the laptop."
	LevelOK
	// LevelWarn is amber: not caught up yet, needs a one-time bootstrap,
	// local content not yet durable on a required target, evidence aged
	// past the policy. Recoverable and expected, not a failure.
	LevelWarn
	// LevelCritical is red: a latched alarm, a refused sync that regressed
	// from a prior success, a failed transfer, or a pair far past its
	// cadence. Something is wrong and needs attention.
	LevelCritical
)

// String renders a Level as a short lowercase word for CLI output and
// tests. It is deliberately stable — scripts and golden tests key on it.
func (l Level) String() string {
	switch l {
	case LevelNeutral:
		return "neutral"
	case LevelOK:
		return "ok"
	case LevelWarn:
		return "amber"
	case LevelCritical:
		return "red"
	default:
		return "unknown"
	}
}

// ExitCode maps a Level onto the process exit status `squirrel status`
// returns: 0 for green/neutral, 1 for amber, 2 for red. Neutral and OK
// share 0 because neither reports a problem to a script.
func (l Level) ExitCode() int {
	switch l {
	case LevelWarn:
		return 1
	case LevelCritical:
		return 2
	default:
		return 0
	}
}

// worst returns the higher-severity of two levels.
func worst(a, b Level) Level {
	if b > a {
		return b
	}
	return a
}

// Standing is a per-target standing state carried over from #157: an
// abnormal condition that persists across cadence ticks until resolved,
// rather than a single run's outcome.
type Standing int

const (
	// StandingNone means no standing condition — the target's health is
	// whatever its latest sync freshness says.
	StandingNone Standing = iota
	// StandingNeedsBootstrap is a fresh destination awaiting its one-time
	// `sync --init`: the latest run refused with a bootstrap reason and the
	// pair has never had a successful sync. Amber — an expected human step,
	// not a fault (F10).
	StandingNeedsBootstrap
	// StandingRefused is a preflight safety gate declining after the pair
	// had previously succeeded (a marker gone missing, a layout guard) —
	// the destination broke. Red (F26).
	StandingRefused
	// StandingAlarm is a latched verify mismatch on the destination (F30).
	// Red until an operator clears it.
	StandingAlarm
)

// String renders a Standing as a short word for display and tests.
func (s Standing) String() string {
	switch s {
	case StandingNeedsBootstrap:
		return "needs-init"
	case StandingRefused:
		return "refused"
	case StandingAlarm:
		return "alarm"
	default:
		return "ok"
	}
}

// TargetKind classifies a target name against this node's config: a local
// destination, a peer node, or a relayed target named only in
// offload_requires (evidence for it arrives via a peer durability pull;
// this node has no local config for it).
type TargetKind string

const (
	KindDestination TargetKind = "destination"
	KindNode        TargetKind = "node"
	KindRelayed     TargetKind = "relayed"
)

// HumanBytes renders a byte count in binary units with one decimal place
// above KiB, for the "N GB offloadable now" figure both surfaces show. It
// is the one formatting helper the library exposes, because the two
// consumers must render identical numbers.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Report is the whole snapshot for one node: every configured volume with
// its per-target coverage and durability. Now is the instant every age and
// staleness judgement in the report was taken against, so a consumer
// renders one coherent moment.
type Report struct {
	Now     time.Time
	Volumes []VolumeStatus
}

// Level is the worst level across every volume — the report's single
// "am I safe?" answer and the basis for the CLI exit code.
func (r Report) Level() Level {
	l := LevelNeutral
	for _, v := range r.Volumes {
		l = worst(l, v.Level())
	}
	return l
}

// VolumeStatus is one configured volume's coverage and durability.
type VolumeStatus struct {
	Name string
	Path string
	// Indexed is true when the volume has a row in the local index (it has
	// been indexed at least once). A configured-but-never-indexed volume is
	// Indexed=false with no targets carrying run history — the F4 "a freshly
	// configured machine reports nothing" gap, surfaced as amber rather than
	// a blank screen.
	Indexed bool
	// LastIndexAgo is the age of the most recent successful index run, or
	// nil when the volume has never been indexed.
	LastIndexAgo *time.Duration
	Targets      []TargetStatus
	// Offload is the volume's offload-readiness tally ("N GB offloadable
	// now"). Offload.Applicable is false for a volume with no offload
	// policy.
	Offload OffloadReadiness
}

// Level is the worst level across the volume's targets, escalated to amber
// when the volume has never been indexed.
func (v VolumeStatus) Level() Level {
	l := LevelNeutral
	if !v.Indexed {
		l = LevelWarn
	}
	for _, t := range v.Targets {
		l = worst(l, t.Level())
	}
	return l
}

// TargetStatus is one (volume × target) cell: is this target caught up,
// and is the volume's local content durable on it?
type TargetStatus struct {
	Name string
	Kind TargetKind
	// SyncTarget is true when the target is in the volume's sync_to (this
	// node pushes to it). Required is true when it is in offload_requires
	// (its durability gates offload). A relayed target is Required but not
	// a SyncTarget.
	SyncTarget bool
	Required   bool
	// Cadence is the volume's sync_every; lateness of the last sync is
	// judged relative to it, not a global constant. Zero means no scheduled
	// cadence, so a successful sync of any age reads as OK.
	Cadence time.Duration
	// LastSyncAgo is the age of the most recent successful sync to this
	// target, or nil when this node has never successfully synced it (or
	// never pushes to it — a relayed target).
	LastSyncAgo *time.Duration
	// Standing is the target's standing state (alarm / refused /
	// needs-bootstrap / none).
	Standing Standing
	// AlarmDetail is the latched alarm's detail text when Standing is
	// StandingAlarm, for the surface to show what tripped.
	AlarmDetail string
	// LastOutcome is the status of the pair's most recent terminal sync run
	// (success / partial / failed / refused / aborted), or "" when this
	// node has never run a sync against the target. It lets a surface name
	// a failed transfer precisely, distinct from an aged-out success.
	LastOutcome string
	// SyncLevel is the severity of the coverage dimension alone (freshness
	// and standing state); durability is scored separately in Durability.
	SyncLevel Level
	// Durability is the local-content durability on this target, or nil when
	// durability is not tracked for the pair (a non-required target with no
	// recorded vector).
	Durability *Durability
}

// Level is the worst of the target's coverage and durability dimensions.
func (t TargetStatus) Level() Level {
	l := t.SyncLevel
	if t.Durability != nil {
		l = worst(l, t.Durability.Level)
	}
	return l
}

// Durability is the local-content durability of a volume on one target.
type Durability struct {
	// LocalContent is true when this node originates present content in the
	// volume. When false, "is my local content durable here" is vacuously
	// satisfied and Level is neutral — the hub, whose photos originate on
	// the edges, sees this for its inbound volumes.
	LocalContent bool
	// Covered is true when the target's durability vector covers this
	// node's highest local origin run — every locally-originated present
	// content is recorded durable there.
	Covered bool
	// Method is the verify method behind the covering component (blake3,
	// peer-blake3, kopia-verify, presence+size, size+mtime), or "" when
	// there is no component.
	Method string
	// EvidenceAge is how long ago the covering component was last verified,
	// or nil when unknown (no component, or a component that never carried
	// a verification instant).
	EvidenceAge *time.Duration
	// MaxEvidenceAge is the volume's offload_max_evidence_age; zero means no
	// staleness policy. Stale is true when the policy is set and the
	// evidence is older than it (or its age is unknown).
	MaxEvidenceAge time.Duration
	Stale          bool
	Level          Level
}

// OffloadReadiness is a volume's "what could I offload right now" tally.
type OffloadReadiness struct {
	// Applicable is false when the volume declares no offload policy.
	Applicable       bool
	OffloadableFiles int
	OffloadableBytes int64
	PresentFiles     int
	PresentBytes     int64
}

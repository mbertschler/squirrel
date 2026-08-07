package status

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/offload"
	"github.com/mbertschler/squirrel/store"
)

// lateFactor is how many cadence periods a pair may fall behind before its
// staleness reads red rather than amber. Within one cadence is on-time
// (green); up to lateFactor cadences is late (amber); beyond is a
// destination that has effectively stopped (red).
const lateFactor = 3

// Options tunes Build.
type Options struct {
	// SkipOffloadReadiness omits the per-volume offload-readiness tally.
	// offload.Readiness loads the whole volume index and runs the gate over
	// every present file, so on large volumes it is the report's dominant
	// cost. The TUI dashboard sets it on its one-second tick (and refreshes
	// the tally on a slower cadence into its own cache) so the coverage and
	// durability grid stays responsive; the CLI `squirrel status` leaves it
	// off so the "N offloadable now" figure is always current. When set, a
	// volume's Offload still reports Applicable from its config policy, just
	// with zero counts.
	SkipOffloadReadiness bool
}

// Build produces the status Report for this node with readiness computed —
// the CLI's view and the default. See BuildWithOptions for the tunable
// form the TUI uses.
func Build(ctx context.Context, s *store.Store, cfg *config.Config) (Report, error) {
	return BuildWithOptions(ctx, s, cfg, Options{})
}

// BuildWithOptions produces the status Report for this node: every
// config-declared volume with its per-target coverage and durability. It is
// read-only — no run rows, no disk reads, no mutation — so it is safe to
// call from any introspection surface at any time. cfg is the source of
// truth for which volumes and targets should exist; the store supplies the
// observations.
func BuildWithOptions(ctx context.Context, s *store.Store, cfg *config.Config, opts Options) (Report, error) {
	rep := Report{Now: time.Now()}
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("look up self node: %w", err)
	}
	alarms, err := s.ListDestinationAlarms(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list destination alarms: %w", err)
	}
	alarmByDest := indexAlarms(alarms)
	for _, name := range sortedVolumeNames(cfg) {
		vs, err := buildVolume(ctx, s, cfg, self, alarmByDest, cfg.Volumes[name], rep.Now, opts)
		if err != nil {
			return Report{}, err
		}
		rep.Volumes = append(rep.Volumes, vs)
	}
	return rep, nil
}

// indexAlarms keys the active alarms by destination name for O(1) lookup
// per target. Alarms latch per destination, not per volume, so one entry
// applies to every volume that names the destination.
func indexAlarms(alarms []store.DestinationAlarm) map[string]store.DestinationAlarm {
	out := make(map[string]store.DestinationAlarm, len(alarms))
	for _, a := range alarms {
		out[a.Destination] = a
	}
	return out
}

// sortedVolumeNames returns the config's volume names in deterministic
// order so the report (and the CLI grid) is stable across runs.
func sortedVolumeNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Volumes))
	for name := range cfg.Volumes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// buildVolume assembles one volume's status. A volume with no index row
// (declared but never indexed) still lists its planned targets so the
// grid shows the configured coverage rather than a blank line.
func buildVolume(ctx context.Context, s *store.Store, cfg *config.Config, self store.Node, alarmByDest map[string]store.DestinationAlarm, vol *config.Volume, now time.Time, opts Options) (VolumeStatus, error) {
	vs := VolumeStatus{Name: vol.Name, Path: vol.Path}
	// Applicable reflects the config policy, not whether a tally ran, so a
	// never-indexed or skip-readiness volume still reports "has a policy".
	vs.Offload.Applicable = len(vol.OffloadRequires) > 0
	dbVol, err := s.GetVolumeByName(ctx, vol.Name)
	if store.IsNotFound(err) {
		vs.Targets, err = buildTargets(ctx, s, cfg, self, alarmByDest, vol, 0, false, 0, now)
		return vs, err
	}
	if err != nil {
		return VolumeStatus{}, fmt.Errorf("look up volume %q: %w", vol.Name, err)
	}
	vs.Indexed = true
	vs.LastIndexAgo, err = lastIndexAge(ctx, s, dbVol.ID, now)
	if err != nil {
		return VolumeStatus{}, err
	}
	selfMax, err := selfOriginMax(ctx, s, dbVol.ID, self.ID)
	if err != nil {
		return VolumeStatus{}, err
	}
	vs.Targets, err = buildTargets(ctx, s, cfg, self, alarmByDest, vol, dbVol.ID, true, selfMax, now)
	if err != nil {
		return VolumeStatus{}, err
	}
	if opts.SkipOffloadReadiness {
		return vs, nil
	}
	readiness, err := offload.Readiness(ctx, s, offload.ReadinessOptions{
		VolumeID:       dbVol.ID,
		Require:        vol.OffloadRequires,
		MaxEvidenceAge: vol.OffloadMaxEvidenceAge,
		VerifyCadenced: cfg.VerifyCadencedTargets(vol.OffloadRequires),
	})
	if err != nil {
		return VolumeStatus{}, fmt.Errorf("offload readiness for %q: %w", vol.Name, err)
	}
	vs.Offload = OffloadReadiness(readiness)
	return vs, nil
}

// buildTargets builds a TargetStatus for every target in the union of the
// volume's sync_to and offload_requires, sync targets first in config
// order then relayed offload targets, deduplicated.
func buildTargets(ctx context.Context, s *store.Store, cfg *config.Config, self store.Node, alarmByDest map[string]store.DestinationAlarm, vol *config.Volume, volID int64, indexed bool, selfMax int64, now time.Time) ([]TargetStatus, error) {
	var out []TargetStatus
	for _, name := range orderedTargets(vol) {
		ts, err := buildTarget(ctx, s, cfg, self, alarmByDest, vol, name, volID, indexed, selfMax, now)
		if err != nil {
			return nil, err
		}
		out = append(out, ts)
	}
	return out, nil
}

// orderedTargets returns the union of sync_to and offload_requires,
// sync_to first in declared order, then required targets not already
// listed, deduplicated.
func orderedTargets(vol *config.Volume) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, group := range [][]string{vol.SyncTo, vol.OffloadRequires} {
		for _, name := range group {
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// buildTarget assembles one (volume × target) cell: coverage freshness,
// standing state, and durability.
func buildTarget(ctx context.Context, s *store.Store, cfg *config.Config, self store.Node, alarmByDest map[string]store.DestinationAlarm, vol *config.Volume, name string, volID int64, indexed bool, selfMax int64, now time.Time) (TargetStatus, error) {
	ts := TargetStatus{
		Name:       name,
		Kind:       targetKind(cfg, name),
		SyncTarget: contains(vol.SyncTo, name),
		Required:   contains(vol.OffloadRequires, name),
		Cadence:    vol.SyncEvery,
	}
	var lastTerminal *store.Run
	if indexed {
		lastSync, err := latestSuccessfulSync(ctx, s, volID, name)
		if err != nil {
			return TargetStatus{}, err
		}
		ts.LastSyncAgo = ageOf(lastSync, now)
		if lastTerminal, err = latestTerminalSync(ctx, s, volID, name); err != nil {
			return TargetStatus{}, err
		}
		if lastTerminal != nil {
			ts.LastOutcome = lastTerminal.Status
		}
	}
	if alarm, ok := alarmByDest[name]; ok {
		ts.Standing = StandingAlarm
		ts.StandingDetail = alarm.Detail
	} else {
		ts.Standing = classifyStanding(lastTerminal, ts.LastSyncAgo != nil)
		// Only a target this volume actually syncs to can be blocked by a
		// byte-path: a node named solely in offload_requires receives no
		// bytes from here, so a stale path on it costs nothing and saying
		// so would be noise. A byte-path that does not resolve outranks
		// needs-init — the bootstrap being asked for cannot land either
		// until the mount is back — but not refused, which names a more
		// specific thing that broke.
		if reason, broken := unavailableBytePath(cfg, name); broken && ts.SyncTarget && ts.Standing != StandingRefused {
			ts.Standing = StandingBytePath
			ts.StandingDetail = reason
		}
	}
	ts.SyncLevel = syncLevel(ts, lastTerminal)
	if indexed {
		dur, err := buildDurability(ctx, s, volID, self, selfMax, vol, name, now)
		if err != nil {
			return TargetStatus{}, err
		}
		ts.Durability = dur
	}
	return ts, nil
}

// unavailableBytePath reports whether the named target is a peer node whose
// byte-path is configured but does not currently resolve to a directory
// here, with the short reason for it. Destinations, relayed names, and
// nodes that legitimately carry no byte-path all answer false.
//
// The rules come from config.Node.CheckBytePath — the same reader
// `squirrel config check` uses — so the two surfaces can never disagree
// about what counts as a usable byte-path. It stats the filesystem on every
// build rather than caching, because a network mount comes and goes under a
// running agent and a stale "fine" is the failure being closed (F34).
func unavailableBytePath(cfg *config.Config, name string) (string, bool) {
	node, ok := cfg.Nodes[name]
	if !ok {
		return "", false
	}
	if state, reason := node.CheckBytePath(); state == config.BytePathUnavailable {
		return reason, true
	}
	return "", false
}

// classifyStanding derives the standing state from the pair's latest
// terminal sync run. A refusal that regressed from a prior success is a
// broken destination (red); a bootstrap-style refusal on a pair that has
// never succeeded is a fresh destination awaiting one-time `--init` (amber).
func classifyStanding(lastTerminal *store.Run, hadSuccess bool) Standing {
	if lastTerminal == nil || lastTerminal.Status != store.RunStatusRefused {
		return StandingNone
	}
	if !hadSuccess && isBootstrapRefusal(lastTerminal.Error) {
		return StandingNeedsBootstrap
	}
	return StandingRefused
}

// isBootstrapRefusal reports whether a refused run's error is a first-use
// bootstrap refusal (a missing destination marker or an unconnected kopia
// repository), both of which tell the operator to "re-run with --init".
// The layout guard and the init-over-a-different-volume refusal carry no
// "--init" hint, so they stay classified as plain refusals.
func isBootstrapRefusal(errVal sql.NullString) bool {
	return errVal.Valid && strings.Contains(errVal.String, "--init")
}

// syncLevel scores the coverage dimension: standing states first, then the
// latest terminal outcome (a failed run is red, a partial run at least
// amber — both are more recent than any success, so they must not be
// masked by freshness), then freshness relative to the pair's own cadence.
// A relayed target (not pushed to by this node) has no coverage dimension —
// its safety is entirely its durability — so it reads neutral.
func syncLevel(ts TargetStatus, lastTerminal *store.Run) Level {
	switch ts.Standing {
	case StandingAlarm, StandingRefused:
		return LevelCritical
	case StandingNeedsBootstrap, StandingBytePath:
		return LevelWarn
	}
	if lastTerminal != nil {
		switch lastTerminal.Status {
		case store.RunStatusFailed:
			return LevelCritical
		case store.RunStatusPartial:
			return LevelWarn
		}
	}
	if !ts.SyncTarget {
		return LevelNeutral
	}
	if ts.LastSyncAgo == nil {
		return LevelWarn
	}
	return cadenceLevel(*ts.LastSyncAgo, ts.Cadence)
}

// cadenceLevel colours a successful sync's age against the pair's cadence:
// within cadence is green, up to lateFactor cadences amber, beyond red. A
// zero cadence (no scheduled sync) cannot be late, so any success is green.
func cadenceLevel(age, cadence time.Duration) Level {
	if cadence <= 0 {
		return LevelOK
	}
	switch {
	case age <= cadence:
		return LevelOK
	case age <= lateFactor*cadence:
		return LevelWarn
	default:
		return LevelCritical
	}
}

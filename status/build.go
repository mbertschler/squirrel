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

// Build produces the status Report for this node: every config-declared
// volume with its per-target coverage and durability. It is read-only —
// no run rows, no disk reads, no mutation — so it is safe to call from any
// introspection surface at any time. cfg is the source of truth for which
// volumes and targets should exist; the store supplies the observations.
func Build(ctx context.Context, s *store.Store, cfg *config.Config) (Report, error) {
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
		vs, err := buildVolume(ctx, s, cfg, self, alarmByDest, cfg.Volumes[name], rep.Now)
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
func buildVolume(ctx context.Context, s *store.Store, cfg *config.Config, self store.Node, alarmByDest map[string]store.DestinationAlarm, vol *config.Volume, now time.Time) (VolumeStatus, error) {
	vs := VolumeStatus{Name: vol.Name, Path: vol.Path}
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
	readiness, err := offload.Readiness(ctx, s, offload.ReadinessOptions{
		VolumeID: dbVol.ID, Require: vol.OffloadRequires, MaxEvidenceAge: vol.OffloadMaxEvidenceAge,
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
		ts.AlarmDetail = alarm.Detail
	} else {
		ts.Standing = classifyStanding(lastTerminal, ts.LastSyncAgo != nil)
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

// syncLevel scores the coverage dimension: standing states first, then a
// failed transfer, then freshness relative to the pair's own cadence. A
// relayed target (not pushed to by this node) has no coverage dimension —
// its safety is entirely its durability — so it reads neutral.
func syncLevel(ts TargetStatus, lastTerminal *store.Run) Level {
	switch ts.Standing {
	case StandingAlarm, StandingRefused:
		return LevelCritical
	case StandingNeedsBootstrap:
		return LevelWarn
	}
	if lastTerminal != nil && lastTerminal.Status == store.RunStatusFailed {
		return LevelCritical
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

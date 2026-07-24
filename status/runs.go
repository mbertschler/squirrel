package status

import (
	"context"
	"database/sql"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// latestSuccessfulSync returns the most recent fully-landed sync of the
// pair, or nil when there is none. sql.ErrNoRows is the expected "never
// synced" signal and maps to nil; any other error propagates.
func latestSuccessfulSync(ctx context.Context, s *store.Store, volID int64, name string) (*store.Run, error) {
	run, err := s.LatestSuccessfulSyncRun(ctx, volID, name)
	if store.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// latestTerminalSync returns the pair's most recent terminal sync run of
// any outcome (success, partial, failed, refused), or nil when there is
// none. It is the basis for the standing-state classification: a refused
// or failed latest run outranks an older success.
func latestTerminalSync(ctx context.Context, s *store.Store, volID int64, name string) (*store.Run, error) {
	run, err := s.LatestFinishedRun(ctx, store.RunKindSync, volID, name)
	if store.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// lastIndexAge returns the age of the volume's most recent successful (or
// partial) index run, or nil when it has never been indexed.
func lastIndexAge(ctx context.Context, s *store.Store, volID int64, now time.Time) (*time.Duration, error) {
	run, err := s.LatestSuccessfulIndexRun(ctx, volID)
	if store.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ageOf(&run, now), nil
}

// ageOf returns how long ago a run ended, or nil for a nil run or one that
// never recorded an end. Negative ages (clock skew) clamp to zero so a
// surface never renders "-3s ago".
func ageOf(run *store.Run, now time.Time) *time.Duration {
	if run == nil || !run.EndedAtNs.Valid {
		return nil
	}
	d := now.Sub(time.Unix(0, run.EndedAtNs.Int64))
	if d < 0 {
		d = 0
	}
	return &d
}

// evidenceAge returns how long ago a durability component was last
// verified, or nil when the instant is unknown (NULL verified_at_ns).
// Clamped at zero for clock skew, matching ageOf.
func evidenceAge(verifiedAtNs sql.NullInt64, now time.Time) *time.Duration {
	if !verifiedAtNs.Valid {
		return nil
	}
	d := now.Sub(time.Unix(0, verifiedAtNs.Int64))
	if d < 0 {
		d = 0
	}
	return &d
}

// targetKind classifies a target name against this node's config.
func targetKind(cfg *config.Config, name string) TargetKind {
	if _, ok := cfg.Nodes[name]; ok {
		return KindNode
	}
	if _, ok := cfg.Destinations[name]; ok {
		return KindDestination
	}
	return KindRelayed
}

// contains reports whether name appears in names.
func contains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

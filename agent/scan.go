package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
)

// runScanLoop drives the periodic drift-detection scan described in
// issue #17. One tick walks every configured volume; for each it
// acquires the same per-volume lock the /v1/sync/* handlers use, then
// runs an `audit`-kind index pass against the volume root.
//
// Locking is try-lock on both sides: an audit tick arriving while a
// sync is in flight logs a skip and retries on the next tick; a sync
// /begin arriving mid-audit returns 409 (matching the pre-existing
// sync-vs-sync behaviour) and the initiator retries from its end.
// Audit and sync against the same volume therefore never run
// concurrently.
//
// The loop returns when ctx is cancelled. Tickers tick on the wall
// clock, not on completion, so a long-running audit doesn't drag the
// next tick — the next tick fires on schedule and will skip if the
// previous one hasn't released the lock yet.
func (s *Server) runScanLoop(ctx context.Context, logger io.Writer) {
	ticker := time.NewTicker(s.cfg.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runScanTick(ctx, logger)
		}
	}
}

// runScanTick walks every configured volume once. Errors per volume
// are logged via logger; the tick never returns an error because the
// scheduler keeps going regardless of an individual volume's outcome.
// Volumes are visited in name order so log output is deterministic
// (helpful for tests and journald inspection).
func (s *Server) runScanTick(ctx context.Context, logger io.Writer) {
	names := make([]string, 0, len(s.cfg.Volumes))
	for name := range s.cfg.Volumes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return
		}
		s.scanOneVolume(ctx, logger, name, s.cfg.Volumes[name])
	}
}

// scanOneVolume runs one audit pass on a single volume. The
// volume row is resolved (or created) up front so the per-volume
// lock can key on the same int64 the sync handlers use. A try-lock
// failure (sync in flight) is logged and skipped; release happens
// on every other exit path.
func (s *Server) scanOneVolume(ctx context.Context, logger io.Writer, name string, vol *config.Volume) {
	v, err := s.resolveScanVolume(ctx, name, vol.Path)
	if err != nil {
		fmt.Fprintf(logger, "audit %s: %v\n", name, err)
		return
	}
	if !s.router.acquireVolumeLock(v.ID) {
		fmt.Fprintf(logger, "audit %s: skipped — sync in flight on this volume\n", name)
		return
	}
	defer s.router.releaseVolumeLock(v.ID)

	rep, err := index.Index(ctx, s.store, vol.Path, index.Options{
		Name:    vol.Name,
		Kind:    store.RunKindAudit,
		Shallow: s.cfg.ScanStrategy != ScanStrategyDeep,
	})
	if inFlight, ok := errors.AsType[*index.ErrAlreadyRunning](err); ok {
		fmt.Fprintf(logger, "audit %s: skipped — %s in flight (run=%d)\n",
			name, inFlight.Kind, inFlight.Blocker.ID)
		return
	}
	if err != nil {
		fmt.Fprintf(logger, "audit %s: %v\n", name, err)
		return
	}
	fmt.Fprintf(logger,
		"audit %s: run=%d modified=%d added=%d missing=%d errors=%d\n",
		name, rep.RunID, rep.Modified, rep.Added, rep.Missing, rep.Errors)
}

// resolveScanVolume returns the (already-existing or freshly-created)
// volume row for name+absPath. The scheduler may run before any
// `squirrel index` has materialised the volume, so it must create on
// first contact rather than refuse. A name collision with a different
// path is a configuration drift we surface rather than silently
// proceed against the wrong row.
func (s *Server) resolveScanVolume(ctx context.Context, name, absPath string) (store.Volume, error) {
	v, err := s.store.GetVolumeByName(ctx, name)
	if err == nil {
		if v.Path != absPath {
			return store.Volume{}, fmt.Errorf("volume %q is at %q in the DB but config says %q", name, v.Path, absPath)
		}
		return v, nil
	}
	if !store.IsNotFound(err) {
		return store.Volume{}, fmt.Errorf("lookup volume: %w", err)
	}
	created, err := s.store.CreateVolume(ctx, name, absPath)
	if err != nil {
		return store.Volume{}, fmt.Errorf("create volume row: %w", err)
	}
	return created, nil
}

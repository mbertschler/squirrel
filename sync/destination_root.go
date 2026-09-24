package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// destinationRoot is a native destination's root for one push: opened on
// first use, closed when the push returns, and handed to a layout only
// behind a name guard and bounded by progress.
type destinationRoot struct {
	store     *store.Store
	dest      *config.Destination
	volumeDir string // the volume's directory under the root
	// openTransport opens the destination root; tests wrap it to inject
	// faults.
	openTransport func(context.Context, *config.Destination) (transport, error)
	// stallTimeout bounds every transport call by progress.
	stallTimeout time.Duration

	raw transport // opened on first use, closed by close
}

func newDestinationRoot(s *store.Store, dest *config.Destination, volumeDir string) *destinationRoot {
	return &destinationRoot{store: s, dest: dest, volumeDir: volumeDir, openTransport: openDestinationTransport, stallTimeout: DefaultStallTimeout}
}

// guarded is the destination root behind guard, bounded by progress.
func (h *destinationRoot) guarded(ctx context.Context, guard nameGuard) (transport, error) {
	if h.raw == nil {
		raw, err := h.openTransport(ctx, h.dest)
		if err != nil {
			return nil, err
		}
		h.raw = raw
	}
	return stallTransport{
		transport: guardedTransport{transport: h.raw, guard: guard},
		timeout:   h.stallTimeout,
		stalled:   stalledCounter(h.dest.Name),
	}, nil
}

func (h *destinationRoot) close() {
	if h.raw != nil {
		_ = h.raw.Close()
		h.raw = nil
	}
}

// runFinished reports whether runID has ended, so its staging may be
// removed. A run squirrel cannot look up is left alone.
func (h *destinationRoot) runFinished(runID int64) bool {
	r, err := h.store.GetRun(context.Background(), runID)
	return err == nil && r.Status != store.RunStatusRunning
}

// clearFinishedStaging removes what finished runs left in staging:
// partial files from a crash, and a drifted source's staged copy.
// Anything in staging squirrel did not name, a run directory of a run the
// index does not know included, is reported and left alone.
func (h *destinationRoot) clearFinishedStaging(ctx context.Context, rep *Report, tr transport) error {
	dir := path.Join(h.volumeDir, StagingDirName)
	runs, err := tr.List(ctx, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	for _, e := range runs {
		if e.name == markerStagingBase || isFoldProbe(e.name) {
			continue
		}
		runID, ok := stagingRunID(e)
		if !ok {
			h.warnForeignStaging(rep, path.Join(dir, e.name))
			continue
		}
		r, err := h.store.GetRun(ctx, runID)
		if errors.Is(err, sql.ErrNoRows) {
			h.warnForeignStaging(rep, path.Join(dir, e.name))
			continue
		}
		if err != nil {
			return fmt.Errorf("look up the run of %s: %w", path.Join(dir, e.name), err)
		}
		if r.Status == store.RunStatusRunning {
			continue
		}
		if err := h.clearStagingRun(ctx, rep, tr, path.Join(dir, e.name)); err != nil {
			return err
		}
	}
	return nil
}

func (h *destinationRoot) clearStagingRun(ctx context.Context, rep *Report, tr transport, runDir string) error {
	entries, err := tr.List(ctx, runDir)
	if err != nil {
		return fmt.Errorf("list %s: %w", runDir, err)
	}
	foreign := false
	for _, e := range entries {
		name := path.Join(runDir, e.name)
		if e.kind != kindFile || !isStagingKey(e.name) {
			foreign = true
			h.warnForeignStaging(rep, name)
			continue
		}
		if err := tr.Remove(ctx, name); err != nil {
			return fmt.Errorf("remove finished staging %s: %w", name, err)
		}
	}
	if foreign {
		return nil
	}
	if err := tr.Remove(ctx, runDir); err != nil {
		return fmt.Errorf("remove finished staging %s: %w", runDir, err)
	}
	return nil
}

// stagingRunID reads the run id of a staging run directory, run-<id>.
func stagingRunID(e entry) (int64, bool) {
	idText, ok := strings.CutPrefix(e.name, "run-")
	if !ok || e.kind != kindDir {
		return 0, false
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != idText {
		return 0, false
	}
	return id, true
}

func (h *destinationRoot) warnForeignStaging(rep *Report, name string) {
	rep.Warnings = append(rep.Warnings, fmt.Sprintf("destination %q: %s is in squirrel's staging but squirrel did not write it; it is left alone", h.dest.Name, name))
}

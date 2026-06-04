package handlers

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	gosync "sync"
	"time"

	"github.com/a-h/templ"
	"github.com/mbertschler/squirrel/cmd/squirrel-desktop/app/templates"
	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/runevents"
	"github.com/mbertschler/squirrel/store"
	syncpkg "github.com/mbertschler/squirrel/sync"
)

// Runs serves the /runs and /runs/{id} pages and dispatches index and
// sync runs kicked off from the volumes page. The runs table is the
// single source of truth for run state — this handler just
// orchestrates the goroutine boundary between an HTTP request and a
// long-running indexer or sync call.
type Runs struct {
	Config *config.Config
	Store  *store.Store
	// rcloneMu serialises per-invocation rclone setup (Find +
	// WriteRcloneConfig). The file write is idempotent — content is
	// fully derived from cfg.Destinations — but back-to-back syncs
	// from the UI shouldn't race on partial truncation. The actual
	// sync run is then handed off without the lock so concurrent syncs
	// of distinct (volume, dest) pairs proceed in parallel; same-pair
	// concurrency is already gated by store.BeginSyncRunIfClear inside
	// sync.RunPair.
	rcloneMu gosync.Mutex

	// hub fans Progress events from in-flight index/sync goroutines
	// out to any SSE subscribers. Shared across the lifetime of the
	// handler so a tab opened after the run starts still receives
	// live frames.
	hub *progressHub
}

func NewRuns(c *config.Config, s *store.Store) *Runs {
	return &Runs{Config: c, Store: s, hub: newProgressHub()}
}

func (h *Runs) ServeIndex(w http.ResponseWriter, r *http.Request) {
	runs, err := h.Store.ListRuns(r.Context(), store.ListRunsOpts{Descending: true, Limit: 100})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows := h.runRows(r.Context(), runs)
	page := templates.Layout("Runs", buildNav("/runs"))
	body := templates.RunsPage(rows)
	if err := page.Render(templ.WithChildren(r.Context(), body), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (h *Runs) ServeDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	run, err := h.Store.GetRun(r.Context(), id)
	if err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	row := h.runRow(r.Context(), run)
	errMsg := ""
	if run.Error.Valid {
		errMsg = run.Error.String
	}
	page := templates.Layout(fmt.Sprintf("Run #%d", id), buildNav("/runs"))
	body := templates.RunDetailPage(row, errMsg)
	if err := page.Render(templ.WithChildren(r.Context(), body), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// StartIndex spawns an index run for the named volume on a background
// goroutine and redirects to the new run's detail page. The redirect
// races the goroutine's BeginRun call (the indexer writes its own
// runs row), so we capture the volume's pre-trigger latest run id and
// poll briefly until a new one appears.
//
// This split is deliberate: keeping the indexer self-contained means
// the desktop doesn't owe Run lifecycle promises the CLI doesn't already
// keep, but it does mean we can't hand the runID to index.Index up
// front. A future callback-based API would let the trigger redirect
// without the poll.
func (h *Runs) StartIndex(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	vc, ok := h.Config.Volumes[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	v, err := h.Store.GetVolumeByPath(r.Context(), vc.Path)
	priorMax := int64(0)
	volumeID := int64(0)
	if err == nil {
		volumeID = v.ID
		priorMax = h.latestRunID(r.Context(), v.ID, "index")
	}

	// Background context, not r.Context(): outliving the request is
	// the whole point. The goroutine writes the run row + per-file
	// changes via the existing index package. OnRunID is the bridge
	// that lets us publish progress events under the right key even
	// though the runs row is allocated inside index.Index.
	go h.runIndexGoroutine(vc.Path, name)

	if volumeID == 0 {
		// Volume row will be created by the indexer; we can't query
		// for the new run until that happens. Redirect to the list.
		http.Redirect(w, r, "/runs", http.StatusSeeOther)
		return
	}

	newID := h.waitForNewRun(r.Context(), volumeID, "index", priorMax, 3*time.Second)
	if newID == 0 {
		http.Redirect(w, r, "/runs", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/runs/%d", newID), http.StatusSeeOther)
}

// StartSync spawns a sync run for the named volume against the named
// destination on a background goroutine and redirects to the new
// run's detail page. Mirrors StartIndex's split: the desktop layer
// owns the HTTP-to-goroutine boundary, the sync package owns the
// run-lifecycle row writes. Because sync.RunPair allocates its own
// kind='sync' row, we again capture the pre-trigger latest sync-run
// id for this (volume, destination) pair and poll until a strictly
// newer one appears.
//
// Validation: we resolve {name} against config.Volumes and {dest}
// against that volume's declared sync_to entries. Dispatch between
// bucket and node destinations is left to sync.PairsFor / RunPair —
// the desktop doesn't reach into config.Destinations / config.Nodes
// directly, which keeps the bucket-vs-node policy in one place.
func (h *Runs) StartSync(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	dest := r.PathValue("dest")
	vc, ok := h.Config.Volumes[name]
	if !ok || !volumeDeclaresDestination(vc, dest) {
		http.NotFound(w, r)
		return
	}

	pair, rcl, err := h.resolveSyncTarget(r.Context(), name, dest)
	if err != nil {
		log.Printf("desktop: sync %s → %s: %v", name, dest, err)
		http.Redirect(w, r, "/runs", http.StatusSeeOther)
		return
	}

	// Snapshot the latest sync-run id for this pair before launch so the
	// goroutine's BeginSyncRunIfClear write is detectable by polling.
	v, vErr := h.Store.GetVolumeByPath(r.Context(), vc.Path)
	volumeID := int64(0)
	priorMax := int64(0)
	if vErr == nil {
		volumeID = v.ID
		priorMax = h.latestSyncRunID(r.Context(), v.ID, dest)
	}

	go h.runSyncGoroutine(name, dest, pair, rcl)

	if volumeID == 0 {
		// No volumes row yet means the volume has never been
		// indexed; sync.RunPair will refuse before writing a runs
		// row. The /runs list is the best surface for the user.
		http.Redirect(w, r, "/runs", http.StatusSeeOther)
		return
	}
	newID := h.waitForNewSyncRun(r.Context(), volumeID, dest, priorMax, 3*time.Second)
	if newID == 0 {
		http.Redirect(w, r, "/runs", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/runs/%d", newID), http.StatusSeeOther)
}

// runIndexGoroutine is the body of the background index goroutine.
// Fresh context.Background() so the index outlives the HTTP request;
// hub.Close runs in a defer so subscribers see a clean end-of-stream
// regardless of whether index.Index returned cleanly or panicked. The
// runID-aware closures route Progress events to the hub keyed by the
// allocated runs row.
func (h *Runs) runIndexGoroutine(path, name string) {
	ctx := context.Background()
	var runID int64
	opts := index.Options{
		Name: name,
		OnRunID: func(id int64) {
			runID = id
		},
		Progress: func(p runevents.Progress) {
			if runID != 0 {
				h.hub.Publish(runID, p)
			}
		},
	}
	defer func() {
		if runID != 0 {
			h.hub.Publish(runID, runevents.Progress{Stage: runevents.StageDone})
			h.hub.Close(runID)
		}
	}()
	_, _ = index.Index(ctx, h.Store, path, opts)
}

// resolveSyncTarget converts the validated (name, dest) into the
// concrete sync.Pair plus a configured Rclone, isolating the
// fail-fast surface that should redirect to /runs from the
// validate-and-redirect-to-detail flow in StartSync.
func (h *Runs) resolveSyncTarget(ctx context.Context, name, dest string) (syncpkg.Pair, *syncpkg.Rclone, error) {
	pairs, err := syncpkg.PairsFor(h.Config, name, dest)
	if err != nil {
		return syncpkg.Pair{}, nil, fmt.Errorf("pairs: %w", err)
	}
	rcl, err := h.prepareRclone(ctx)
	if err != nil {
		return syncpkg.Pair{}, nil, fmt.Errorf("prepare rclone: %w", err)
	}
	return pairs[0], rcl, nil
}

// runSyncGoroutine is the body of the background sync goroutine. It
// uses a fresh context.Background() because the sync may outlive the
// HTTP request that kicked it off, and surfaces non-success outcomes
// via the request log — the runs table carries the durable state.
func (h *Runs) runSyncGoroutine(name, dest string, pair syncpkg.Pair, rcl *syncpkg.Rclone) {
	ctx := context.Background()
	var runID int64
	opts := syncpkg.Options{
		OnRunID: func(id int64) {
			runID = id
		},
		Progress: func(p runevents.Progress) {
			if runID != 0 {
				h.hub.Publish(runID, p)
			}
		},
	}
	defer func() {
		if runID != 0 {
			h.hub.Publish(runID, runevents.Progress{Stage: runevents.StageDone})
			h.hub.Close(runID)
		}
	}()
	rep, err := syncpkg.RunPair(ctx, h.Store, rcl, pair, opts)
	switch {
	case err != nil:
		log.Printf("desktop: sync %s → %s: %v", name, dest, err)
	case rep.Status != store.RunStatusSuccess:
		log.Printf("desktop: sync %s → %s finished status=%s run=%d errors=%d",
			name, dest, rep.Status, rep.RunID, rep.RcloneResult.Errors)
	}
}

// prepareRclone locates the rclone binary, verifies the version
// floor, and (re)writes the squirrel-managed rclone.conf next to the
// loaded config. Per-invocation rather than long-lived: Find is a
// PATH lookup, WriteRcloneConfig is an idempotent file rewrite, and
// binding setup to each trigger means a user can install or upgrade
// rclone without restarting the desktop. The mutex prevents two
// concurrent triggers from racing on the config-file truncate.
func (h *Runs) prepareRclone(ctx context.Context) (*syncpkg.Rclone, error) {
	h.rcloneMu.Lock()
	defer h.rcloneMu.Unlock()
	rcl, err := syncpkg.Find()
	if err != nil {
		return nil, err
	}
	// Shallow=false matches CLI defaults — the desktop trigger runs
	// the full integrity-checking sync, not a fast path.
	if err := syncpkg.EnsureMinVersion(ctx, rcl, io.Discard, false); err != nil {
		return nil, err
	}
	confPath := filepath.Join(filepath.Dir(h.Config.Path), "rclone.conf")
	if _, err := rcl.WriteRcloneConfig(confPath, h.Config.Destinations); err != nil {
		return nil, err
	}
	return rcl, nil
}

// volumeDeclaresDestination reports whether dest appears in the
// volume's sync_to list. Exact-match because destinations and nodes
// share one user-visible namespace.
func volumeDeclaresDestination(vc *config.Volume, dest string) bool {
	for _, d := range vc.SyncTo {
		if d == dest {
			return true
		}
	}
	return false
}

// latestSyncRunID returns the highest id of any kind='sync' run for
// the given (volume, destination) pair, or 0 if none. Per-destination
// filtering is necessary because a single volume may have multiple
// sync targets, and triggering one shouldn't redirect to another's
// in-flight run.
func (h *Runs) latestSyncRunID(ctx context.Context, volumeID int64, dest string) int64 {
	runs, err := h.Store.ListRuns(ctx, store.ListRunsOpts{
		VolumeID: &volumeID, Descending: true, Limit: 20,
	})
	if err != nil {
		return 0
	}
	for _, r := range runs {
		if r.Kind == store.RunKindSync && r.Destination.Valid && r.Destination.String == dest {
			return r.ID
		}
	}
	return 0
}

// waitForNewSyncRun is the sync-flavoured sibling of waitForNewRun:
// polls for a (volume, destination)-scoped kind='sync' row with id
// strictly greater than priorMax.
func (h *Runs) waitForNewSyncRun(ctx context.Context, volumeID int64, dest string, priorMax int64, budget time.Duration) int64 {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if id := h.latestSyncRunID(ctx, volumeID, dest); id > priorMax {
			return id
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(100 * time.Millisecond):
		}
	}
	return 0
}

// latestRunID returns the highest id of any (volume, kind) run, or 0
// if none. Used to detect "a new run has appeared since I started
// looking".
func (h *Runs) latestRunID(ctx context.Context, volumeID int64, kind string) int64 {
	runs, err := h.Store.ListRuns(ctx, store.ListRunsOpts{
		VolumeID: &volumeID, Descending: true, Limit: 5,
	})
	if err != nil {
		return 0
	}
	for _, r := range runs {
		if r.Kind == kind {
			return r.ID
		}
	}
	return 0
}

// waitForNewRun polls until a run with id > priorMax appears for the
// given (volume, kind), or the deadline elapses. Returns the new id
// or zero on timeout. The 100ms poll is fine for desktop use; an SSE
// channel from the indexer would be cleaner but is out of scope.
func (h *Runs) waitForNewRun(ctx context.Context, volumeID int64, kind string, priorMax int64, budget time.Duration) int64 {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if id := h.latestRunID(ctx, volumeID, kind); id > priorMax {
			return id
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(100 * time.Millisecond):
		}
	}
	return 0
}

// runRows resolves each run's volume id to a name in one pass over
// the volumes table.
func (h *Runs) runRows(ctx context.Context, runs []store.Run) []templates.RunRow {
	vols, _ := h.Store.ListVolumes(ctx)
	byID := make(map[int64]string, len(vols))
	for _, v := range vols {
		byID[v.ID] = v.Name
	}
	out := make([]templates.RunRow, 0, len(runs))
	for _, r := range runs {
		name := ""
		if r.VolumeID.Valid {
			name = byID[r.VolumeID.Int64]
		}
		out = append(out, templates.RunRow{Run: r, VolumeName: name})
	}
	return out
}

func (h *Runs) runRow(ctx context.Context, r store.Run) templates.RunRow {
	name := ""
	if r.VolumeID.Valid {
		if v, err := h.Store.GetVolumeByID(ctx, r.VolumeID.Int64); err == nil {
			name = v.Name
		}
	}
	return templates.RunRow{Run: r, VolumeName: name}
}

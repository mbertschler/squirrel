package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/a-h/templ"
	"github.com/mbertschler/squirrel/cmd/squirrel-desktop/app/templates"
	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
)

// Runs serves the /runs and /runs/{id} pages and dispatches index runs
// kicked off from the volumes page. The runs table is the single source
// of truth for run state — this handler just orchestrates the goroutine
// boundary between an HTTP request and a long-running indexer call.
type Runs struct {
	Config *config.Config
	Store  *store.Store
}

func NewRuns(c *config.Config, s *store.Store) *Runs {
	return &Runs{Config: c, Store: s}
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
	// changes via the existing index package.
	go func() {
		ctx := context.Background()
		_, _ = index.Index(ctx, h.Store, vc.Path, index.Options{Name: name})
	}()

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

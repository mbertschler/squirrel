package handlers

import (
	"context"
	"net/http"
	"sort"

	"github.com/mbertschler/squirrel/cmd/squirrel-desktop/app/templates"
	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// Menubar renders the compact at-a-glance dashboard used by the macOS
// status-bar form factor. The same route is reachable from --serve so
// the panel can be iterated on in a regular browser without a Wails
// build. Read-only: no run dispatch, no mutation.
type Menubar struct {
	Config *config.Config
	Store  *store.Store
}

func NewMenubar(c *config.Config, s *store.Store) *Menubar { return &Menubar{Config: c, Store: s} }

// Serve renders the full page (layout + body). ServeFrame renders only
// the inner panel so the polling Stimulus controller can swap content
// without remounting.
func (h *Menubar) Serve(w http.ResponseWriter, r *http.Request) {
	body := templates.MenubarPage(h.snapshot(r.Context()))
	if err := body.Render(r.Context(), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ServeFrame returns only the inner refreshable region. Targeted by
// the menubar-refresh Stimulus controller so the wrapping <turbo-frame>
// can be replaced without tearing down the controller.
func (h *Menubar) ServeFrame(w http.ResponseWriter, r *http.Request) {
	body := templates.MenubarFrame(h.snapshot(r.Context()))
	if err := body.Render(r.Context(), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// snapshot reads the current volumes + active runs in one pass. Kept
// small enough to call on every poll: the volumes loop is O(configured)
// and the runs query is capped at 50 rows.
func (h *Menubar) snapshot(ctx context.Context) templates.MenubarSnapshot {
	return templates.MenubarSnapshot{
		Volumes:    h.volumeRows(ctx),
		ActiveRuns: h.activeRuns(ctx),
	}
}

// volumeRows materialises one MenubarVolume per configured volume,
// in name order. Pulls the same aggregates the regular volumes page
// uses (root folder file_count + cumulative_size).
func (h *Menubar) volumeRows(ctx context.Context) []templates.MenubarVolume {
	names := make([]string, 0, len(h.Config.Volumes))
	for k := range h.Config.Volumes {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]templates.MenubarVolume, 0, len(names))
	for _, name := range names {
		vc := h.Config.Volumes[name]
		row := templates.MenubarVolume{Name: name, Path: vc.Path}
		if h.Store != nil {
			h.fillVolumeRow(ctx, vc, &row)
		}
		out = append(out, row)
	}
	return out
}

// fillVolumeRow looks up the volume + its root folder + whether a
// run is currently in flight for it. Failures are non-fatal: a volume
// that has never been indexed shows as zero counts, never-running.
func (h *Menubar) fillVolumeRow(ctx context.Context, vc *config.Volume, row *templates.MenubarVolume) {
	v, err := h.Store.GetVolumeByPath(ctx, vc.Path)
	if err != nil {
		return
	}
	if root, ferr := h.Store.GetFolderByPath(ctx, v.ID, ""); ferr == nil {
		row.Indexed = true
		row.FileCount = root.FileCount
		row.CumulativeSize = root.CumulativeSize
	}
	runs, err := h.Store.ListRuns(ctx, store.ListRunsOpts{
		VolumeID: &v.ID, Descending: true, Limit: 10,
	})
	if err != nil {
		return
	}
	for _, r := range runs {
		if r.Status == store.RunStatusRunning {
			row.Running = true
			break
		}
	}
}

// activeRuns returns the runs currently in flight across every
// volume, most recent first. Filtering in Go (rather than adding a
// status filter to ListRunsOpts) keeps the store API small for a
// query that's only used here; the descending+limited window is
// far larger than any plausible concurrent-run count.
func (h *Menubar) activeRuns(ctx context.Context) []templates.MenubarRun {
	if h.Store == nil {
		return nil
	}
	runs, err := h.Store.ListRuns(ctx, store.ListRunsOpts{Descending: true, Limit: 50})
	if err != nil {
		return nil
	}
	vols, _ := h.Store.ListVolumes(ctx)
	byID := make(map[int64]string, len(vols))
	for _, v := range vols {
		byID[v.ID] = v.Name
	}
	out := make([]templates.MenubarRun, 0)
	for _, r := range runs {
		if r.Status != store.RunStatusRunning {
			continue
		}
		mr := templates.MenubarRun{
			ID:          r.ID,
			Kind:        r.Kind,
			StartedAtNs: r.StartedAtNs,
		}
		if r.VolumeID.Valid {
			mr.Volume = byID[r.VolumeID.Int64]
		}
		if r.Destination.Valid {
			mr.Destination = r.Destination.String
		}
		out = append(out, mr)
	}
	return out
}

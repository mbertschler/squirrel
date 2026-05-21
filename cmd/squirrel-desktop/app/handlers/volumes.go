// Package handlers wires the squirrel-desktop HTTP endpoints to the
// existing config and store layer. Each handler is a thin adapter that
// resolves request input, reads the model, and renders a templ
// component to the response.
package handlers

import (
	"context"
	"net/http"
	"sort"

	"github.com/a-h/templ"
	"github.com/mbertschler/squirrel/cmd/squirrel-desktop/app/templates"
	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// Volumes renders the home page: the configured volumes (with each
// volume's index aggregates pulled from the folders table root row)
// alongside the configured destinations. Read-only.
type Volumes struct {
	Config *config.Config
	Store  *store.Store
}

func NewVolumes(c *config.Config, s *store.Store) *Volumes { return &Volumes{Config: c, Store: s} }

func (h *Volumes) ServeIndex(w http.ResponseWriter, r *http.Request) {
	rows := h.rows(r.Context())
	nav := buildNav("/volumes")
	page := templates.Layout("Volumes", nav)
	body := templates.VolumesPage(rows, h.Config.Destinations)
	if err := page.Render(templ.WithChildren(r.Context(), body), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// rows materialises one VolumeRow per configured volume. Aggregates
// come from the volume's root folder; a volume that has never been
// indexed (no volumes row, or volumes row but no root folder) reports
// Indexed = false and zero counts. For indexed volumes we additionally
// look up any in-flight 'running' index/sync runs so the page can
// render disabled "Indexing…" / "Syncing → ⟨dest⟩…" affordances —
// non-indexed volumes can't have running runs against a non-existent
// volume_id, so we skip the query.
func (h *Volumes) rows(ctx context.Context) []templates.VolumeRow {
	names := make([]string, 0, len(h.Config.Volumes))
	for k := range h.Config.Volumes {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]templates.VolumeRow, 0, len(names))
	for _, name := range names {
		vc := h.Config.Volumes[name]
		row := templates.VolumeRow{
			Name:   name,
			Path:   vc.Path,
			SyncTo: vc.SyncTo,
		}
		if h.Store != nil {
			if v, err := h.Store.GetVolumeByPath(ctx, vc.Path); err == nil {
				if root, ferr := h.Store.GetFolderByPath(ctx, v.ID, ""); ferr == nil {
					row.Indexed = true
					row.FileCount = root.FileCount
					row.CumulativeSize = root.CumulativeSize
				}
				h.fillRunning(ctx, &row, v.ID)
			}
		}
		out = append(out, row)
	}
	return out
}

// fillRunning populates RunningIndexRunID and RunningSyncRunIDs from
// the runs table. One ListRuns call per volume (descending, capped at
// runningRunsScanLimit) is enough: there's at most one 'running' row
// per (kind, volume, destination) tuple in normal operation, and a
// stale-row backlog would still surface within the cap. Errors are
// swallowed — the action stays safe (store-side guards still refuse
// duplicates); the UI just falls back to enabled buttons.
func (h *Volumes) fillRunning(ctx context.Context, row *templates.VolumeRow, volumeID int64) {
	runs, err := h.Store.ListRuns(ctx, store.ListRunsOpts{
		VolumeID: &volumeID, Descending: true, Limit: runningRunsScanLimit,
	})
	if err != nil {
		return
	}
	for _, r := range runs {
		if r.Status != store.RunStatusRunning {
			continue
		}
		switch r.Kind {
		case store.RunKindIndex:
			if row.RunningIndexRunID == 0 {
				row.RunningIndexRunID = r.ID
			}
		case store.RunKindSync:
			if !r.Destination.Valid {
				continue
			}
			dest := r.Destination.String
			if row.RunningSyncRunIDs == nil {
				row.RunningSyncRunIDs = make(map[string]int64, len(row.SyncTo))
			}
			if _, seen := row.RunningSyncRunIDs[dest]; !seen {
				row.RunningSyncRunIDs[dest] = r.ID
			}
		}
	}
}

// runningRunsScanLimit caps the per-volume runs scan used to derive
// the running-state map. A handful of in-flight runs is the realistic
// upper bound; the cap guards against a pathological backlog of stale
// 'running' rows dominating the listing render.
const runningRunsScanLimit = 20

// buildNav returns the shared sidebar entries with the currently
// active path marked. Kept in handlers (rather than templates) because
// the set is policy, not presentation, and may grow as features land.
func buildNav(active string) []templates.NavItem {
	items := []templates.NavItem{
		{Label: "Volumes", Href: "/volumes"},
		{Label: "Query", Href: "/query"},
		{Label: "Runs", Href: "/runs"},
	}
	for i := range items {
		if items[i].Href == active {
			items[i].Active = true
		}
	}
	return items
}

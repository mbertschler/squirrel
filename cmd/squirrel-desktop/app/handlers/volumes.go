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
// Indexed = false and zero counts.
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
			}
		}
		out = append(out, row)
	}
	return out
}

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

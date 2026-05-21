package handlers

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/a-h/templ"
	"github.com/mbertschler/squirrel/cmd/squirrel-desktop/app/templates"
	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// Browse renders the ncdu-style directory listing for one volume. The
// current location within the volume is the `?path=` query parameter;
// "" means the volume root. Aggregates per child come from the
// folders.file_count / folders.cumulative_size columns the v9
// migration added.
type Browse struct {
	Config *config.Config
	Store  *store.Store
}

func NewBrowse(c *config.Config, s *store.Store) *Browse { return &Browse{Config: c, Store: s} }

func (h *Browse) Serve(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	vc, ok := h.Config.Volumes[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	current := strings.Trim(r.URL.Query().Get("path"), "/")

	v, err := h.Store.GetVolumeByPath(r.Context(), vc.Path)
	if err != nil {
		if store.IsNotFound(err) {
			renderNotIndexed(w, r, name, current)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	folder, err := h.Store.GetFolderByPath(r.Context(), v.ID, current)
	if err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	entries, err := h.collectEntries(r.Context(), name, folder)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	crumbs := buildCrumbs(name, current)
	upHref := upHref(name, current)
	page := templates.Layout("Browse · "+name, buildNav("/volumes"))
	body := templates.BrowsePage(name, current, crumbs, entries, folder.CumulativeSize, upHref)
	if err := page.Render(templ.WithChildren(r.Context(), body), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// collectEntries merges direct subfolders (read with their aggregates)
// and direct present files into a single descending-by-size list. ncdu
// shows largest first; we copy that default. Folders and files are
// interleaved by size — a 10 GB file ranks above a 2 GB subfolder, which
// matches the "where is my disk going" use case.
func (h *Browse) collectEntries(ctx context.Context, volumeName string, folder store.Folder) ([]templates.BrowseEntry, error) {
	children, err := h.Store.ListChildFolders(ctx, folder.ID)
	if err != nil {
		return nil, err
	}
	files, err := h.Store.ListPresentFilesInFolder(ctx, folder.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	entries := make([]templates.BrowseEntry, 0, len(children)+len(files))
	for _, c := range children {
		entries = append(entries, templates.BrowseEntry{
			Name:  c.Name(),
			Dir:   true,
			Size:  c.CumulativeSize,
			Count: c.FileCount,
			Href:  folderHref(volumeName, c.Path),
		})
	}
	for _, f := range files {
		entries = append(entries, templates.BrowseEntry{
			Name: fileName(f.Path),
			Dir:  false,
			Size: f.SizeBytes,
			Hash: hex.EncodeToString(f.Blake3),
			// File detail page is not implemented yet; "#" keeps the
			// anchor focusable for keyboard nav without navigating
			// the Turbo frame back to the current listing.
			Href: "#",
		})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Size > entries[j].Size })
	return entries, nil
}

func folderHref(volumeName, path string) string {
	u := url.URL{Path: "/volumes/" + volumeName + "/browse"}
	if path != "" {
		q := u.Query()
		q.Set("path", path)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func upHref(volumeName, current string) string {
	if current == "" {
		return ""
	}
	parent := ""
	if i := strings.LastIndexByte(current, '/'); i >= 0 {
		parent = current[:i]
	}
	return folderHref(volumeName, parent)
}

func buildCrumbs(volumeName, current string) []templates.NavItem {
	crumbs := []templates.NavItem{{Label: volumeName, Href: folderHref(volumeName, "")}}
	if current == "" {
		crumbs[0].Active = true
		return crumbs
	}
	parts := strings.Split(current, "/")
	acc := ""
	for i, p := range parts {
		if i > 0 {
			acc += "/"
		}
		acc += p
		crumbs = append(crumbs, templates.NavItem{
			Label:  p,
			Href:   folderHref(volumeName, acc),
			Active: i == len(parts)-1,
		})
	}
	return crumbs
}

func fileName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// renderNotIndexed is the friendly empty-state shown when the user
// browses a volume that the index has never seen. Avoids a 500/404 in
// the typical "fresh install" flow.
func renderNotIndexed(w http.ResponseWriter, r *http.Request, name, current string) {
	page := templates.Layout("Browse · "+name, buildNav("/volumes"))
	body := templates.BrowsePage(name, current, []templates.NavItem{{Label: name, Active: true}}, nil, 0, "")
	if err := page.Render(templ.WithChildren(r.Context(), body), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

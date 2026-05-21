package handlers

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/a-h/templ"
	"github.com/mbertschler/squirrel/cmd/squirrel-desktop/app/templates"
	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// Query handles the lookup view. The same URL serves an empty form (no
// `q` parameter) and the result page (with one), so users can bookmark
// or share a query.
type Query struct {
	Config *config.Config
	Store  *store.Store
}

func NewQuery(c *config.Config, s *store.Store) *Query { return &Query{Config: c, Store: s} }

func (h *Query) Serve(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	res := templates.QueryResult{Query: q}
	if q != "" {
		h.resolve(r.Context(), q, &res)
	}
	h.render(w, r, res)
}

func (h *Query) ServeDuplicates(w http.ResponseWriter, r *http.Request) {
	files, err := h.Store.ListDuplicates(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.render(w, r, templates.QueryResult{Mode: "duplicates", Files: files})
}

func (h *Query) ServeMissing(w http.ResponseWriter, r *http.Request) {
	files, err := h.Store.ListMissing(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.render(w, r, templates.QueryResult{Mode: "missing", Files: files})
}

// resolve routes the user's free-form input to the right store API.
// A 64-character hex string is treated as a BLAKE3 hash; everything
// else is interpreted as a path (absolute first, then relative against
// each configured volume). Empty results for both branches show up as
// an empty Files slice rather than an error, so the page renders a
// "no matches" empty state instead of a 5xx.
func (h *Query) resolve(ctx context.Context, q string, res *templates.QueryResult) {
	if looksLikeHash(q) {
		res.Mode = "hash"
		digest, err := hex.DecodeString(q)
		if err != nil {
			return
		}
		files, err := h.Store.GetByBlake3(ctx, digest)
		if err == nil {
			res.Files = files
		}
		return
	}
	res.Mode = "path"
	if strings.HasPrefix(q, "/") || strings.HasPrefix(q, "~") {
		fwv, err := h.Store.GetByAbsolutePath(ctx, q)
		if err != nil {
			return
		}
		res.PathVolume = fwv.Volume.Name
		hist, err := h.Store.ListHistoryByPath(ctx, fwv.Volume.ID, fwv.File.Path)
		if err == nil {
			res.History = hist
		}
		return
	}
	// Relative path: try each configured volume in declaration order.
	for name, vc := range h.Config.Volumes {
		v, err := h.Store.GetVolumeByPath(ctx, vc.Path)
		if err != nil {
			continue
		}
		hist, err := h.Store.ListHistoryByPath(ctx, v.ID, q)
		if err == nil && len(hist) > 0 {
			res.PathVolume = name
			res.History = hist
			return
		}
	}
}

func (h *Query) render(w http.ResponseWriter, r *http.Request, res templates.QueryResult) {
	page := templates.Layout("Query", buildNav("/query"))
	body := templates.QueryPage(res)
	if err := page.Render(templ.WithChildren(r.Context(), body), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func looksLikeHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		isDigit := c >= '0' && c <= '9'
		isLowerHex := c >= 'a' && c <= 'f'
		isUpperHex := c >= 'A' && c <= 'F'
		if !isDigit && !isLowerHex && !isUpperHex {
			return false
		}
	}
	return true
}

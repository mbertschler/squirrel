package handlers

import (
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"

	"github.com/a-h/templ"
	"github.com/mbertschler/squirrel/cmd/squirrel-desktop/app/templates"
	"github.com/mbertschler/squirrel/store"
)

// Files serves the per-hash detail view at /files/{hash}. The hash path
// segment is a 64-character lowercase hex BLAKE3 digest; the page lists
// every files row (live and superseded) that has ever held the digest.
type Files struct {
	Store *store.Store
}

func NewFiles(s *store.Store) *Files { return &Files{Store: s} }

func (h *Files) ServeDetail(w http.ResponseWriter, r *http.Request) {
	hashStr := strings.ToLower(r.PathValue("hash"))
	if !looksLikeHash(hashStr) {
		http.NotFound(w, r)
		return
	}
	digest, err := hex.DecodeString(hashStr)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	files, err := h.Store.GetByBlake3(r.Context(), digest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	live, history := partitionByStatus(files)
	detail := templates.FileDetail{
		Hash:    hashStr,
		Live:    live,
		History: history,
		BackTo:  backTo(r),
	}
	page := templates.Layout("File · "+templates.ShortHash(hashStr), buildNav(""))
	body := templates.FileDetailPage(detail)
	if err := page.Render(templ.WithChildren(r.Context(), body), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// partitionByStatus splits a GetByBlake3 result into live (present or
// missing) and superseded buckets. Order within each bucket is preserved
// from the store's ORDER BY (volume name, then path).
func partitionByStatus(files []store.FileWithVolume) (live, history []store.FileWithVolume) {
	for _, f := range files {
		if f.File.Status == store.StatusSuperseded {
			history = append(history, f)
		} else {
			live = append(live, f)
		}
	}
	return live, history
}

// backTo picks a sensible "back" target. Prefer the Referer when it
// points at our own browse view; otherwise fall back to /volumes so the
// user has a top-level escape hatch.
func backTo(r *http.Request) string {
	ref := r.Header.Get("Referer")
	if ref == "" {
		return "/volumes"
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "/volumes"
	}
	if u.Host != "" && u.Host != r.Host {
		return "/volumes"
	}
	if !strings.HasPrefix(u.Path, "/volumes/") && u.Path != "/volumes" && u.Path != "/query" {
		return "/volumes"
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

// Package app builds the http.Handler that backs both the Wails webview
// and the standalone --serve HTTP mode. Handlers are server-rendered
// templ partials; navigation and progressive updates ride on Turbo.
package app

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"

	"github.com/mbertschler/squirrel/cmd/squirrel-desktop/app/handlers"
	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// staticFS is the embedded copy of the built CSS/JS bundle (and any
// vendored fonts/icons). Populated from cmd/squirrel-desktop/app/static
// at compile time via the //go:embed directive on the variable below.
//
//go:embed static/dist/*
var staticFS embed.FS

// Deps bundles the long-lived dependencies the handlers need. Server is
// agnostic to *where* it runs (Wails AssetHandler or net/http on a
// port); the only difference is how the returned handler is mounted.
type Deps struct {
	Config *config.Config
	Store  *store.Store
}

// New builds the application's http.Handler. The handler is safe to
// share across both the Wails AssetHandler (one process, embedded
// webview) and a stand-alone http.Server on a TCP port (--serve mode).
func New(deps Deps) (http.Handler, error) {
	staticSub, err := fs.Sub(staticFS, "static/dist")
	if err != nil {
		return nil, fmt.Errorf("static sub fs: %w", err)
	}
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticSub)))

	vh := handlers.NewVolumes(deps.Config, deps.Store)
	bh := handlers.NewBrowse(deps.Config, deps.Store)
	qh := handlers.NewQuery(deps.Config, deps.Store)
	rh := handlers.NewRuns(deps.Config, deps.Store)

	mux.HandleFunc("GET /{$}", vh.ServeIndex)
	mux.HandleFunc("GET /volumes", vh.ServeIndex)
	mux.HandleFunc("GET /volumes/{name}/browse", bh.Serve)
	mux.HandleFunc("POST /volumes/{name}/index", rh.StartIndex)
	mux.HandleFunc("GET /query", qh.Serve)
	mux.HandleFunc("GET /query/duplicates", qh.ServeDuplicates)
	mux.HandleFunc("GET /query/missing", qh.ServeMissing)
	mux.HandleFunc("GET /runs", rh.ServeIndex)
	mux.HandleFunc("GET /runs/{id}", rh.ServeDetail)

	return logRequests(mux), nil
}

// logRequests writes a single line per request to stdout. Kept here
// rather than as middleware on each route so a future switch to a
// structured logger only changes one site.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("%s %s\n", r.Method, r.URL.RequestURI())
		next.ServeHTTP(w, r)
	})
}

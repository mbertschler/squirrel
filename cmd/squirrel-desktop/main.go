// Command squirrel-desktop is the Wails v3 desktop application for the
// squirrel backup tool. It opens a webview onto a server-rendered
// templ/Turbo/Stimulus UI backed by the same config + store packages
// the squirrel CLI uses.
//
// The same binary doubles as a stand-alone web server via `--serve
// :PORT`. In serve mode no webview is opened; only the http.Handler
// runs, which makes the desktop app trivially portable to a remote
// browser and lets the dev loop iterate without spawning a webview.
//
// Wails v3 (alpha) is pulled in only when the binary is built with
// `-tags wailsdesktop`. Without that tag the binary is serve-only and
// builds on any platform with no native webview dependencies — that
// keeps CI green on plain Linux runners. The desktop entry point
// (runDesktop) lives in main_wailsdesktop.go; a no-deps stub with the
// same signature lives in main_no_wailsdesktop.go.
//
// Build steps:
//
//	templ generate ./...                  # *.templ -> *_templ.go
//	bun install                            # JS deps -> node_modules/
//	bun run --cwd cmd/squirrel-desktop build:css   # Tailwind -> app/static/dist/app.css
//	go run ./cmd/squirrel-desktop/internal/assets/build.go  # esbuild -> app/static/dist/app.js
//	go build -tags wailsdesktop ./cmd/squirrel-desktop  # native window + serve
//	go build ./cmd/squirrel-desktop        # serve-only (no webview deps)
//
//go:generate go run ./internal/assets/build.go
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/mbertschler/squirrel/cmd/squirrel-desktop/app"
	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("squirrel-desktop: %v", err)
	}
}

// desktopOptions captures the flag-driven form factor for the desktop
// window. Defined here rather than in main_wailsdesktop.go so flag
// parsing and dispatch stay free of build tags — runDesktop's serve-only
// stub also accepts it without dragging in webview deps.
type desktopOptions struct {
	// Menubar selects the macOS status-bar attached panel: a small
	// frameless on-top window pinned to the tray icon, accessory
	// activation policy (no dock icon), starting hidden. The regular
	// full window is used when this is false.
	Menubar bool
}

func run() error {
	cfgPath := flag.String("config", "", "TOML config path (default: $SQUIRREL_CONFIG or ~/.squirrel/config.toml)")
	dbPath := flag.String("db", "", "SQLite index db path; overrides the value in config")
	serveAddr := flag.String("serve", "", "Run as a stand-alone web server on the given address (e.g. :8080); when set, no desktop window is opened")
	menubar := flag.Bool("menubar", false, "Use the macOS status-bar attached panel form factor (small frameless window pinned to the tray icon) instead of the regular full window")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	resolved := *dbPath
	if resolved == "" {
		resolved = cfg.DB
	}
	if resolved == "" {
		return fmt.Errorf("no database path: set --db, or `db = ...` in %s", cfg.Path)
	}
	st, err := store.OpenWithOptions(resolved, store.OpenOptions{NodeName: cfg.NodeName})
	if err != nil {
		return fmt.Errorf("open store %q: %w", resolved, err)
	}
	defer st.Close()

	handler, err := app.New(app.Deps{Config: cfg, Store: st})
	if err != nil {
		return fmt.Errorf("build handler: %w", err)
	}

	if *serveAddr != "" {
		return runServe(*serveAddr, handler)
	}
	return runDesktop(handler, desktopOptions{Menubar: *menubar})
}

func runServe(addr string, h http.Handler) error {
	fmt.Fprintf(os.Stderr, "squirrel-desktop: listening on %s\n", addr)
	srv := &http.Server{Addr: addr, Handler: h}
	return srv.ListenAndServe()
}

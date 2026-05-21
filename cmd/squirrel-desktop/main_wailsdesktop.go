//go:build wailsdesktop

package main

import (
	"net/http"
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// runDesktop boots Wails v3. When opts.Menubar is set the app uses the
// macOS status-bar attached panel form factor; otherwise a regular
// full-size window with the existing tray menu. Both variants serve
// the same embedded http.Handler so every route — initial HTML and
// XHRs alike — flows through `app.New`'s mux.
func runDesktop(h http.Handler, opts desktopOptions) error {
	if opts.Menubar {
		return runMenubar(h)
	}
	return runRegularWindow(h)
}

// runRegularWindow is the original form factor: one main window with
// the small tray menu on macOS. Behaviour preserved verbatim so the
// default flow stays exactly as it was before --menubar was added.
func runRegularWindow(h http.Handler) error {
	wapp := application.New(application.Options{
		Name:        "Squirrel",
		Description: "Backup tool for your own NAS + cloud offsite storage.",
		Assets: application.AssetOptions{
			Handler: h,
		},
		Mac: application.MacOptions{
			ActivationPolicy: application.ActivationPolicyRegular,
		},
	})

	win := wapp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "Squirrel",
		Width:  1200,
		Height: 800,
		URL:    "/",
	})

	if runtime.GOOS == "darwin" {
		tray := wapp.SystemTray.New()
		menu := wapp.NewMenu()
		menu.Add("Show Squirrel").OnClick(func(*application.Context) {
			win.Show()
			win.Focus()
		})
		menu.AddSeparator()
		menu.Add("Quit").OnClick(func(*application.Context) { wapp.Quit() })
		tray.SetMenu(menu)
		// Closing the window on macOS should hide it rather than quit;
		// the tray brings it back. Without this hook the standard "x"
		// closes the only window and the app stops accepting input.
		win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
			win.Hide()
			e.Cancel()
		})
	}

	return wapp.Run()
}

// runMenubar boots the macOS status-bar form factor: accessory
// activation policy (no dock icon), a frameless always-on-top window
// hidden on launch, and the systray icon's AttachWindow wiring so a
// click on the icon toggles the panel near the menu bar. Pattern
// mirrors wails/v3/examples/systray-menu/main.go.
func runMenubar(h http.Handler) error {
	wapp := application.New(application.Options{
		Name:        "Squirrel",
		Description: "Backup tool for your own NAS + cloud offsite storage.",
		Assets: application.AssetOptions{
			Handler: h,
		},
		Mac: application.MacOptions{
			ActivationPolicy: application.ActivationPolicyAccessory,
		},
	})

	win := wapp.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:          "Squirrel Menubar",
		Title:         "Squirrel",
		Width:         360,
		Height:        460,
		URL:           "/menubar",
		Frameless:     true,
		AlwaysOnTop:   true,
		Hidden:        true,
		DisableResize: true,
		Windows: application.WindowsWindow{
			HiddenOnTaskbar: true,
		},
	})

	// Closing the panel (e.g. losing focus or hitting Esc) should
	// hide it rather than terminate the app — the tray icon brings
	// it back. Without Cancel(), Wails would dispose the only window
	// and the app would have no UI surface to return to.
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		win.Hide()
		e.Cancel()
	})

	tray := wapp.SystemTray.New()
	// AttachWindow makes the tray icon a toggle for the panel; the
	// 2px offset is the spacing example the Wails docs and the
	// systray-menu sample use to sit the window just below the icon.
	tray.AttachWindow(win).WindowOffset(2)

	return wapp.Run()
}

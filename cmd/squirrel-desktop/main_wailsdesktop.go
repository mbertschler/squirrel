//go:build wailsdesktop

package main

import (
	"net/http"
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// runDesktop boots Wails v3 with one main window and (on macOS) a
// system tray entry that toggles window visibility. The window points
// at the embedded asset handler so every request — including the
// initial HTML page — flows through the same routes as `--serve`.
func runDesktop(h http.Handler) error {
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

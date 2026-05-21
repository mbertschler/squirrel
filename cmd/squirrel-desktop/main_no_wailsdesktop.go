//go:build !wailsdesktop

package main

import (
	"errors"
	"net/http"
)

// runDesktop is the no-deps stub used when the binary is built without
// the wailsdesktop tag. Returning an error here keeps the build
// reproducible on a plain Linux runner without GTK4/WebKit, while
// still making the failure mode obvious if a user forgets the tag.
func runDesktop(_ http.Handler, _ desktopOptions) error {
	return errors.New("desktop mode not built into this binary; rebuild with `go build -tags wailsdesktop`, or run with --serve :PORT for the web mode")
}

package main

import (
	"runtime"
	"strings"
	"testing"
)

// TestVersionCommand asserts `squirrel version` prints the injected build
// metadata. The defaults are the unreleased-build placeholders; a tagged
// release replaces `version` via -ldflags, and this same string is what
// the agent reports over GET /v1/health.
func TestVersionCommand(t *testing.T) {
	out := runCLI(t, "version")

	for _, want := range []string{
		"squirrel " + version,
		"commit:   " + commit,
		"built:    " + date,
		runtime.Version(),
		runtime.GOOS + "/" + runtime.GOARCH,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("version output missing %q\ngot:\n%s", want, out)
		}
	}
}

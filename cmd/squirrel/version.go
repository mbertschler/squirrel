package main

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Build metadata, injected at release time via `-ldflags -X` (see
// .goreleaser.yaml). A plain `go build` / `go install` leaves the
// unreleased-build placeholders untouched, so a source build is
// distinguishable from a tagged release at a glance — and the agent
// reports the same `version` string via GET /v1/health.
var (
	version = "0.0.0-dev"
	commit  = "none"
	date    = "unknown"
)

// newVersionCmd returns `squirrel version`, printing the injected build
// metadata: the semver version, the source commit, the build date, and
// the Go toolchain / target platform the binary was built with.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the squirrel version and build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprint(cmd.OutOrStdout(), versionInfo())
			return nil
		},
	}
}

// versionInfo formats the multi-line build report the version command
// prints. Kept separate from the cobra wiring so it is trivially
// testable and can be reused by any other caller that wants the block.
func versionInfo() string {
	return fmt.Sprintf(
		"squirrel %s\n  commit:   %s\n  built:    %s\n  go:       %s\n  platform: %s/%s\n",
		version, commit, date, runtime.Version(), runtime.GOOS, runtime.GOARCH,
	)
}

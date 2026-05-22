//go:build ignore

// Package main is the asset bundler for squirrel-desktop. It is invoked
// via `go generate ./...` and writes the production JS bundle to
// app/static/dist/app.js. The CSS bundle is built by the bun script
// `tailwindcss` (see ../../package.json) and embedded the same way.
//
// Kept as a Go program (rather than a shell call to esbuild) so the
// repository requires zero JavaScript runtime to compile a fresh
// checkout — bun is needed only for dependency resolution and the
// Tailwind CSS pass. The esbuild Go API resolves imports out of
// node_modules just as the JS CLI does.
package main

import (
	"fmt"
	"os"

	"github.com/evanw/esbuild/pkg/api"
)

func main() {
	result := api.Build(api.BuildOptions{
		EntryPoints:       []string{"app/static/src/app.ts"},
		Bundle:            true,
		Outfile:           "app/static/dist/app.js",
		Format:            api.FormatIIFE,
		Platform:          api.PlatformBrowser,
		Target:            api.ES2022,
		MinifyWhitespace:  true,
		MinifyIdentifiers: true,
		MinifySyntax:      true,
		// Sourcemap is generated but not embedded — production builds
		// only ship app.js. Set to None here so we don't even write the
		// .map file; flip to api.SourceMapLinked locally when debugging.
		Sourcemap: api.SourceMapNone,
		Write:     true,
		LogLevel:  api.LogLevelInfo,
		NodePaths: []string{"node_modules"},
	})
	if len(result.Errors) > 0 {
		for _, e := range result.Errors {
			fmt.Fprintf(os.Stderr, "esbuild: %s: %s\n", e.Location, e.Text)
		}
		os.Exit(1)
	}
}

package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// cliReferencePath is the CLI reference page the cobra tree is pinned to.
// The docs tree sits outside the Go packages, so the path is relative to
// this directory; the test only reads the file, so it stays hermetic.
const cliReferencePath = "../../docs/src/content/docs/reference/cli.md"

// TestDocsCoverEveryCommand is the CLI half of the documentation-drift
// guard (#193). AGENTS.md has always required that a change carries its
// documentation; nothing enforced it, and eight commands once existed with
// no entry on this page. This applies the repo's own answer to an
// unenforced rule — the golden test that fails, like TestSchemaSnapshot —
// to the CLI reference: every command in the tree must have a heading, and
// every flag it declares must be named in that command's section.
func TestDocsCoverEveryCommand(t *testing.T) {
	sections := markdownSections(t)
	for _, cmd := range allCommands(newRootCmd()) {
		path := cmd.CommandPath()
		body, ok := sections[path]
		if !ok {
			t.Errorf("%s: no `%s` heading in %s — document the command", path, path, cliReferencePath)
			continue
		}
		if cmd.Parent() == nil {
			// The root's persistent flags are documented in the page
			// preamble's inherited-flag table, above the first heading.
			body = sections[""] + body
		}
		for _, name := range localFlagNames(cmd) {
			if !strings.Contains(body, "`--"+name+"`") {
				t.Errorf("%s: flag --%s is not documented under %q in %s", path, name, path, cliReferencePath)
			}
		}
	}
}

// TestDocsDescribeOnlyRealCommands is the other direction: a `squirrel …`
// heading with no command behind it means the page kept a command the tree
// renamed or dropped, which reads as a working command to anyone following
// the reference.
func TestDocsDescribeOnlyRealCommands(t *testing.T) {
	real := map[string]bool{}
	for _, cmd := range allCommands(newRootCmd()) {
		real[cmd.CommandPath()] = true
	}
	for heading := range markdownSections(t) {
		if heading != "squirrel" && !strings.HasPrefix(heading, "squirrel ") {
			continue
		}
		if !real[heading] {
			t.Errorf("%s documents %q, which is not a command in the cobra tree", cliReferencePath, heading)
		}
	}
}

// allCommands flattens the cobra tree, root first. Cobra's generated help
// and completion commands are not part of squirrel's documented surface,
// and hidden commands are deliberately not user-facing.
func allCommands(root *cobra.Command) []*cobra.Command {
	out := []*cobra.Command{root}
	for _, sub := range root.Commands() {
		if sub.Hidden || sub.Name() == "help" || sub.Name() == "completion" {
			continue
		}
		out = append(out, allCommands(sub)...)
	}
	return out
}

// localFlagNames returns the long names of the flags a command declares
// itself, sorted. Flags inherited from a parent are excluded — they are
// documented once, on the command that owns them.
func localFlagNames(cmd *cobra.Command) []string {
	var names []string
	cmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		names = append(names, f.Name)
	})
	sort.Strings(names)
	return names
}

var markdownHeadingRe = regexp.MustCompile(`^#{1,6} +(.+?) *$`)

// markdownSections splits a markdown page into heading text → body. A
// body runs to the next heading of any level, so a parent command's
// section ends where its first subcommand's section begins. Text before
// the first heading (frontmatter and the inherited-flag table) is keyed
// with the empty string. Fenced code blocks are skipped so a `#` inside
// an example never reads as a heading.
func markdownSections(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(cliReferencePath)
	if err != nil {
		t.Fatalf("read %s: %v", cliReferencePath, err)
	}
	sections := map[string]string{}
	current := ""
	var body strings.Builder
	fenced := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		if !fenced {
			if m := markdownHeadingRe.FindStringSubmatch(line); m != nil {
				sections[current] += body.String()
				body.Reset()
				current = m[1]
				continue
			}
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	sections[current] += body.String()
	return sections
}

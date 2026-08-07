package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// runsDocPath is the concepts page that explains the audit trail. The docs
// tree sits outside the Go packages, so the path is relative to this
// directory; the test only reads the file, so it stays hermetic.
const runsDocPath = "../docs/src/content/docs/concepts/runs.md"

// TestDocsRunKindsMatchConstants and TestDocsRunStatusesMatchConstants are
// the runs half of the documentation-drift guard (#193). Both compare the
// declared constants against the first column of the matching table, in
// both directions: a kind or status added without a table row fails, and
// so does a row describing something that is not a constant — the shape
// that once put `verify` on the page as a run kind of its own when it is
// recorded as `audit`.
func TestDocsRunKindsMatchConstants(t *testing.T) {
	assertSameSet(t, "run kinds",
		constValuesWithPrefix(t, "RunKind"),
		docTableFirstColumn(t, "Run kinds"))
}

func TestDocsRunStatusesMatchConstants(t *testing.T) {
	assertSameSet(t, "run statuses",
		constValuesWithPrefix(t, "RunStatus"),
		docTableFirstColumn(t, "Run statuses"))
}

// assertSameSet reports the symmetric difference between the constants and
// the documented set, naming which side each entry is missing from.
func assertSameSet(t *testing.T, what string, code, docs []string) {
	t.Helper()
	inDocs := map[string]bool{}
	for _, d := range docs {
		inDocs[d] = true
	}
	inCode := map[string]bool{}
	for _, c := range code {
		inCode[c] = true
		if !inDocs[c] {
			t.Errorf("%s: %q is declared in code but has no row in %s", what, c, runsDocPath)
		}
	}
	for _, d := range docs {
		if !inCode[d] {
			t.Errorf("%s: %s lists %q, which no constant declares", what, runsDocPath, d)
		}
	}
}

// constValuesWithPrefix parses this package's source and returns the string
// values of every exported constant whose name starts with prefix. Reading
// the declarations instead of listing them here is what makes the guard
// hold: a new RunKind is picked up the moment it is declared, without the
// test having to be remembered too.
func constValuesWithPrefix(t *testing.T, prefix string) []string {
	t.Helper()
	var out []string
	for _, file := range parseGoFiles(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range spec.Names {
				if !strings.HasPrefix(name.Name, prefix) || i >= len(spec.Values) {
					continue
				}
				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", name.Name, err)
				}
				out = append(out, v)
			}
			return true
		})
	}
	if len(out) == 0 {
		t.Fatalf("no %s* constants found — the scan is broken, not the docs", prefix)
	}
	sort.Strings(out)
	return out
}

// parseGoFiles parses every non-test .go file of this package.
func parseGoFiles(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	return files
}

// docTableFirstColumn returns the first cell of every body row of the
// first markdown table under the given heading, stripped of backticks.
// Header and separator rows are skipped, and the scan stops at the next
// heading so a later table can't leak in.
func docTableFirstColumn(t *testing.T, heading string) []string {
	t.Helper()
	raw, err := os.ReadFile(runsDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", runsDocPath, err)
	}
	var out []string
	inSection, inTable := false, false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "#") {
			if inTable {
				break
			}
			inSection = strings.TrimLeft(line, "# ") == heading
			continue
		}
		if !inSection || !strings.HasPrefix(line, "|") {
			if inTable {
				break
			}
			continue
		}
		cell := strings.Trim(strings.Split(strings.Trim(line, "|"), "|")[0], " `")
		if cell == "" || strings.HasPrefix(cell, "-") {
			inTable = true // header separator
			continue
		}
		if !inTable {
			continue // header row
		}
		out = append(out, cell)
	}
	if len(out) == 0 {
		t.Fatalf("no table rows found under %q in %s", heading, runsDocPath)
	}
	sort.Strings(out)
	return out
}

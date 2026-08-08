package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// configReferencePath is the reference page every config key must appear
// on. The docs tree sits outside the Go packages, so the path is relative
// to this directory; the test only reads the file, so it stays hermetic.
const configReferencePath = "../docs/src/content/docs/reference/configuration.md"

// TestDocsCoverEveryConfigKey is the config half of the
// documentation-drift guard (#193). The configuration reference is in good
// shape today and nothing keeps it that way: a key added to a raw struct
// or a destination schema is invisible to users until someone remembers
// the page. This asserts every key squirrel accepts is named on it, in the
// same spirit as TestSchemaSnapshot pinning store/schema.sql.
//
// Membership is deliberately loose — the key must appear as (or inside) an
// inline code span anywhere on the page, not in a particular table. The
// page groups keys by context (`[agent]`, `[nodes.<name>]`, per
// destination type) and a stricter rule would dictate its structure rather
// than its completeness.
func TestDocsCoverEveryConfigKey(t *testing.T) {
	documented := codeSpanTokens(t)
	for _, key := range configKeys(t) {
		if !documented[key] {
			t.Errorf("config key %q is accepted by config/ but never named in %s", key, configReferencePath)
		}
	}
}

// configKeys is every key name squirrel's config loader accepts: the toml
// struct tags of the raw decode types, plus the destination keys, which
// are validated out of an untyped map and so carry no tags.
func configKeys(t *testing.T) []string {
	t.Helper()
	keys := map[string]bool{}
	for _, key := range tomlTags(t) {
		keys[key] = true
	}
	for _, key := range destinationKeys() {
		keys[key] = true
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// destinationKeys collects the `[destinations.<name>]` surface: the keys
// every type accepts plus every field of every registered type's schema.
// The crypt sub-table's keys are validated inline in resolveCrypt and are
// listed here alongside them.
func destinationKeys() []string {
	out := append([]string{}, universalDestKeys...)
	out = append(out, "password", "password2", "obscured") // crypt sub-table
	for _, schema := range destSchemas {
		out = append(out, schema.requiredString...)
		out = append(out, schema.optionalString...)
		out = append(out, schema.secretFields...)
		out = append(out, schema.requiredSecret...)
	}
	return out
}

// tomlTags parses every non-test .go file in dir and returns the name in
// each `toml:"…"` struct tag. Reading the tags from source is what makes
// the guard hold without maintenance: a new field on a raw struct is
// picked up the moment it is declared.
func tomlTags(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			field, ok := n.(*ast.Field)
			if !ok || field.Tag == nil {
				return true
			}
			tag, ok := tomlTagName(field.Tag.Value)
			if ok && tag != "" && tag != "-" {
				out = append(out, tag)
			}
			return true
		})
	}
	if len(out) == 0 {
		t.Fatalf("no toml tags found in config/ — the scan is broken, not the docs")
	}
	return out
}

// tomlTagName reads the toml key out of a raw (still backquoted) struct
// tag literal, dropping any `,omitempty`-style options.
func tomlTagName(literal string) (string, bool) {
	unquoted, err := strconv.Unquote(literal)
	if err != nil {
		return "", false
	}
	v, ok := reflect.StructTag(unquoted).Lookup("toml")
	if !ok {
		return "", false
	}
	return strings.Split(v, ",")[0], true
}

var codeSpanRe = regexp.MustCompile("`[^`\n]+`")

// codeSpanTokens returns every TOML-bare-key-shaped token that appears
// inside an inline code span on the page. Splitting spans into tokens is
// what lets a dotted or bracketed mention — `auth.token`, `[agent]
// verify_every` — count for the key it names.
func codeSpanTokens(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(configReferencePath)
	if err != nil {
		t.Fatalf("read %s: %v", configReferencePath, err)
	}
	notBareKey := regexp.MustCompile(`[^A-Za-z0-9_-]+`)
	out := map[string]bool{}
	for _, span := range codeSpanRe.FindAllString(string(raw), -1) {
		for _, token := range notBareKey.Split(strings.Trim(span, "`"), -1) {
			if token != "" {
				out[token] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no inline code spans found in %s — the scan is broken, not the docs", configReferencePath)
	}
	return out
}

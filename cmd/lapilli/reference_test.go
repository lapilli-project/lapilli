package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The reference pages name everything of theirs the code has: every route
// of the JSON API in docs/api.md, every metric name in docs/metrics.md, and
// every schema of the ledger in docs/upgrading.md's table, so a route, a
// metric or a schema added without its line fails here.

func TestAPIDocumented(t *testing.T) {
	page := readDoc(t, filepath.Join("..", "..", "docs", "api.md"))
	routes := codeMatches(t, regexp.MustCompile(`"((?:GET|POST|PUT|PATCH|DELETE) /api/v1/[^"]+)"`), "internal/serve")
	if len(routes) < 20 {
		t.Fatalf("found %d routes in internal/serve: the pattern no longer finds them", len(routes))
	}
	for _, r := range routes {
		if !strings.Contains(page, "`"+r+"`") {
			t.Errorf("docs/api.md doesn't name `%s`", r)
		}
	}
}

func TestMetricsDocumented(t *testing.T) {
	page := readDoc(t, filepath.Join("..", "..", "docs", "metrics.md"))
	// A name in a string, or in the # HELP line of one written out whole.
	names := codeMatches(t, regexp.MustCompile(`\b(lapilli_[a-z_]+)`), "internal", "cmd")
	if len(names) < 20 {
		t.Fatalf("found %d metric names: the pattern no longer finds them", len(names))
	}
	for _, n := range names {
		if !strings.Contains(page, n) {
			t.Errorf("docs/metrics.md doesn't name %s", n)
		}
	}
}

func TestSchemasDocumented(t *testing.T) {
	page := readDoc(t, filepath.Join("..", "..", "docs", "upgrading.md"))
	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "ledger", "schema", "[0-9][0-9][0-9].sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("internal/ledger/schema: %v, %d files", err, len(files))
	}
	for _, f := range files {
		n := strings.TrimSuffix(filepath.Base(f), ".sql")
		if !regexp.MustCompile(`(?m)^\| ` + n + ` \|`).MatchString(page) {
			t.Errorf("docs/upgrading.md has no row for schema %s", n)
		}
	}
}

// codeMatches is the first group of each match of re in the Go files,
// tests left out, under the repository's dirs, each once, sorted.
func codeMatches(t *testing.T, re *regexp.Regexp, dirs ...string) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, d := range dirs {
		err := filepath.WalkDir(filepath.Join("..", "..", d), func(path string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				seen[m[1]] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var out []string
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The reference pages name everything of theirs the code has, and nothing
// it hasn't: every route of the JSON API and of the notifications port in
// docs/api.md, every metric name in docs/metrics.md, and every schema of
// the ledger, SQLite's and Postgres's, in docs/upgrading.md's table. They
// don't hold what a page says of each (a role, a label, a release): a
// route, a metric or a schema added without its line, or a line left after
// it is gone, fails here.

func TestAPIDocumented(t *testing.T) {
	page := readDoc(t, filepath.Join("..", "..", "docs", "api.md"))
	routes := codeMatches(t, regexp.MustCompile(`"((?:GET|POST|PUT|PATCH|DELETE) /(?:api/)?v1/[^"]+)"`), "internal/serve")
	if len(routes) < 20 {
		t.Fatalf("found %d routes in internal/serve: the pattern no longer finds them", len(routes))
	}
	known := map[string]bool{}
	for _, r := range routes {
		known[r] = true
		if !strings.Contains(page, "`"+r+"`") {
			t.Errorf("docs/api.md doesn't name `%s`", r)
		}
	}
	// A route may be named with its query: PUT /api/v1/firings/{source}?from=&to=.
	for _, m := range regexp.MustCompile("`((?:GET|POST|PUT|PATCH|DELETE) /(?:api/)?v1/[^`?]+)[^`]*`").FindAllStringSubmatch(page, -1) {
		if !known[m[1]] {
			t.Errorf("docs/api.md names `%s`, which internal/serve doesn't have", m[1])
		}
	}
}

func TestMetricsDocumented(t *testing.T) {
	page := readDoc(t, filepath.Join("..", "..", "docs", "metrics.md"))
	// A name in a string, or in the # HELP line of one written out whole;
	// one that ends in _ is the start of names made from it.
	names := codeMatches(t, regexp.MustCompile(`\b(lapilli_[a-z_]+)`), "internal", "cmd")
	if len(names) < 20 {
		t.Fatalf("found %d metric names: the pattern no longer finds them", len(names))
	}
	known := map[string]bool{}
	var starts []string
	for _, n := range names {
		known[n] = true
		if strings.HasSuffix(n, "_") {
			starts = append(starts, n)
			if !strings.Contains(page, n) {
				t.Errorf("docs/metrics.md doesn't name a metric starting %s", n)
			}
			continue
		}
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(n) + `\b`).MatchString(page) {
			t.Errorf("docs/metrics.md doesn't name %s", n)
		}
	}
next:
	for _, n := range regexp.MustCompile(`\blapilli_[a-z_]*[a-z]\b`).FindAllString(page, -1) {
		if known[n] {
			continue
		}
		for _, s := range starts {
			if strings.HasPrefix(n, s) {
				continue next
			}
		}
		t.Errorf("docs/metrics.md names %s, which the code doesn't have", n)
	}
}

func TestSchemasDocumented(t *testing.T) {
	page := readDoc(t, filepath.Join("..", "..", "docs", "upgrading.md"))
	schema := filepath.Join("..", "..", "internal", "ledger", "schema")
	files, err := filepath.Glob(filepath.Join(schema, "[0-9][0-9][0-9].sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("internal/ledger/schema: %v, %d files", err, len(files))
	}
	pg, err := filepath.Glob(filepath.Join(schema, "postgres", "[0-9][0-9][0-9].sql"))
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, f := range append(files, pg...) {
		n := strings.TrimSuffix(filepath.Base(f), ".sql")
		if known[n] {
			continue
		}
		known[n] = true
		if !regexp.MustCompile(`(?m)^\| ` + n + ` \|`).MatchString(page) {
			t.Errorf("docs/upgrading.md has no row for schema %s", n)
		}
	}
	for _, m := range regexp.MustCompile(`(?m)^\| ([0-9]{3}) \|`).FindAllStringSubmatch(page, -1) {
		if !known[m[1]] {
			t.Errorf("docs/upgrading.md has a row for schema %s, which internal/ledger/schema doesn't have", m[1])
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

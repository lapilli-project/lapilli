package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// The docs are the README and docs/*.md, a page a subject, which
// site/docs.sh publishes at lapilli.dev/docs: each page is listed in the
// README and in docs/README.md, whose lists are the site's sidebar, and
// every link in them, to a page, a heading or a file of the repository,
// goes somewhere. The recipes' and the chart's READMEs link into the docs,
// so their links are held too, and so is every lapilli.dev/docs/NAME the
// code, the help and the site name.
func TestDocsLinks(t *testing.T) {
	root := filepath.Join("..", "..")
	pages, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil || len(pages) < 2 {
		t.Fatalf("docs/*.md: %v, %d pages", err, len(pages))
	}
	recipes, err := filepath.Glob(filepath.Join(root, "recipes", "*", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	files := append([]string{
		filepath.Join(root, "README.md"),
		filepath.Join(root, "charts", "lapilli", "README.md"),
	}, append(pages, recipes...)...)
	heads := map[string]map[string]bool{}
	anchors := func(path string) map[string]bool {
		if a, ok := heads[path]; ok {
			return a
		}
		a := headingAnchors(readDoc(t, path))
		heads[path] = a
		return a
	}
	for _, f := range files {
		name, _ := filepath.Rel(root, f)
		for _, l := range docLinks(readDoc(t, f)) {
			target, anchor, _ := strings.Cut(l.target, "#")
			path := f
			if target != "" {
				path = filepath.Join(filepath.Dir(f), filepath.FromSlash(target))
				if _, err := os.Stat(path); err != nil {
					t.Errorf("%s:%d: %s goes nowhere", name, l.line, l.target)
					continue
				}
			}
			if anchor != "" && strings.HasSuffix(path, ".md") && !anchors(path)[anchor] {
				t.Errorf("%s:%d: %s: no heading #%s", name, l.line, l.target, anchor)
			}
		}
	}

	_, list, _ := strings.Cut(readDoc(t, filepath.Join(root, "README.md")), "\n## Docs\n")
	list, _, _ = strings.Cut(list, "\n## ")
	home := readDoc(t, filepath.Join(root, "docs", "README.md"))
	for _, p := range pages {
		n := filepath.Base(p)
		if n == "README.md" {
			continue
		}
		if !strings.Contains(list, "](docs/"+n+")") {
			t.Errorf("the README's Docs doesn't list docs/%s", n)
		}
		// An item whose first link is to the page, as site/docs.sh reads it.
		if !regexp.MustCompile(`(?m)^- \[[^\]\n]*\]\(` + regexp.QuoteMeta(n) + `\)`).MatchString(home) {
			t.Errorf("docs/README.md doesn't list %s as an item of its own: site/docs.sh wouldn't publish it", n)
		}
	}

	// The pages' addresses on lapilli.dev, as the help and the site give them.
	site := regexp.MustCompile(`lapilli\.dev/docs/([a-z0-9-]+)|'/docs/([a-z0-9-]+)/'`)
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if name == ".git" || name == filepath.Join("site", "docs") {
				return filepath.SkipDir
			}
			return nil
		}
		if name == "CHANGELOG.md" || !(strings.HasSuffix(name, ".go") || strings.HasPrefix(name, "site"+string(filepath.Separator))) {
			return nil
		}
		for i, line := range strings.Split(readDoc(t, path), "\n") {
			for _, m := range site.FindAllStringSubmatch(line, -1) {
				if _, err := os.Stat(filepath.Join(root, "docs", m[1]+m[2]+".md")); err != nil {
					t.Errorf("%s:%d: %s isn't a page of docs/", name, i+1, m[0])
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readDoc(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type docLink struct {
	line   int
	target string
}

var (
	docFence  = regexp.MustCompile("^[ \t]*(```|~~~)")
	docHref   = regexp.MustCompile(`\]\(([^)\s]+)\)|(?:src|srcset)="([^"]+)"`)
	docScheme = regexp.MustCompile(`^[a-z][a-z0-9+.-]*:`)
)

// docLinks are the links of a Markdown file to somewhere in the
// repository, outside code blocks.
func docLinks(doc string) []docLink {
	var links []docLink
	code := false
	for i, line := range strings.Split(doc, "\n") {
		if docFence.MatchString(line) {
			code = !code
			continue
		}
		if code {
			continue
		}
		for _, m := range docHref.FindAllStringSubmatch(line, -1) {
			target := m[1] + m[2]
			if docScheme.MatchString(target) || strings.HasPrefix(target, "/") {
				continue
			}
			links = append(links, docLink{i + 1, target})
		}
	}
	return links
}

// headingAnchors are the anchors GitHub gives a file's headings: lower
// case, spaces as hyphens, other punctuation left out, a second of the
// same name with -1.
func headingAnchors(doc string) map[string]bool {
	anchors := map[string]bool{}
	seen := map[string]int{}
	code := false
	for _, line := range strings.Split(doc, "\n") {
		if docFence.MatchString(line) {
			code = !code
			continue
		}
		if code || !strings.HasPrefix(line, "#") {
			continue
		}
		text := strings.TrimLeft(line, "#")
		if len(text) == 0 || text[0] != ' ' {
			continue
		}
		var b strings.Builder
		for _, r := range strings.ToLower(strings.TrimSpace(text)) {
			switch {
			case r == ' ':
				b.WriteRune('-')
			case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
				b.WriteRune(r)
			}
		}
		a := b.String()
		if n := seen[a]; n > 0 {
			seen[a]++
			a = fmt.Sprintf("%s-%d", a, n)
		} else {
			seen[a] = 1
		}
		anchors[a] = true
	}
	return anchors
}

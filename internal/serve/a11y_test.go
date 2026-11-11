package serve

// The UI against WCAG 2.2 AA, as far as a test can tell from the HTML and
// the CSS: every page in every language. test/a11y/axe.sh checks the same
// pages in a browser, with axe-core.

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// a11yServer is pagesServer with a proposal, a rule that caught nothing
// (for the noise's heatmap) and a task with an issue in a tracker, and the
// pages to read: those TestPagesInEachLanguage renders, the proposals, and
// a page not found.
func a11yServer(t *testing.T) (*Server, []string) {
	t.Helper()
	ctx := context.Background()
	s := pagesServer(t)
	if _, err := s.propose(ctx, ask{Rule: "Errors", Kind: ledger.KindFor, For: 15 * time.Minute}, "ana", false, true); err != nil {
		t.Fatal(err)
	}
	from := now.Add(-20 * 24 * time.Hour)
	if err := s.l.Record(ctx, ledger.Found{From: from, To: from.Add(time.Hour), Run: ledger.Run{Kind: "backfill", Started: from, Finished: from},
		Firings: []firing.Firing{{Rule: "Noise", Labels: map[string]string{"service": "search"}, Start: from, End: from.Add(10 * time.Minute)}}},
		s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "linked", Kind: "tune", Rule: "Noise", Link: "https://tracker.example/1"}, "ana"); err != nil {
		t.Fatal(err)
	}
	return s, append(slices.Clone(pagePaths), "/proposals", "/proposals?state=all", "/proposals/1", "/rules/Nothing")
}

// TestA11yServe serves the pages at LAPILLI_A11Y_SERVE until it is
// stopped, for test/a11y/axe.sh; /a11y-paths lists them.
func TestA11yServe(t *testing.T) {
	addr := os.Getenv("LAPILLI_A11Y_SERVE")
	if addr == "" {
		t.Skip("LAPILLI_A11Y_SERVE isn't set")
	}
	s, paths := a11yServer(t)
	s.cfg.AllowedHosts = append(s.cfg.AllowedHosts, "host.docker.internal")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /a11y-paths", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, strings.Join(paths, "\n"))
	})
	mux.Handle("/", s.Handler())
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("serving %d pages on %s", len(paths), ln.Addr())
	t.Fatal((&http.Server{Handler: mux, ReadHeaderTimeout: time.Minute}).Serve(ln))
}

// Every page, in every language, has its parts where assistive technology
// looks for them, and names every control, link, image and chart.
func TestPagesAccessible(t *testing.T) {
	s, paths := a11yServer(t)
	h := s.Handler()
	for _, n := range names {
		for _, p := range paths {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Accept-Language", n.Code)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if want := http.StatusOK; w.Code != want && !strings.HasSuffix(p, "/none") && !strings.HasSuffix(p, "/Nothing") {
				t.Errorf("%s %s: %d, not %d", n.Code, p, w.Code, want)
			}
			doc, err := html.Parse(w.Body)
			if err != nil {
				t.Fatalf("%s %s: %v", n.Code, p, err)
			}
			for _, problem := range a11yProblems(doc, n.Code) {
				t.Errorf("%s %s: %s", n.Code, p, problem)
			}
		}
	}
}

// The charts say their numbers are in the table below: the table there has
// a column for each measure they draw, and a row for each week they draw
// something for.
func TestChartsInTables(t *testing.T) {
	s, _ := a11yServer(t)
	h := s.Handler()
	date := regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	for p, measures := range map[string][]string{
		"/":         {"Fired", "○ Caught", "● Nothing", "Precision", "Incidents", "✓ Caught", "◐ Noisy only", "✗ Missed"},
		"/outcomes": {"From the file", "Written in Lapilli", "Impact windows to decide", "Edits"},
	} {
		_, body, _ := get(t, h, p)
		doc, err := html.Parse(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var table *html.Node // the last on the page, below the charts
		drawn := map[string]bool{}
		for _, n := range allNodes(doc) {
			switch {
			case n.DataAtom == atom.Table:
				table = n
			case n.Data == "title" && nodeWithin(n, atom.Svg):
				for _, d := range date.FindAllString(nodeText(n), -1) {
					drawn[d] = true
				}
			}
		}
		if len(drawn) == 0 || table == nil {
			t.Fatalf("%s: charts drawing %d weeks, and a table: %v", p, len(drawn), table != nil)
		}
		var heads []string
		weeks := map[string]bool{}
		for _, n := range allNodes(table) {
			switch {
			case n.DataAtom == atom.Th && nodeWithin(n, atom.Thead):
				heads = append(heads, nodeText(n))
			case n.DataAtom == atom.Tr && nodeWithin(n, atom.Tbody):
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type == html.ElementNode {
						weeks[date.FindString(nodeText(c))] = true
						break
					}
				}
			}
		}
		for _, m := range measures {
			if !slices.Contains(heads, m) {
				t.Errorf("%s: the table below the charts has no %q: %q", p, m, heads)
			}
		}
		for d := range drawn {
			if !weeks[d] {
				t.Errorf("%s: a chart draws the week of %s, the table below hasn't it", p, d)
			}
		}
	}
}

// a11yProblems are what a page, in a language, gets wrong.
func a11yProblems(doc *html.Node, lang string) []string {
	var out []string
	bad := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	all := allNodes(doc)
	ids := map[string]*html.Node{}
	for _, n := range all {
		if id := nodeAttr(n, "id"); id != "" {
			if ids[id] != nil {
				bad("id %q is given twice", id)
			}
			ids[id] = n
		}
	}
	labelled := map[string]bool{} // ids a label is for
	for _, n := range all {
		if n.DataAtom == atom.Label && nodeAttr(n, "for") != "" {
			labelled[nodeAttr(n, "for")] = true
		}
	}
	// name is what assistive technology reads for an element named by its
	// attributes, or "".
	name := func(n *html.Node) string {
		if v := strings.TrimSpace(nodeAttr(n, "aria-label")); v != "" {
			return v
		}
		var parts []string
		for _, id := range strings.Fields(nodeAttr(n, "aria-labelledby")) {
			if x := ids[id]; x != nil {
				parts = append(parts, nodeText(x))
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	}

	// The language, the title, the landmarks, and a way past them.
	var root, body, title, h1, mainEl *html.Node
	count := map[atom.Atom]int{}
	for _, n := range all {
		if n.Type != html.ElementNode {
			continue
		}
		count[n.DataAtom]++
		switch n.DataAtom {
		case atom.Html:
			root = n
		case atom.Body:
			body = n
		case atom.Title:
			if nodeWithin(n, atom.Head) {
				title = n
			}
		case atom.H1:
			h1 = n
		case atom.Main:
			mainEl = n
		}
	}
	if root == nil || nodeAttr(root, "lang") != lang {
		bad("html isn't lang=%q", lang)
	}
	for _, a := range []atom.Atom{atom.Header, atom.Nav, atom.Main, atom.Footer} {
		if count[a] == 0 {
			bad("no %s", a)
		}
	}
	if count[atom.Main] > 1 || count[atom.Header] > 1 || count[atom.Footer] > 1 {
		bad("more than one main, header or footer")
	}
	if count[atom.H1] != 1 {
		bad("%d h1, not one", count[atom.H1])
	}
	if title == nil || h1 == nil || !strings.Contains(squashed(nodeText(title)), squashed(nodeText(h1))) || squashed(nodeText(title)) == squashed(nodeText(h1)) {
		bad("the title doesn't name the page and the site: %q, h1 %q", nodeText(title), nodeText(h1))
	}
	if body != nil {
		first := body.FirstChild
		for first != nil && first.Type != html.ElementNode {
			first = first.NextSibling
		}
		if first == nil || first.DataAtom != atom.A || mainEl == nil || nodeAttr(first, "href") != "#"+nodeAttr(mainEl, "id") || nodeAttr(mainEl, "id") == "" {
			bad("the body doesn't start with a link to main")
		}
	}
	// Each landmark of a kind that comes more than once has a name of its
	// own.
	navs := map[string]bool{}
	for _, n := range all {
		if n.DataAtom == atom.Nav && count[atom.Nav] > 1 {
			if l := name(n); l == "" || navs[l] {
				bad("a nav without a name of its own: %q", l)
			} else {
				navs[l] = true
			}
		}
	}

	// Each language is offered in a nav, named in its own language.
	for _, l := range names {
		if !slices.ContainsFunc(all, func(n *html.Node) bool {
			return nodeAttr(n, "lang") == l.Code && nodeWithin(n, atom.Nav) && nodeText(n) == l.Name
		}) {
			bad("no %s in a nav, in lang=%q", l.Name, l.Code)
		}
	}

	// Headings go down a level at a time.
	level := 0
	for _, n := range all {
		if n.Type == html.ElementNode && len(n.Data) == 2 && n.Data[0] == 'h' && n.Data[1] >= '1' && n.Data[1] <= '6' {
			l := int(n.Data[1] - '0')
			if l > level+1 {
				bad("%s %q after h%d", n.Data, nodeText(n), level)
			}
			level = l
		}
	}

	for _, n := range all {
		if n.Type != html.ElementNode {
			continue
		}
		if v, err := strconv.Atoi(nodeAttr(n, "tabindex")); err == nil && v > 0 {
			bad("tabindex=%d on %s", v, n.Data)
		}
		if l := nodeAttr(n, "lang"); n != root && l != "" && !known(l) {
			bad("lang=%q on %s", l, n.Data)
		}
		switch n.DataAtom {
		case atom.Input, atom.Select, atom.Textarea:
			if n.DataAtom == atom.Input && slices.Contains([]string{"hidden", "submit", "button", "reset", "image"}, nodeAttr(n, "type")) {
				continue
			}
			id := nodeAttr(n, "id")
			if name(n) == "" && !(id != "" && labelled[id]) && !nodeWithin(n, atom.Label) {
				bad("%s name=%q has no label", n.Data, nodeAttr(n, "name"))
			}
		case atom.A, atom.Button:
			if n.DataAtom == atom.A && !hasNodeAttr(n, "href") {
				continue
			}
			if l := firstOf(name(n), nodeText(n)); !strings.ContainsFunc(l, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) {
				bad("%s %q has no name in words", n.Data, l)
			}
		case atom.Img:
			if !hasNodeAttr(n, "alt") {
				bad("img %q has no alt", nodeAttr(n, "src"))
			}
		case atom.Svg:
			if nodeAttr(n, "aria-hidden") == "true" {
				continue
			}
			l := name(n)
			for c := n.FirstChild; c != nil && l == ""; c = c.NextSibling {
				if c.Type == html.ElementNode && c.Data == "title" {
					l = nodeText(c)
				}
			}
			if nodeAttr(n, "role") != "img" || l == "" {
				bad("svg %q isn't an image with a name, nor hidden", nodeAttr(n, "class"))
			}
		case atom.Table:
			named := name(n) != ""
			ths := 0
			for _, c := range allNodes(n) {
				switch c.DataAtom {
				case atom.Caption:
					named = true
				case atom.Th:
					ths++
					if nodeAttr(c, "scope") == "" && !named {
						bad("a th %q without a scope, in a table without a caption", nodeText(c))
					}
					if strings.TrimSpace(nodeText(c)) == "" {
						bad("an empty th")
					}
				}
			}
			if ths == 0 {
				bad("a table without th")
			}
		}
	}
	return out
}

func allNodes(n *html.Node) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, c)
		out = append(out, allNodes(c)...)
	}
	return out
}

func nodeAttr(n *html.Node, key string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasNodeAttr(n *html.Node, key string) bool {
	return slices.ContainsFunc(n.Attr, func(a html.Attribute) bool { return a.Key == key })
}

// nodeText is what an element says: its text, and its images' alt.
func nodeText(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range allNodes(n) {
		switch {
		case c.Type == html.TextNode:
			b.WriteString(c.Data)
		case c.DataAtom == atom.Img:
			b.WriteString(nodeAttr(c, "alt"))
		}
	}
	return strings.TrimSpace(b.String())
}

func nodeWithin(n *html.Node, a atom.Atom) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.DataAtom == a {
			return true
		}
	}
	return false
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func squashed(s string) string { return strings.Join(strings.Fields(s), " ") }

// Text is 4.5:1 or more on each surface it sits on, and what shows a
// control or where the focus is, 3:1, in light and in dark.
func TestContrast(t *testing.T) {
	b, err := files.ReadFile("static/lapilli.css")
	if err != nil {
		t.Fatal(err)
	}
	light, dark := cssTokens(string(b))
	texts := map[string][]string{
		"ink":       {"bg", "card", "panel", "accent-soft", "accent-wash"},
		"dim":       {"bg", "card", "panel", "accent-soft"},
		"accent":    {"bg", "card", "panel", "accent-soft"},
		"bad":       {"bg", "card", "panel"},
		"better":    {"card"},
		"worse":     {"card"},
		"viz-muted": {"card"},
		"on-accent": {"accent"},
		"on-blue":   {"tint-blue"},
		"on-lav":    {"tint-lav"},
		"on-green":  {"accent-soft"},
	}
	ui := map[string][]string{
		"accent":  {"bg", "card", "panel"}, // the focus ring
		"line-ui": {"bg", "card", "panel"}, // a field's edge, a link's underline
	}
	for mode, tokens := range map[string]map[string]string{"light": light, "dark": dark} {
		for least, pairs := range map[float64]map[string][]string{4.5: texts, 3: ui} {
			for fg, bgs := range pairs {
				for _, bg := range bgs {
					r, err := tokenContrast(tokens, fg, bg)
					if err != nil {
						t.Errorf("%s: %v", mode, err)
					} else if r < least {
						t.Errorf("%s: --%s on --%s is %.2f:1, under %.1f:1", mode, fg, bg, r, least)
					}
				}
			}
		}
	}
}

var (
	cssRootRe  = regexp.MustCompile(`(@media \(prefers-color-scheme: dark\) \{\s*)?:root \{([^}]*)\}`)
	cssTokenRe = regexp.MustCompile(`--([\w-]+):\s*([^;]+);`)
)

// cssTokens are the colour tokens of the stylesheet, in light and in dark,
// dark falling back on light.
func cssTokens(css string) (light, dark map[string]string) {
	light, dark = map[string]string{}, map[string]string{}
	for _, m := range cssRootRe.FindAllStringSubmatch(css, -1) {
		to := light
		if m[1] != "" {
			to = dark
		}
		for _, x := range cssTokenRe.FindAllStringSubmatch(m[2], -1) {
			to[x[1]] = strings.TrimSpace(x[2])
		}
	}
	for k, v := range light {
		if _, ok := dark[k]; !ok {
			dark[k] = v
		}
	}
	return light, dark
}

// contrast is WCAG's contrast ratio of a token on another, a colour with
// alpha laid over the one under it.
func tokenContrast(tokens map[string]string, fg, bg string) (float64, error) {
	b, err := cssRGBA(tokens[bg])
	if err != nil {
		return 0, fmt.Errorf("--%s: %v", bg, err)
	}
	f, err := cssRGBA(tokens[fg])
	if err != nil {
		return 0, fmt.Errorf("--%s: %v", fg, err)
	}
	for i := range 3 {
		f[i] = f[i]*f[3] + b[i]*(1-f[3])
	}
	lf, lb := relLuminance(f), relLuminance(b)
	return (max(lf, lb) + 0.05) / (min(lf, lb) + 0.05), nil
}

func cssRGBA(hex string) ([4]float64, error) {
	h := strings.TrimPrefix(hex, "#")
	if len(h) == 6 {
		h += "ff"
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if len(h) != 8 || err != nil {
		return [4]float64{}, fmt.Errorf("not a colour: %q", hex)
	}
	return [4]float64{float64(v>>24&255) / 255, float64(v>>16&255) / 255, float64(v>>8&255) / 255, float64(v&255) / 255}, nil
}

func relLuminance(c [4]float64) float64 {
	lin := func(x float64) float64 {
		if x <= 0.04045 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c[0]) + 0.7152*lin(c[1]) + 0.0722*lin(c[2])
}

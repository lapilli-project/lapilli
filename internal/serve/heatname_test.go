package serve

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// The heatmap cuts a long rule's name short to keep its layout; the whole
// name stays reachable: in the markup for a screen reader, laid over the
// cells beside it for the pointer and for the keyboard's focus (CSS, in the
// row's own height, so nothing moves), and on the rule's page it links to.
// A title would show it a second time, and not for the keyboard.
func TestHeatmapNameReachable(t *testing.T) {
	parallel(t)
	s, _ := a11yServer(t)
	_, body, _ := get(t, s.Handler(), "/")
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for _, n := range allNodes(doc) {
		if n.DataAtom != atom.Th || nodeAttr(n, "class") != "hn" {
			continue
		}
		rows++
		var link *html.Node
		for _, c := range allNodes(n) {
			if c.DataAtom == atom.A {
				link = c
			}
		}
		if link == nil {
			t.Fatalf("a row header with no link: %s", nodeText(n))
		}
		if nodeText(link) == "" || nodeAttr(link, "data-name") != nodeText(link) {
			t.Errorf("the link says %q, its data-name %q", nodeText(link), nodeAttr(link, "data-name"))
		}
		if hasNodeAttr(link, "title") {
			t.Error("a title on the link shows the name a second time, and not to the keyboard")
		}
	}
	if rows == 0 {
		t.Fatal("no heatmap rows")
	}
	b, err := files.ReadFile("static/lapilli.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	i := strings.Index(css, "table.heat th.hn a:is(:hover, :focus-visible)::after {")
	if i < 0 {
		t.Fatal("no rule that shows the whole name with the pointer or the focus on it")
	}
	rule := css[i : i+strings.Index(css[i:], "}")]
	for _, want := range []string{"content: attr(data-name) / \"\"", "position: absolute", "min-height: 100%", "background: var(--card)"} {
		if !strings.Contains(rule, want) {
			t.Errorf("the rule has no %q:\n%s", want, rule)
		}
	}
	if !strings.Contains(css, "table.heat th.hn a { display: block; overflow: hidden; text-overflow: ellipsis; }") {
		t.Error("the cut is on the link, so what is laid over it isn't clipped")
	}
}

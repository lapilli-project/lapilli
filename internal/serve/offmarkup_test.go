package serve

import (
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// TestDisabledOneWay: every action a reader may not take, on every page,
// is drawn one way, the off template's (docs/design/ui.md): a button of its
// own, disabled, in an action-off, tied by aria-describedby to the sentence
// that says why, which is said once on the page; but the hidden button a
// bulk form's Enter presses (noenter), and a rule's header, whose reasons
// are said after its buttons. Read-only, as a viewer, and as a team's
// editor, the overview's suggestions, the firings' buttons and the rules
// list's bar among them.
func TestDisabledOneWay(t *testing.T) {
	s, paths := a11yServer(t)
	h := s.Handler()
	s.cfg.Admins, s.cfg.Editors, s.cfg.UserHeader = []string{"boss"}, []string{"ed"}, "X-Forwarded-Email"
	paths = append(paths, "/firings?day=2026-11-02", "/rules?weeks=12")
	seen := 0
	for _, who := range []string{"", "vi", "pat"} {
		for _, p := range paths {
			code, body := getAs(t, h, p, who)
			if code >= 500 {
				t.Fatalf("%s as %q: %d", p, who, code)
			}
			doc, err := html.Parse(strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			all := allNodes(doc)
			whys := map[string]int{}
			for _, n := range all {
				if id := nodeAttr(n, "id"); id != "" && slices.Contains(strings.Fields(nodeAttr(n, "class")), "why") {
					whys[id]++
				}
			}
			for id, k := range whys {
				if k > 1 {
					t.Errorf("%s as %q: why %s said %d times", p, who, id, k)
				}
			}
			for _, n := range all {
				if n.DataAtom != atom.Button || !hasNodeAttr(n, "disabled") || hasNodeAttr(n, "hidden") || inClass(n, "rule-actions") {
					continue
				}
				seen++
				if nodeAttr(n, "type") != "button" || !slices.Contains(strings.Fields(nodeAttr(n.Parent, "class")), "action-off") {
					t.Errorf("%s as %q: %q is disabled by hand, not by the off template: %v", p, who, nodeText(n), n.Attr)
				}
				if id := nodeAttr(n, "aria-describedby"); whys[id] != 1 {
					t.Errorf("%s as %q: %q is tied to %q, which says why %d times", p, who, nodeText(n), id, whys[id])
				}
			}
		}
	}
	if seen < 20 {
		t.Errorf("only %d disabled actions looked at", seen)
	}
}

// inClass reports whether n is inside an element of the class.
func inClass(n *html.Node, class string) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if slices.Contains(strings.Fields(nodeAttr(p, "class")), class) {
			return true
		}
	}
	return false
}

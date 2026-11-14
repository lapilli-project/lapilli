package serve

import (
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// defaultButton is the button Enter in a form presses (implicit
// submission): the first submit button, in the document's order, whose
// form is the one whose action is action, by its form= or by being in it.
func defaultButton(t *testing.T, body, action string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	all := allNodes(doc)
	var form *html.Node
	for _, n := range all {
		if n.DataAtom == atom.Form && nodeAttr(n, "action") == action {
			form = n
		}
	}
	if form == nil {
		t.Fatalf("no form to %s", action)
	}
	for _, n := range all {
		submit := n.DataAtom == atom.Button && (nodeAttr(n, "type") == "" || nodeAttr(n, "type") == "submit") ||
			n.DataAtom == atom.Input && (nodeAttr(n, "type") == "submit" || nodeAttr(n, "type") == "image")
		if !submit {
			continue
		}
		owner := n.Parent
		for owner != nil && owner.DataAtom != atom.Form {
			owner = owner.Parent
		}
		if id := nodeAttr(n, "form"); id != "" {
			owner = nil
			if nodeAttr(form, "id") == id {
				owner = form
			}
		}
		if owner == form {
			return n
		}
	}
	t.Fatalf("the form to %s has no button", action)
	return nil
}

// TestBulkNoEnter: Enter in a bulk form, in a checkbox or a list, presses
// the form's first button: a disabled one, so that it writes nothing,
// hidden from sight and from screen readers; each action is a button
// pressed.
func TestBulkNoEnter(t *testing.T) {
	s, _, _ := bulkServer(t, nil)
	h := s.Handler()
	l := labelServer(t, nil)
	for _, c := range []struct {
		h            http.Handler
		path, action string
	}{{h, "/rules?weeks=12&perf=ok&perf=noisy", "/rules/bulk"}, {l.Handler(), "/firings?day=2026-11-02", "/firings/real"}} {
		_, body := getAs(t, c.h, c.path, "ed@example.com")
		b := defaultButton(t, body, c.action)
		if !hasNodeAttr(b, "disabled") || !hasNodeAttr(b, "hidden") || nodeAttr(b, "name") != "" {
			t.Errorf("%s: Enter presses %v", c.path, b.Attr)
		}
	}
}

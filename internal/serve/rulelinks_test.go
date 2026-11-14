package serve

// Links into a rule's page from elsewhere: the notices, and the pages that
// show a period of their own.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"lapilli.dev/internal/ledger"
)

// TestNoticeLinks: a notice links to where what it tells of is: a comment
// on a rule to the comments on its activity tab, a replay that differs to
// the rules whose replay does.
func TestNoticeLinks(t *testing.T) {
	s, _ := server(t, func(c *Config) { c.ExternalURL = "https://lapilli.example.com/" })
	for _, c := range []struct {
		n    notice
		want string
	}{
		{notice{Event: "comment.written", Comment: &ledger.Comment{On: "rule", Key: "Errors/a b"}}, "/rules/Errors%2Fa%20b?tab=activity#comments"},
		{notice{Event: "comment.written", Comment: &ledger.Comment{On: "incident", Key: "INC-1"}}, "/incidents/INC-1"},
		{notice{Event: "comment.written", Comment: &ledger.Comment{On: "task", Key: "3"}}, "/tasks/3"},
		{notice{Event: "replay.differs"}, "/rules?trust=differs"},
	} {
		if got := s.pageOf(c.n); got != "https://lapilli.example.com"+c.want {
			t.Errorf("%s on %+v: %s, want %s", c.n.Event, c.n.Comment, got, c.want)
		}
	}
	// Where a rule's comment links to, its comments are, and nowhere else
	// on its page.
	a, _ := a11yServer(t)
	for path, want := range map[string]bool{"/rules/Errors?tab=activity": true, "/rules/Errors": false, "/rules/Errors?tab=firings": false} {
		code, body, _ := get(t, a.Handler(), path)
		if code != http.StatusOK {
			t.Fatalf("%s: %d", path, code)
		}
		doc, err := html.Parse(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, n := range allNodes(doc) {
			if nodeAttr(n, "id") == "comments" {
				found = true
				if !strings.Contains(nodeText(n), "Comments") {
					t.Errorf("%s#comments: %q", path, squashed(nodeText(n)))
				}
			}
		}
		if found != want {
			t.Errorf("%s: #comments %v, want %v", path, found, want)
		}
	}
}

// TestRuleLinksPeriod: a link to a rule's page from a page that shows a
// period of its own has the weeks the reader saw there: from the rules
// list, four unless it was asked otherwise, as a rule's page is twelve;
// from the overview's items that speak of four weeks, four; and a search
// from the rules list opens a rule over its weeks. Pages without a period
// of their own carry the weeks as their URL has them.
func TestRuleLinksPeriod(t *testing.T) {
	s := facetServer(t)
	h := s.Handler()
	toRule := regexp.MustCompile(`href="(/rules/[^"]*)"`)
	for path, want := range map[string]string{"/rules": "4", "/rules?weeks=12": "12", "/rules?weeks=1": "1", "/rules?perf=silent": "4",
		"/rules?has=proposal": "4", "/search?q=R1": "", "/search?q=R1&weeks=4": "4", "/": ""} {
		_, body, _ := get(t, h, path)
		ms := toRule.FindAllStringSubmatch(body, -1)
		if len(ms) == 0 {
			t.Errorf("%s: no link to a rule's page", path)
		}
		for _, m := range ms {
			u, err := url.Parse(html.UnescapeString(m[1]))
			if err != nil {
				t.Fatal(err)
			}
			if got := u.Query().Get("weeks"); got != want && !(path == "/" && got == "4") {
				t.Errorf("%s links to %s: weeks %q, want %q", path, m[1], got, want)
			}
		}
	}
	// The overview's items that speak of four weeks.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	o, err := s.overview(r)
	if err != nil {
		t.Fatal(err)
	}
	four := 0
	for _, a := range o.(dashboard).Actions {
		if !strings.HasPrefix(a.Href, "/rules/") {
			continue
		}
		u, _ := url.Parse(a.Href)
		if says := strings.Contains(a.Detail, "four weeks"); says != (u.Query().Get("weeks") == "4") {
			t.Errorf("%q, %q: %s", a.Text, a.Detail, a.Href)
		} else if says {
			four++
		}
	}
	if four == 0 {
		t.Error("the overview has no item over four weeks")
	}
	// The search: from the rules list with its weeks, from the overview
	// without; one rule found opens over them.
	for path, want := range map[string]string{"/rules": `<input type="hidden" name="weeks" value="4">`, "/rules?weeks=12": `<input type="hidden" name="weeks" value="12">`} {
		if _, body, _ := get(t, h, path); !strings.Contains(body, `role="search"`) || !strings.Contains(searchForm(body), want) {
			t.Errorf("%s: the search has no %s", path, want)
		}
	}
	if _, body, _ := get(t, h, "/"); strings.Contains(searchForm(body), `name="weeks"`) {
		t.Error("/: the search has weeks")
	}
	if code, _, hdr := get(t, h, "/search?q=R20&weeks=4"); code != http.StatusSeeOther || hdr.Get("Location") != "/rules/R20?weeks=4" {
		t.Errorf("a search for one rule: %d %s", code, hdr.Get("Location"))
	}
}

// searchForm is the bar's search form of a page.
func searchForm(body string) string {
	i := strings.Index(body, `role="search"`)
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], "</form>")
	return body[i : i+j]
}

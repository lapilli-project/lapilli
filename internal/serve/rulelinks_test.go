package serve

// Links into a rule's page from elsewhere: the notices, and the pages that
// show a period of their own.

import (
	"net/http"
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

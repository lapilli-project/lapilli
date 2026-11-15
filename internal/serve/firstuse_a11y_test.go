package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"lapilli.dev/internal/ledger"
)

// firstUseServer is a deployment as a first-time user has it, as one was
// found: rules that say whose they are in a label, and no team; changes on,
// a proposal the only admin made waiting for an approval no one can give;
// nowhere to send a notice, and a report on with nowhere to go. Its pages
// are among those TestA11yServe serves (under /first), and
// TestFirstUseAccessible checks.
func firstUseServer(t *testing.T) (*Server, []string) {
	t.Helper()
	ctx := context.Background()
	s := labelledServer(t, labelledRules+"      - alert: Errors\n        expr: errors > 5\n        for: 2m\n", func(c *Config) {
		c.UserHeader, c.Editors, c.Admins = "X-Forwarded-Email", []string{"*"}, []string{ana}
		c.Changes, c.ChangesToken = true, tokenFile(t, changesToken)
	})
	id, err := s.propose(ctx, ask{Rule: "Errors", Kind: ledger.KindFor, For: 15 * time.Minute}, ana, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutScheduled(ctx, ledger.Scheduled{Name: "daily", Schedule: "0 9 * * *", Span: "24h", Zone: "ledger", Sections: ledger.ReportSections,
		Dests: []string{"old"}, Enabled: true}, ana, now); err != nil {
		t.Fatal(err)
	}
	return s, []string{"/teams", "/teams?made=3&skipped=1", "/rules", "/rules/Plain", "/settings", "/manage", "/firings", "/proposals/" + itoa(id)}
}

// TestFirstUseAccessible: the first-time user's pages, in every language,
// have their parts where assistive technology looks for them, as every
// page does (TestPagesAccessible), and say what this bundle has them say.
func TestFirstUseAccessible(t *testing.T) {
	s, paths := firstUseServer(t)
	h := s.Handler()
	for _, n := range names {
		for _, p := range paths {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Accept-Language", n.Code)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Errorf("%s %s: %d", n.Code, p, w.Code)
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
	for p, said := range map[string]string{
		"/teams":       "Make these teams</button> <span id=\"suggested-why\" class=\"why\">Read-only: no one is signed in</span>",
		"/rules":       "No team owns a rule yet, so the Team column is empty.",
		"/settings":    "1 report is on with nowhere to go",
		"/manage":      "The weekly digest goes nowhere.",
		"/proposals/1": "No one may approve it: only the admins approve",
	} {
		if _, body, _ := get(t, h, p); !strings.Contains(body, said) {
			t.Errorf("%s doesn't say %q", p, said)
		}
	}
	inEachLanguage(t, h, "", paths...)
}

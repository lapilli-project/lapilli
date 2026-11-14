package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
)

// The fourth phase of the web UI (docs/design/ui.md) beyond the
// proposals: the tasks' facet, the quiet windows' preview, the Manage
// page, and what a reader may not do, shown disabled with why.

// tasksFor is the tasks list for a query.
func tasksFor(t *testing.T, s *Server, query string) tasksView {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/tasks?"+query, nil)
	_, vr := s.withContext(r, "tasks")
	v, err := s.tasksPage(vr)
	if err != nil {
		t.Fatal(err)
	}
	return v.(tasksView)
}

// TestTasksFacet: the tasks' status is a facet, each value counted for the
// team: the open ones (open and being done) unless asked; done, dropped,
// all, and an older link's status as it was; the team kept in the form.
func TestTasksFacet(t *testing.T) {
	s, _ := server(t, nil)
	ctx := context.Background()
	pay, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "^P"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	for i, st := range []string{"open", "doing", "done", "dropped", "open", "done", "done"} {
		task := ledger.Task{Title: "t", Kind: "tune", Status: st}
		if i < 4 {
			task.TeamID = pay
		}
		if _, err := s.l.PutTask(ctx, task, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	count := func(v tasksView) map[string]int {
		out := map[string]int{}
		for _, o := range v.Pick.Facets[0].Options {
			out[o.Value] = o.Count
		}
		return out
	}
	for _, c := range []struct {
		q            string
		shown, total int
		counts       map[string]int
	}{
		{"", 3, 7, map[string]int{"open": 3, "done": 3, "dropped": 1}},
		{"status=all", 7, 7, nil},
		{"status=done", 3, 7, nil},
		{"status=done&status=dropped", 4, 7, nil},
		{"status=doing", 1, 7, map[string]int{"open": 3, "done": 3, "dropped": 1, "doing": 1}},
		{"status=bogus", 7, 7, nil},
		{"team=payments", 2, 4, map[string]int{"open": 2, "done": 1, "dropped": 1}},
		{"team=payments&status=all", 4, 4, nil},
	} {
		v := tasksFor(t, s, c.q)
		if len(v.Lines) != c.shown || v.Total != c.total {
			t.Errorf("%q: %d of %d, want %d of %d", c.q, len(v.Lines), v.Total, c.shown, c.total)
		}
		if got := count(v); c.counts != nil && len(got) != len(c.counts) {
			t.Errorf("%q: counts %v, want %v", c.q, got, c.counts)
		} else {
			for k, n := range c.counts {
				if got[k] != n {
					t.Errorf("%q: %s counts %d, want %d", c.q, k, got[k], n)
				}
			}
		}
	}
	v := tasksFor(t, s, "team=payments&status=done")
	if len(v.Pick.Chips) != 2 || v.Pick.Chips[1].Href != "/tasks?status=all&team=payments" || v.Pick.Clear != "/tasks?status=all&team=payments" {
		t.Errorf("chips %+v, clear %q", v.Pick.Chips, v.Pick.Clear)
	}
	_, body, _ := get(t, s.Handler(), "/tasks?team=payments")
	if !strings.Contains(body, `<form method="get" action="/tasks" class="facets pick" aria-label="Filters"><input type="hidden" name="team" value="payments"><input type="hidden" name="status" value="all">`) ||
		!strings.Contains(body, `<input type="checkbox" name="status" value="open" checked> <span>open</span> <span class="n">2</span>`) {
		t.Error("the form doesn't keep the team, or count the open ones")
	}
}

// TestFixTextsTranslated: every text the proposals' pages, the tasks'
// facet and the actions shown disabled take from the code has a
// translation: some are shown only in states the pages rendered elsewhere
// aren't in.
func TestFixTextsTranslated(t *testing.T) {
	texts := map[string]bool{}
	for st := range stateGroups {
		b := badgeOf(st)
		texts[b.Word] = true
		if b.Detail != "" {
			texts[b.Detail] = true
		}
	}
	d := time.Minute
	for _, ev := range []evidence{{}, {Edit: &rules.Edit{For: &d, From: "1", To: "2", Annotation: "summary"}}, {Edit: &rules.Edit{Annotation: "summary", Unset: true}}} {
		for _, k := range []string{ledger.KindFor, ledger.KindThreshold, ledger.KindDisable, ledger.KindDelete, ledger.KindSays, ledger.KindUndo, "other"} {
			texts[headOf(ledger.Proposal{Kind: k, Rule: "r"}, ev).Format] = true
		}
	}
	for _, r := range btTableOf(&backtest{}).Rows {
		texts[r.Label] = true
	}
	for _, f := range append(slices.Clone(proposalFacets), taskFacets...) {
		texts[f.name] = true
		for _, l := range f.labels {
			texts[l] = true
		}
	}
	for _, ls := range diffLinks(diffOpts{}, func(string, string) string { return "" }) {
		for _, l := range ls {
			texts[l.Label] = true
		}
	}
	for _, x := range []string{"Now", "As proposed", "As the change left it", "As it was before", "The version before", "This version",
		"I have read the backtest", "I have read the change and its evidence", "Approvers only", "Editors only", "Admins only",
		"Changes are off: lapilli serve was started without --changes", "For a person to make where the rule is kept: nothing to approve here",
		"You proposed this: someone else approves it.", "You approved it: it waits for another approver.", "Ask to undo it", "Drop it", "Close it",
		"Make it a task", "Approve", "Pause changes", "Resume changes", "Lapilli", "a change recipe", "No proposals yet.", "No proposal matches these filters.",
		"A rule's page proposes a change to it, with its backtest; Lapilli proposes of itself with --changes, after the weekly digest."} {
		texts[x] = true
	}
	for x := range texts {
		for lang, c := range catalogs {
			if _, ok := c[x]; !ok {
				t.Errorf("%s has no translation for %q", lang, x)
			}
		}
	}
}

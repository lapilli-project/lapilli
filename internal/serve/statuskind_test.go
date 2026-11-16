package serve

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
)

// statusStates gives s, an a11yServer, the states its pages don't show
// already, so that every status kind is drawn somewhere: a quiet window in
// effect now (it quiets no rule there is), a task overdue, one doing and
// one done, the waiting proposal made a task, a rule to keep (Limited), a run that
// failed, and proposals approved, failed, refused and stale (backtested by
// a newer lapilli, so that it is left for a person). The pages that show
// them, for TestStatusKinds and test/a11y/axe.sh (TestA11yServe).
func statusStates(t *testing.T, s *Server) []string {
	t.Helper()
	ctx := context.Background()
	if _, err := s.l.PutWindow(ctx, ledger.Window{Title: "now", Start: now.Add(-time.Hour), End: now.Add(time.Hour), Rules: "^NoRuleIsCalledThis$"},
		"ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	var tasks []string
	for _, tk := range []ledger.Task{
		{Title: "late", Kind: "tune", Rule: "Errors", Due: now.Add(-72 * time.Hour)},
		{Title: "under way", Kind: "decide", Rule: "Noise", Status: "doing"},
		{Title: "finished", Kind: "write", Rule: "Noise", Status: "done"},
		{Title: "approve #1", Kind: "decide", Rule: "Errors"},
	} {
		id, err := s.l.PutTask(ctx, tk, "ana")
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, "/tasks/"+strconv.FormatInt(id, 10))
	}
	last, _ := strconv.ParseInt(strings.TrimPrefix(tasks[len(tasks)-1], "/tasks/"), 10, 64)
	if err := s.l.SetProposalTask(ctx, 1, last); err != nil {
		t.Fatal(err)
	}
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "Limited", Status: "keep"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := s.l.AddRun(ctx, ledger.Run{Kind: "hourly", Started: now.Add(-2 * time.Hour), Finished: now.Add(-2 * time.Hour),
		From: now.Add(-3 * time.Hour), To: now.Add(-2 * time.Hour), Error: "prometheus: connection refused"}); err != nil {
		t.Fatal(err)
	}
	five := 5 * time.Minute
	propose := func(rule string, ev evidence, moves ...[2]string) string {
		ev.Edit = &rules.Edit{Name: rule, For: &five}
		b, _ := json.Marshal(ev)
		id, err := s.l.Propose(ctx, ledger.Proposal{Rule: rule, Kind: ledger.KindFor, Path: ledger.PathGrafana, MadeBy: "bo",
			Before: rule + " ()\nfor: 0s\n", After: rule + " ()\nfor: 5m\n", Evidence: b})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range moves {
			reason := map[string]string{ledger.StateFailed: "grafana: 500", ledger.StateRefused: "the rule changed since"}[m[1]]
			if _, err := s.l.Move(ctx, id, []string{m[0]}, m[1], ledger.Proposer, ledger.Step{Reason: reason}); err != nil {
				t.Fatal(err)
			}
		}
		return "/proposals/" + strconv.FormatInt(id, 10)
	}
	bt := &backtest{From: now.Add(-weekLen), To: now, Before: side{Firings: 9, White: 1}, After: side{Firings: 2, White: 1}}
	approved := [2]string{ledger.StateProposed, ledger.StateApproved}
	paths := append(tasks, "/windows", "/tasks", "/tasks?status=all", "/runs",
		propose("Flapping", evidence{Backtest: bt, Semantics: semantics}, approved),
		propose("Limited", evidence{Backtest: bt, Semantics: semantics}, approved, [2]string{ledger.StateApproved, ledger.StateFailed}),
		propose("Limited", evidence{Backtest: bt, Semantics: semantics}, approved, [2]string{ledger.StateApproved, ledger.StateRefused}),
		propose("Limited", evidence{Backtest: bt, Semantics: semantics + 1, Version: "9.9.9"}))
	return paths
}

// statusWant is the status kind of each state the pages draw, as
// docs/design/ui.md's "Status colours" maps it: the text of the element
// coloured (a badge's with its detail beside it), in English; the kind it
// is, on every page; and pages it must be on. A state Lapilli can't judge
// is unknown, never ok or bad.
var statusWant = []struct {
	text *regexp.Regexp
	kind string
	on   []string
}{
	// ok
	{regexp.MustCompile(`^○ OK$`), "ok", []string{"/rules"}},
	{regexp.MustCompile(`^✓ approved`), "ok", []string{"/proposals?state=all"}},
	{regexp.MustCompile(`^● applied`), "ok", []string{"/proposals?state=all"}},
	{regexp.MustCompile(`^done$`), "ok", []string{"/tasks?status=all"}},
	{regexp.MustCompile(`^✓ keep$`), "ok", []string{"/rules/Limited"}},
	{regexp.MustCompile(`^○ `), "ok", []string{"/incidents"}},
	{regexp.MustCompile(`^all ran$`), "ok", nil},
	{regexp.MustCompile(`^✓ Last sent from the outbox`), "ok", nil},
	{regexp.MustCompile(`^✓ Nothing to look at now`), "ok", nil},
	{regexp.MustCompile(`^as predicted$`), "ok", nil},
	{regexp.MustCompile(`, better$| better$`), "ok", nil},
	// warn
	{regexp.MustCompile(`^● Noisy`), "warn", []string{"/rules"}},
	{regexp.MustCompile(`^≠ Differs$`), "warn", nil},
	{regexp.MustCompile(`^◌ Not replayed$`), "warn", nil},
	{regexp.MustCompile(`^⚠\x{FE0E} \d+ to check$`), "warn", []string{"/rules/Errors"}},
	{regexp.MustCompile(`^◷ awaiting approval`), "warn", []string{"/proposals", "/proposals/1", "/tasks/7"}},
	{regexp.MustCompile(`^⟳ stale: backtest it again$`), "warn", []string{"/proposals"}},
	{regexp.MustCompile(`^⟳ It is stale`), "warn", []string{"/proposals/7"}},
	{regexp.MustCompile(`^⚙\x{FE0E} (an agent|proposed by an agent)$`), "warn", []string{"/proposals/2", "/proposals?state=all"}},
	{regexp.MustCompile(`^⚠\x{FE0E} An agent proposed this`), "warn", []string{"/proposals/2"}},
	{regexp.MustCompile(`one event alone can't fire it|one event alone could never fire it`), "warn", nil},
	{regexp.MustCompile(`^⚠\x{FE0E} Changes (to the rules )?are paused|On, and paused`), "warn", nil},
	{regexp.MustCompile(`^◐ `), "warn", nil},
	{regexp.MustCompile(`^⚠\x{FE0E} Some of what the (config )?file declares can't be kept`), "warn", []string{"/settings", "/runs", "/manage"}},
	{regexp.MustCompile(`were given back to the UI`), "warn", []string{"/settings"}},
	{regexp.MustCompile(`its team was removed`), "warn", []string{"/reports"}},
	{regexp.MustCompile(`nothing names .* now$`), "warn", []string{"/reports"}},
	{regexp.MustCompile(`^⚠\x{FE0E} Their team was removed`), "warn", []string{"/runs"}},
	{regexp.MustCompile(`held nothing over`), "warn", []string{"/runs"}},
	{regexp.MustCompile(`No one (can|may) approve`), "warn", []string{"/first/proposals/1"}},
	{regexp.MustCompile(`goes nowhere|nowhere to go`), "warn", []string{"/first/manage", "/first/settings"}},
	{regexp.MustCompile(`No incident written down has started|are they still being written down`), "warn", nil},
	{regexp.MustCompile(`^⊘ closed \(expired\)$`), "warn", nil},
	{regexp.MustCompile(`^not as predicted$|^no longer replayed$`), "warn", nil},
	// bad
	{regexp.MustCompile(`^⊘ closed \(failed\)$`), "bad", []string{"/proposals?state=all", "/proposals/5"}},
	{regexp.MustCompile(`^grafana: 500$`), "bad", []string{"/proposals/5"}},
	{regexp.MustCompile(`^✕ prometheus: connection refused$`), "bad", []string{"/runs"}},
	{regexp.MustCompile(`^✕ The last read failed`), "bad", []string{"/runs"}},
	{regexp.MustCompile(`^✗ nothing fired$|^✗ Nothing fired\.$`), "bad", []string{"/incidents"}},
	{regexp.MustCompile(`overdue$`), "bad", []string{"/tasks", "/tasks/4", "/"}},
	{regexp.MustCompile(`^\d+ overdue$`), "bad", []string{"/outcomes"}},
	{regexp.MustCompile(`^\d+ failed$`), "bad", []string{"/outcomes"}},
	{regexp.MustCompile(`^✕ Left out, as it isn't understood`), "bad", []string{"/rules?perf=ok&severity=none&trust=differs&q=i+label%3Aservice%3Dsearch+group%3Ashop+foo%3Abar"}},
	{regexp.MustCompile(`^✕ As proposed, it misses|^✕ Undone, it misses`), "bad", nil},
	{regexp.MustCompile(`^✕ While it was off`), "bad", nil},
	{regexp.MustCompile(` worse$|, worse$`), "bad", nil},
	{regexp.MustCompile(`^✕ a run: boom$`), "bad", []string{"/"}},
	// info
	{regexp.MustCompile(`^● active now$`), "info", []string{"/windows"}},
	{regexp.MustCompile(`^✎ Proposed$`), "info", []string{"/rules/Errors"}},
	{regexp.MustCompile(`^⇣ Recorded only$`), "info", nil},
	{regexp.MustCompile(`^(open|doing)$`), "info", []string{"/tasks?status=all"}},
	{regexp.MustCompile(`^± tune$`), "info", []string{"/rules/Errors"}},
	{regexp.MustCompile(`^↻ being applied`), "info", nil},
	{regexp.MustCompile(`^↻ replaying`), "info", []string{"/"}},
	{regexp.MustCompile(`^↻ Catching up|behind: firings are in up to`), "info", nil},
	{regexp.MustCompile(`^measuring|^rule written$`), "info", nil},
	// unknown
	{regexp.MustCompile(`^\? No record$`), "unknown", nil},
	{regexp.MustCompile(`^– Silent$`), "unknown", []string{"/rules?perf=silent&weeks=12"}},
	{regexp.MustCompile(`^⧉ Spare$`), "unknown", nil},
	{regexp.MustCompile(`^no record$`), "unknown", nil},
	{regexp.MustCompile(`^declared$`), "unknown", []string{"/windows", "/reports"}},
	{regexp.MustCompile(`^⊘ retire$`), "unknown", nil},
	{regexp.MustCompile(`^↶ undone`), "unknown", nil},
	{regexp.MustCompile(`^⊘ closed \((refused|dropped|changed in review)\)$`), "unknown", []string{"/proposals?state=all", "/proposals/6"}},
	{regexp.MustCompile(`^\? No incident was written down`), "unknown", nil},
	{regexp.MustCompile(`^dropped$`), "unknown", nil},
}

// statusOf is each element a page colours by status: its text, a badge's
// with the detail beside it, and its kind.
func statusOf(doc *html.Node) [][2]string {
	var out [][2]string
	for _, n := range allNodes(doc) {
		if n.Type != html.ElementNode {
			continue
		}
		kind := ""
		for _, c := range strings.Fields(nodeAttr(n, "class")) {
			if k, ok := strings.CutPrefix(c, "status-"); ok {
				kind = k
			}
		}
		if kind == "" {
			continue
		}
		text := squashed(nodeText(n))
		next := n.NextSibling
		for next != nil && next.Type == html.TextNode && strings.TrimSpace(next.Data) == "" {
			next = next.NextSibling
		}
		if strings.Contains(nodeAttr(n, "class"), "badge") && next != nil && nodeAttr(next, "class") == "quiet" && strings.HasPrefix(nodeText(next), "(") {
			text += " " + squashed(nodeText(next))
		}
		out = append(out, [2]string{text, kind})
	}
	return out
}

// TestStatusKinds: each state is the same status kind on every page that
// colours it, as statusWant has it (docs/design/ui.md, "Status colours"),
// and is coloured on the pages statusWant names; every kind is one of the
// five; and what Lapilli can't judge ("–", no record) is never ok or bad.
func TestStatusKinds(t *testing.T) {
	s, paths := a11yServer(t)
	paths = append(paths, statusStates(t, s)...)
	first, firstPaths := firstUseServer(t)
	nothing, _ := server(t, nil) // nothing judged yet
	type site struct {
		h      http.Handler
		prefix string
		paths  []string
	}
	sites := []site{{s.Handler(), "", paths}, {first.Handler(), "/first", firstPaths}, {nothing.Handler(), "/none", []string{"/", "/outcomes", "/rules"}}}
	seen := map[string][][2]string{} // page, its coloured elements
	for _, x := range sites {
		for _, p := range x.paths {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Accept-Language", "en")
			w := httptest.NewRecorder()
			x.h.ServeHTTP(w, r)
			doc, err := html.Parse(w.Body)
			if err != nil {
				t.Fatal(err)
			}
			seen[x.prefix+p] = statusOf(doc)
		}
	}
	unjudged := regexp.MustCompile(`^–$|not known|[Nn]o record|no incident (was )?written down`)
	for page, els := range seen {
		for _, e := range els {
			if !slices.Contains(statusKindNames, e[1]) {
				t.Errorf("%s: %q is status-%s, not a kind", page, e[0], e[1])
			}
			if (e[1] == "ok" || e[1] == "bad") && unjudged.MatchString(e[0]) {
				t.Errorf("%s: %q, which Lapilli can't judge, is %s", page, e[0], e[1])
			}
			for _, w := range statusWant {
				if w.text.MatchString(e[0]) && e[1] != w.kind {
					t.Errorf("%s: %q is %s, not %s", page, e[0], e[1], w.kind)
				}
			}
		}
	}
	for _, w := range statusWant {
		for _, page := range w.on {
			els, ok := seen[page]
			if !ok {
				t.Errorf("%s isn't among the pages rendered", page)
				continue
			}
			if !slices.ContainsFunc(els, func(e [2]string) bool { return w.text.MatchString(e[0]) && e[1] == w.kind }) {
				t.Errorf("%s doesn't colour %v as %s: %q", page, w.text, w.kind, els)
			}
		}
	}
	// And every kind is drawn somewhere.
	for _, k := range statusKindNames {
		if !slices.ContainsFunc(slices.Collect(maps.Values(seen)), func(els [][2]string) bool {
			return slices.ContainsFunc(els, func(e [2]string) bool { return e[1] == k })
		}) {
			t.Errorf("no page draws status-%s", k)
		}
	}
}

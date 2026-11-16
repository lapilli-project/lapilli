package serve

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// statusStates gives s, an a11yServer, the states its pages don't show
// already, so that every status kind is drawn somewhere: a quiet window in
// effect now (it quiets no rule there is), tasks overdue, doing, done and
// dropped, the waiting proposal made a task, a rule to keep (Limited), a
// run that failed, and a proposal in each state it can be in: approved,
// checked again (ready), its pull request open, merged, failed (with why),
// refused, dropped (a for one event can't outlast), changed in review (its
// backtest with no record), expired, undone, stale (backtested by a newer
// lapilli, so that it is left for a person), and one to delete that would
// have caught an incident while its rule was off. The pages that show
// them, and those pages by name (@task:overdue, @proposal:failed …), for
// TestStatusKinds and test/a11y/axe.sh (TestA11yServe).
func statusStates(t *testing.T, s *Server) ([]string, map[string]string) {
	t.Helper()
	ctx := context.Background()
	named := map[string]string{}
	if _, err := s.l.PutWindow(ctx, ledger.Window{Title: "now", Start: now.Add(-time.Hour), End: now.Add(time.Hour), Rules: "^NoRuleIsCalledThis$"},
		"ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		name string
		task ledger.Task
	}{
		{"overdue", ledger.Task{Title: "late", Kind: "tune", Rule: "Errors", Due: now.Add(-72 * time.Hour)}},
		{"doing", ledger.Task{Title: "under way", Kind: "decide", Rule: "Noise", Status: "doing"}},
		{"done", ledger.Task{Title: "finished", Kind: "write", Rule: "Noise", Status: "done"}},
		{"dropped", ledger.Task{Title: "let go", Kind: "write", Rule: "Noise", Status: "dropped"}},
		{"proposal", ledger.Task{Title: "approve #1", Kind: "decide", Rule: "Errors"}},
	} {
		id, err := s.l.PutTask(ctx, x.task, "ana")
		if err != nil {
			t.Fatal(err)
		}
		named["@task:"+x.name] = "/tasks/" + strconv.FormatInt(id, 10)
		if x.name == "proposal" {
			if err := s.l.SetProposalTask(ctx, 1, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "Limited", Status: "keep"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := s.l.AddRun(ctx, ledger.Run{Kind: "hourly", Started: now.Add(-2 * time.Hour), Finished: now.Add(-2 * time.Hour),
		From: now.Add(-3 * time.Hour), To: now.Add(-2 * time.Hour), Error: "prometheus: connection refused"}); err != nil {
		t.Fatal(err)
	}
	five := 5 * time.Minute
	bt := func() *backtest {
		return &backtest{From: now.Add(-weekLen), To: now, Before: side{Firings: 9, White: 1}, After: side{Firings: 2, White: 1}}
	}
	yes := true
	noRecord := bt()
	noRecord.NoRecord = &yes
	reasons := map[string]string{ledger.StateFailed: "grafana: 500", ledger.StateRefused: "the rule changed since"}
	for _, x := range []struct {
		name, rule, kind, path string
		ev                     evidence
		moves                  []string // the states it moves through, from proposed
	}{
		{name: "approved", rule: "Flapping", moves: []string{ledger.StateApproved}},
		{name: "failed", rule: "Limited", moves: []string{ledger.StateApproved, ledger.StateFailed}},
		{name: "refused", rule: "Limited", moves: []string{ledger.StateApproved, ledger.StateRefused}},
		{name: "stale", rule: "Limited", ev: evidence{Semantics: semantics + 1, Version: "9.9.9"}},
		{name: "ready", rule: "Readied", moves: []string{ledger.StateApproved, ledger.StateReady}},
		{name: "opened", rule: "Opened", path: ledger.PathGit, moves: []string{ledger.StateApproved, ledger.StateOpened}},
		{name: "merged", rule: "Merged", path: ledger.PathGit, moves: []string{ledger.StateApproved, ledger.StateOpened, ledger.StateMerged}},
		{name: "dropped", rule: "Dropped", ev: evidence{Outlasts: &verdict.Outlast{For: 15 * time.Minute, Window: 10 * time.Minute}},
			moves: []string{ledger.StateDropped}},
		{name: "amended", rule: "Amended", path: ledger.PathGit, ev: evidence{Backtest: noRecord},
			moves: []string{ledger.StateApproved, ledger.StateOpened, ledger.StateMerged, ledger.StateAmended}},
		{name: "expired", rule: "Expired", moves: []string{ledger.StateExpired}},
		{name: "undone", rule: "Undone", moves: []string{ledger.StateApproved, ledger.StateApplying, ledger.StateApplied, ledger.StateUndone}},
		{name: "off", rule: "Retired", kind: ledger.KindDelete, ev: evidence{Record: &ruleRecord{Weeks: 4, Would: []string{"INC-1"}}}},
	} {
		ev := x.ev
		kind, path := cmpOr(x.kind, ledger.KindFor), cmpOr(x.path, ledger.PathGrafana)
		if kind == ledger.KindFor {
			ev.Edit = &rules.Edit{Name: x.rule, For: &five}
			if ev.Backtest == nil {
				ev.Backtest = bt()
			}
			if ev.Semantics == 0 {
				ev.Semantics = semantics
			}
		}
		b, _ := json.Marshal(ev)
		id, err := s.l.Propose(ctx, ledger.Proposal{Rule: x.rule, Kind: kind, Path: path, MadeBy: "bo",
			Before: x.rule + " ()\nfor: 0s\n", After: x.rule + " ()\nfor: 5m\n", Evidence: b})
		if err != nil {
			t.Fatal(err)
		}
		from := ledger.StateProposed
		for _, to := range x.moves {
			if _, err := s.l.Move(ctx, id, []string{from}, to, ledger.Proposer, ledger.Step{Reason: reasons[to]}); err != nil {
				t.Fatal(err)
			}
			from = to
		}
		named["@proposal:"+x.name] = "/proposals/" + strconv.FormatInt(id, 10)
	}
	paths := []string{"/windows", "/tasks", "/tasks?status=all", "/runs", "/proposals?state=all"}
	for _, p := range named {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return slices.Compact(paths), named
}

// statusSites are the deployments TestStatusKinds renders, each under a
// prefix, beside a11yServer's pages (with statusStates) and the first-time
// user's (/first): with nothing judged yet (/none); with no record
// (/facet, facetServerOf without its incident); one rule that caught the
// one incident, nothing to look at (/clear); an incident only a noisy rule
// caught (/noisy); rule changes predicted and measured, the runs up to
// date (/outcomes); notices sent, failed and given up on (/notify); and
// changes on, paused (/paused).
func statusSites(t *testing.T) map[string]statusSite {
	t.Helper()
	ctx := context.Background()
	sites := map[string]statusSite{}
	nothing, _ := server(t, nil)
	sites["/none"] = statusSite{nothing, []string{"/", "/outcomes", "/rules", "/proposals"}}
	sites["/facet"] = statusSite{facetServerOf(t, false), []string{"/", "/outcomes", "/rules?weeks=4", "/rules/R06?weeks=4"}}

	judged := func(fs []firing.Firing) *Server {
		s, _ := server(t, nil)
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		if err := s.syncIncidents(ctx, s.cfg.Options); err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
		if err := s.l.Record(ctx, ledger.Found{From: at, To: at.Add(12 * time.Hour), Firings: fs, Run: ledger.Run{Kind: "backfill", Started: at, Finished: at}},
			s.cfg.Options); err != nil {
			t.Fatal(err)
		}
		if err := s.l.JudgePending(ctx, s.cfg.Options); err != nil {
			t.Fatal(err)
		}
		return s
	}
	inc := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC) // INC-1, on checkout
	fire := func(at time.Time) firing.Firing {
		return firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at, End: at.Add(5 * time.Minute)}
	}
	sites["/clear"] = statusSite{judged([]firing.Firing{fire(inc)}), []string{"/", "/incidents", "/proposals"}}
	noisy := []firing.Firing{fire(inc)}
	for h := range 12 {
		noisy = append(noisy, fire(inc.Add(time.Duration(h+1)*30*time.Minute)))
	}
	sites["/noisy"] = statusSite{judged(noisy), []string{"/incidents"}}

	o, _ := server(t, func(c *Config) { c.UserHeader = "X-Forwarded-Email" })
	today := time.Now().UTC().Truncate(24 * time.Hour)
	keepOutcomes(t, o, today)
	o.now = func() time.Time { return today.Add(30 * time.Minute) } // the runs up to date
	sites["/outcomes"] = statusSite{o, []string{"/outcomes"}}

	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("https://hooks.example.com/x"), 0o600)
	n, _ := server(t, func(c *Config) { c.NotifyWebhook, c.ReportDests = hook, map[string]string{"ops": hook} })
	for i := range 4 {
		if _, err := n.l.PutTask(ctx, ledger.Task{Title: "t" + strconv.Itoa(i), Kind: "tune"}, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	due, err := n.l.Due(ctx, "webhook", time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), 10) // whenever they were made
	if err != nil || len(due) != 4 {
		t.Fatalf("%d due: %v", len(due), err)
	}
	if err := n.l.Sent(ctx, due[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := n.l.Failed(ctx, due[1].ID, "503"); err != nil {
		t.Fatal(err)
	}
	for range ledger.GiveUpAfter {
		if err := n.l.Failed(ctx, due[2].ID, "503"); err != nil {
			t.Fatal(err)
		}
	}
	sites["/notify"] = statusSite{n, []string{"/manage", "/settings"}}

	p, _ := changesServer(t, nil)
	if err := p.l.SetSwitch(ctx, pausedKey, true, ana, "changes to the rules paused"); err != nil {
		t.Fatal(err)
	}
	p.changes.paused.Store(true)
	sites["/paused"] = statusSite{p, []string{"/", "/proposals", "/settings"}}
	for prefix, src := range sourcesStates(t) {
		sites[prefix] = statusSite{src, []string{"/settings"}}
	}
	return sites
}

type statusSite struct {
	s     *Server
	paths []string
}

// statusWant is the status kind of each state the pages draw, as
// docs/design/ui.md's "Status colours" maps it: the words that draw it
// (a badge's with the detail beside it), in English; its kind, or "none"
// for a state said in no colour; and the pages it is on, a page of
// statusStates by its name. On those pages each element that says it,
// the smallest, is of that kind by its own class or one around it, and
// there is one; on every page, an element coloured that says it is of that
// kind.
var statusWant = []struct {
	text *regexp.Regexp
	kind string
	on   []string
}{
	// ok
	{regexp.MustCompile(`^○ OK$`), "ok", []string{"/rules/Errors"}},
	{regexp.MustCompile(`^✓ approved`), "ok", []string{"/proposals?state=all", "@proposal:approved"}},
	{regexp.MustCompile(`^● applied`), "ok", []string{"/proposals?state=all"}},
	{regexp.MustCompile(`^done$`), "ok", []string{"/tasks?status=all", "@task:done"}},
	{regexp.MustCompile(`^✓ keep$`), "ok", []string{"/rules/Limited"}},
	{regexp.MustCompile(`^○ [A-Za-z]+ [+-]\d+m$`), "ok", []string{"/incidents", "/clear/incidents"}},
	{regexp.MustCompile(`^all ran$`), "ok", []string{"/outcomes/outcomes"}},
	{regexp.MustCompile(`^✓ Last sent from the outbox`), "ok", []string{"/notify/manage", "/notify/settings"}},
	{regexp.MustCompile(`^✓ Nothing to look at now`), "ok", []string{"/clear/"}},
	{regexp.MustCompile(`^as predicted$`), "ok", []string{"/outcomes/outcomes"}},
	{regexp.MustCompile(`, better$| better$`), "ok", []string{"/proposals/2"}},
	// warn
	{regexp.MustCompile(`^● Noisy`), "warn", []string{"/rules"}},
	{regexp.MustCompile(`^≠ Differs$`), "warn", []string{"/rules", "/rules/Errors"}},
	{regexp.MustCompile(`^◌ Not replayed$`), "warn", []string{"/rules"}},
	{regexp.MustCompile(`^⚠\x{FE0E} \d+ to check$`), "warn", []string{"/rules/Errors"}},
	{regexp.MustCompile(`^◷ awaiting approval`), "warn", []string{"/proposals", "/proposals/1", "@task:proposal"}},
	{regexp.MustCompile(`^⟳ stale: backtest it again$`), "warn", []string{"/proposals"}},
	{regexp.MustCompile(`^⟳ It is stale`), "warn", []string{"@proposal:stale"}},
	{regexp.MustCompile(`^⚙\x{FE0E} (an agent|proposed by an agent)$`), "warn", []string{"/proposals/2", "/proposals?state=all"}},
	{regexp.MustCompile(`^⚠\x{FE0E} An agent proposed this`), "warn", []string{"/proposals/2"}},
	{regexp.MustCompile(`one event alone (can't|could never) fire it`), "warn", []string{"@proposal:dropped", "/proposals?state=all"}},
	{regexp.MustCompile(`^⚠\x{FE0E} Changes (to the rules )?are paused|On, and paused`), "warn", []string{"/paused/", "/paused/proposals", "/paused/settings"}},
	{regexp.MustCompile(`^◐ [A-Za-z]+ [+-]\d+m, noisy$`), "warn", []string{"/noisy/incidents"}},
	{regexp.MustCompile(`^⚠\x{FE0E} Some of what the (config )?file declares can't be kept`), "warn", []string{"/settings", "/runs", "/manage"}},
	{regexp.MustCompile(`were given back to the UI`), "warn", []string{"/settings"}},
	{regexp.MustCompile(`its team was removed`), "warn", []string{"/reports"}},
	{regexp.MustCompile(`nothing names .* now$`), "warn", []string{"/reports"}},
	{regexp.MustCompile(`^⚠\x{FE0E} Their team was removed`), "warn", []string{"/runs"}},
	{regexp.MustCompile(`held nothing over`), "warn", []string{"/runs"}},
	{regexp.MustCompile(`No one (can|may) approve`), "warn", []string{"/first/proposals/1"}},
	{regexp.MustCompile(`goes nowhere|nowhere to go`), "warn", []string{"/first/manage", "/first/settings"}},
	{regexp.MustCompile(`notices? failed in the last day|wasn't sent: its last try failed`), "warn", []string{"/notify/manage", "/notify/settings"}},
	{regexp.MustCompile(`No incident written down has started|are they still being written down`), "warn", []string{"/facet/", "/facet/outcomes"}},
	{regexp.MustCompile(`^⊘ closed \(expired\)$`), "warn", []string{"/proposals?state=all", "@proposal:expired"}},
	{regexp.MustCompile(`^not as predicted$|^no longer replayed$`), "warn", []string{"/outcomes/outcomes"}},
	// bad
	{regexp.MustCompile(`^⊘ closed \(failed\)$`), "bad", []string{"/proposals?state=all", "@proposal:failed"}},
	{regexp.MustCompile(`^✕ grafana: 500$`), "bad", []string{"@proposal:failed"}},
	{regexp.MustCompile(`^✕ prometheus: connection refused$`), "bad", []string{"/runs"}},
	{regexp.MustCompile(`^✕ The last read failed`), "bad", []string{"/runs"}},
	{regexp.MustCompile(`^✗ nothing fired$|^✗ Nothing fired\.$`), "bad", []string{"/incidents"}},
	{regexp.MustCompile(`overdue$`), "bad", []string{"/tasks", "@task:overdue", "/"}},
	{regexp.MustCompile(`^\d+ overdue$`), "bad", []string{"/outcomes"}},
	{regexp.MustCompile(`^\d+ failed$`), "bad", []string{"/outcomes"}},
	{regexp.MustCompile(`given up on`), "bad", []string{"/notify/manage", "/notify/settings"}},
	{regexp.MustCompile(`^✕ Left out, as it isn't understood`), "bad", []string{"/rules?perf=ok&severity=none&trust=differs&q=i+label%3Aservice%3Dsearch+group%3Ashop+foo%3Abar"}},
	{regexp.MustCompile(`^✕ As proposed, it misses|^✕ Undone, it misses`), "bad", []string{"/proposals/3"}},
	{regexp.MustCompile(`^✕ While it was off`), "bad", []string{"@proposal:off"}},
	{regexp.MustCompile(` worse$|, worse$`), "bad", []string{"/proposals/2"}},
	{regexp.MustCompile(`^✕ a run: boom$`), "bad", []string{"/"}},
	// info
	{regexp.MustCompile(`^● active now$`), "info", []string{"/windows"}},
	{regexp.MustCompile(`^✎ Proposed$`), "info", []string{"/rules/Errors"}},
	{regexp.MustCompile(`^⇣ Recorded only$`), "info", []string{"/rules"}},
	{regexp.MustCompile(`^(open|doing)$`), "info", []string{"/tasks?status=all", "@task:doing"}},
	{regexp.MustCompile(`^task #\d+$`), "info", []string{"/"}},
	{regexp.MustCompile(`^± tune$`), "info", []string{"/rules/Errors"}},
	{regexp.MustCompile(`^↻ being applied`), "info", []string{"/proposals?state=all", "@proposal:ready", "@proposal:opened", "@proposal:merged"}},
	{regexp.MustCompile(`^↻ it stopped firing`), "info", []string{"/proposals/2"}},
	{regexp.MustCompile(`^↻ replaying`), "info", []string{"/"}},
	{regexp.MustCompile(`^↻ Catching up|behind: firings are in up to`), "info", []string{"/facet/", "/facet/outcomes"}},
	{regexp.MustCompile(`^measuring|^rule written$`), "info", []string{"/outcomes/outcomes"}},
	// unknown
	{regexp.MustCompile(`^\? No record$`), "unknown", []string{"/rules", "/facet/rules?weeks=4"}},
	{regexp.MustCompile(`^– Silent$`), "unknown", []string{"/rules?perf=silent&weeks=12"}},
	{regexp.MustCompile(`^⧉ Spare$`), "unknown", []string{"/rules"}},
	{regexp.MustCompile(`^no record$`), "unknown", []string{"/"}},
	{regexp.MustCompile(`^declared$`), "unknown", []string{"/windows", "/reports"}},
	{regexp.MustCompile(`^⊘ retire$`), "unknown", []string{"/rules"}},
	{regexp.MustCompile(`^↶ undone`), "unknown", []string{"/proposals?state=all", "@proposal:undone"}},
	{regexp.MustCompile(`^⊘ closed \((refused|dropped|changed in review)\)$`), "unknown",
		[]string{"/proposals?state=all", "@proposal:refused", "@proposal:dropped", "@proposal:amended"}},
	{regexp.MustCompile(`^\? the rule changed since$`), "unknown", []string{"@proposal:refused"}},
	{regexp.MustCompile(`^\? No incident was written down`), "unknown", []string{"/facet/rules?weeks=4", "@proposal:amended"}},
	{regexp.MustCompile(`^dropped$`), "unknown", []string{"/tasks?status=all", "@task:dropped"}},
	{regexp.MustCompile(`^rule taken away$`), "unknown", []string{"/outcomes/outcomes"}},
	// Where incidents come from (sources.go).
	{regexp.MustCompile(`^✓ working$`), "ok", []string{"/sources/settings"}},
	{regexp.MustCompile(`^⚠\x{FE0E} quiet$`), "warn", []string{"/sources/settings"}},
	{regexp.MustCompile(`^⚠\x{FE0E} no data$`), "warn", []string{"/nodata/settings"}},
	{regexp.MustCompile(`keeps no record: a selector it reads has no data yet`), "warn", []string{"/nodata/settings"}},
	{regexp.MustCompile(`^✕ failing$`), "bad", []string{"/sources/settings"}},
	{regexp.MustCompile(`^↻ waiting$`), "info", []string{"/intake/settings"}},
	{regexp.MustCompile(`^\? not set up$`), "unknown", []string{"/settings"}},
	{regexp.MustCompile(`^– none in four weeks$`), "unknown", []string{"/intake/settings"}},
	// said in no colour: nothing is left to do, but nothing is well either
	{regexp.MustCompile(`^No proposals yet\.$`), "none", []string{"/none/proposals", "/clear/proposals"}},
}

// statusAlso are the kinds a state of statusWant is drawn in elsewhere,
// by what its words don't say: "○ OK" of a rule none of whose firings was
// judged is unknown (TestStatusKinds holds such rules to it); "all ran",
// behind, is under way (TestOutcomesRunsKind).
var statusAlso = map[string]string{`^○ OK$`: "unknown", `^all ran$`: "info"}

// statusClassOf is the status kind an element is drawn in, by its own
// class or the nearest around it that has one; "none" for none.
func statusClassOf(n *html.Node) string {
	for ; n != nil; n = n.Parent {
		for _, c := range strings.Fields(nodeAttr(n, "class")) {
			if k, ok := strings.CutPrefix(c, "status-"); ok {
				return k
			}
		}
	}
	return "none"
}

// saidText is what an element says, for statusWant: its text, and a
// badge's with the detail beside it ("⊘ closed (failed)").
func saidText(n *html.Node) string {
	text := squashed(nodeText(n))
	if !slices.Contains(strings.Fields(nodeAttr(n, "class")), "badge") {
		return text
	}
	next := n.NextSibling
	for next != nil && next.Type == html.TextNode && strings.TrimSpace(next.Data) == "" {
		next = next.NextSibling
	}
	if next != nil && nodeAttr(next, "class") == "quiet" && strings.HasPrefix(nodeText(next), "(") {
		text += " " + squashed(nodeText(next))
	}
	return text
}

// shownEls are the elements of a page a reader reads as a state: all but a
// list's facets (a filter, not a state), the options of a select, a
// chart's drawing, and the head.
func shownEls(doc *html.Node) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			cls := strings.Fields(nodeAttr(c, "class"))
			if c.DataAtom == atom.Head || c.DataAtom == atom.Select || c.DataAtom == atom.Svg ||
				c.DataAtom == atom.Form && (slices.Contains(cls, "facets") || slices.Contains(cls, "pick")) {
				continue
			}
			out = append(out, c)
			walk(c)
		}
	}
	walk(doc)
	return out
}

// sayingEls are the smallest elements of els that say what re matches: none
// within them does.
func sayingEls(els []*html.Node, re *regexp.Regexp) []*html.Node {
	var hit []*html.Node
	for _, n := range els {
		if re.MatchString(saidText(n)) {
			hit = append(hit, n)
		}
	}
	var out []*html.Node
	for _, n := range hit {
		if !slices.ContainsFunc(hit, func(m *html.Node) bool { return m != n && nodeInside(m, n) }) {
			out = append(out, n)
		}
	}
	return out
}

func nodeInside(n, ancestor *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p == ancestor {
			return true
		}
	}
	return false
}

// statusOf is each element a page colours by status: what it says, and
// its kind.
func statusOf(doc *html.Node) [][2]string {
	var out [][2]string
	for _, n := range allNodes(doc) {
		if n.Type != html.ElementNode {
			continue
		}
		for _, c := range strings.Fields(nodeAttr(n, "class")) {
			if k, ok := strings.CutPrefix(c, "status-"); ok {
				out = append(out, [2]string{saidText(n), k})
			}
		}
	}
	return out
}

// TestStatusKinds: each state is of its status kind, as statusWant has it
// (docs/design/ui.md, "Status colours"), wherever it is drawn: on the
// pages statusWant names, every element that says it, found by its words,
// is drawn in its kind, so that a class left out fails as one of another
// kind does; on every page, every element coloured that says it is of its
// kind. Every kind is one of the five, and drawn. What Lapilli can't judge
// is never ok or bad: "–" or no record coloured so, or a rule none of
// whose firings was judged (NoRecord) with an ok or bad badge, whatever
// it says. Every notice starts with its sign.
func TestStatusKinds(t *testing.T) {
	s, paths := a11yServer(t)
	more, named := statusStates(t, s)
	paths = append(paths, more...)
	first, firstPaths := firstUseServer(t)
	sites := statusSites(t)
	sites[""] = statusSite{s, paths}
	sites["/first"] = statusSite{first, firstPaths}
	docs := map[string]*html.Node{}
	for prefix, x := range sites {
		h := x.s.Handler()
		for _, p := range x.paths {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Sec-Fetch-Site", "none") // a page asked for in a browser, as axe does
			r.Header.Set("Accept-Language", "en")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK && !strings.HasSuffix(p, "/none") && !strings.HasSuffix(p, "/Nothing") {
				t.Errorf("%s%s: %d", prefix, p, w.Code)
			}
			doc, err := html.Parse(w.Body)
			if err != nil {
				t.Fatal(err)
			}
			docs[prefix+p] = doc
		}
	}
	page := func(p string) string {
		if n, ok := named[p]; ok {
			return n
		}
		return p
	}

	// On the pages named, found by its words.
	for _, w := range statusWant {
		if len(w.on) == 0 {
			t.Errorf("%v: no page to find it on", w.text)
		}
		for _, p := range w.on {
			doc, ok := docs[page(p)]
			if !ok {
				t.Errorf("%v: %s isn't among the pages rendered", w.text, p)
				continue
			}
			said := sayingEls(shownEls(doc), w.text)
			if len(said) == 0 {
				t.Errorf("%s (%s) doesn't say %v", p, page(p), w.text)
			}
			for _, n := range said {
				if k := statusClassOf(n); k != w.kind {
					t.Errorf("%s (%s): %q is drawn %s, not %s", p, page(p), saidText(n), k, w.kind)
				}
			}
		}
	}

	unjudged := regexp.MustCompile(`^–$|not known|[Nn]o record|no incident (was )?written down`)
	drawn := map[string]bool{}
	for p, doc := range docs {
		for _, e := range statusOf(doc) {
			drawn[e[1]] = true
			if !slices.Contains(statusKindNames, e[1]) {
				t.Errorf("%s: %q is status-%s, not a kind", p, e[0], e[1])
			}
			if (e[1] == "ok" || e[1] == "bad") && unjudged.MatchString(e[0]) {
				t.Errorf("%s: %q, which Lapilli can't judge, is %s", p, e[0], e[1])
			}
			for _, w := range statusWant {
				if w.text.MatchString(e[0]) && e[1] != w.kind && e[1] != statusAlso[w.text.String()] {
					t.Errorf("%s: %q is %s, not %s", p, e[0], e[1], w.kind)
				}
			}
		}
		// Every notice starts with its sign, for the eye: a box of
		// paragraphs, its first.
		for _, n := range allNodes(doc) {
			if !slices.Contains(strings.Fields(nodeAttr(n, "class")), "notice") {
				continue
			}
			start := n
			if n.DataAtom == atom.Div {
				start = firstElement(n)
			}
			if c := firstElement(start); c == nil || nodeAttr(c, "class") != "sign" || strings.TrimSpace(nodeText(c)) == "" {
				t.Errorf("%s: a notice without its sign first: %q", p, squashed(nodeText(n)))
			}
		}
	}
	for _, k := range statusKindNames {
		if !drawn[k] {
			t.Errorf("no page draws status-%s", k)
		}
	}

	// A rule none of whose firings was judged is neither ok nor bad, on the
	// list and its page, whatever its badge says.
	for _, x := range []struct {
		site  string
		s     *Server
		query string
	}{{"", s, "weeks=4"}, {"/facet", sites["/facet"].s, "weeks=4"}} {
		v := rulesFor(t, x.s, x.query)
		var none []string
		for _, r := range v.Rows {
			if r.NoRecord {
				none = append(none, r.Name)
			}
		}
		if x.site == "/facet" && len(none) == 0 {
			t.Fatal("/facet: no rule with nothing judged")
		}
		h := x.s.Handler()
		for _, name := range none {
			for _, p := range []string{"/rules?" + x.query, "/rules/" + name + "?" + x.query} {
				_, body, _ := get(t, h, p)
				doc, err := html.Parse(strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				badges := perfBadges(doc, name)
				if len(badges) == 0 {
					t.Errorf("%s%s: no badge of how %s did", x.site, p, name)
				}
				for _, b := range badges {
					if k := statusClassOf(b); k != "unknown" {
						t.Errorf("%s%s: %s, none of whose firings was judged, is %q in %s", x.site, p, name, saidText(b), k)
					}
				}
			}
		}
	}
}

// firstElement is n's first child element, or nil.
func firstElement(n *html.Node) *html.Node {
	if n == nil {
		return nil
	}
	c := n.FirstChild
	for c != nil && c.Type != html.ElementNode {
		c = c.NextSibling
	}
	return c
}

// perfBadges are the badges of how a rule did, on the rules list's row of
// it or its page's header.
func perfBadges(doc *html.Node, rule string) []*html.Node {
	var out []*html.Node
	for _, n := range allNodes(doc) {
		holds := n.DataAtom == atom.Td && slices.Contains(strings.Fields(nodeAttr(n, "class")), "rule") ||
			n.DataAtom == atom.Div && nodeAttr(n, "class") == "rule-name"
		if !holds || !slices.ContainsFunc(allNodes(n), func(c *html.Node) bool {
			return (c.DataAtom == atom.A || c.DataAtom == atom.H1) && squashed(nodeText(c)) == rule
		}) {
			continue
		}
		for _, c := range allNodes(n) {
			if slices.ContainsFunc(strings.Fields(nodeAttr(c, "class")), func(x string) bool { return strings.HasPrefix(x, "perf-") }) {
				out = append(out, c)
			}
		}
	}
	return out
}

// Outcomes' Runs tile is ok only when the runs ran, none
// failed, and they are up to date: behind, it is under way (info); with a
// run that failed, bad.
func TestOutcomesRunsKind(t *testing.T) {
	ctx := context.Background()
	runs := func(now time.Time, failed bool) string {
		s, _ := server(t, nil)
		at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
		if err := s.l.Record(ctx, ledger.Found{From: at, To: at.Add(12 * time.Hour), Run: ledger.Run{Kind: "backfill", Started: at, Finished: at}},
			s.cfg.Options); err != nil {
			t.Fatal(err)
		}
		if failed {
			if err := s.l.AddRun(ctx, ledger.Run{Kind: "hourly", Started: at, Finished: at, From: at, To: at, Error: "boom"}); err != nil {
				t.Fatal(err)
			}
		}
		s.now = func() time.Time { return now }
		_, body, _ := get(t, s.Handler(), "/outcomes")
		doc, err := html.Parse(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range allNodes(doc) {
			if slices.Contains(strings.Fields(nodeAttr(n, "class")), "tile-value") && nodeWithin(n, atom.Section) {
				if txt := squashed(nodeText(n)); txt == "all ran" || strings.HasSuffix(txt, "failed") {
					return txt + " " + statusClassOf(n)
				}
			}
		}
		return ""
	}
	upTo := time.Date(2026, 11, 2, 12, 30, 0, 0, time.UTC) // half an hour after the firings are in
	for _, c := range []struct {
		now    time.Time
		failed bool
		want   string
	}{
		{upTo, false, "all ran ok"},
		{upTo.Add(24 * time.Hour), false, "all ran info"},
		{upTo, true, "1 failed bad"},
	} {
		if got := runs(c.now, c.failed); got != c.want {
			t.Errorf("at %s, a run failed %v: %q, not %q", c.now, c.failed, got, c.want)
		}
	}
}

// The incidents chart's fills, caught, only noisy rules and missed, are
// told apart by those who see colours otherwise: each pair 8 or more apart
// (ΔE in OKLab, ×100) under protanopia and deuteranopia, as Machado,
// Oliveira and Fernandes (2009) simulate them at full severity, and caught
// and missed apart in lightness too, in light and dark.
func TestChartFillsApart(t *testing.T) {
	b, err := files.ReadFile("static/lapilli.css")
	if err != nil {
		t.Fatal(err)
	}
	light, dark := cssTokens(string(b))
	sim := map[string][3][3]float64{
		"protanopia":   {{0.152286, 1.052583, -0.204868}, {0.114503, 0.786281, 0.099216}, {-0.003882, -0.048116, 1.051998}},
		"deuteranopia": {{0.367322, 0.860646, -0.227968}, {0.280085, 0.672501, 0.047413}, {-0.011820, 0.042940, 0.968881}},
	}
	lab := func(hex string, m *[3][3]float64) [3]float64 {
		c, err := cssRGBA(hex)
		if err != nil {
			t.Fatal(err)
		}
		lin := func(x float64) float64 {
			if x <= 0.04045 {
				return x / 12.92
			}
			return math.Pow((x+0.055)/1.055, 2.4)
		}
		r, g, bl := lin(c[0]), lin(c[1]), lin(c[2])
		if m != nil {
			cl := func(x float64) float64 { return max(0, min(1, x)) }
			r, g, bl = cl(m[0][0]*r+m[0][1]*g+m[0][2]*bl), cl(m[1][0]*r+m[1][1]*g+m[1][2]*bl), cl(m[2][0]*r+m[2][1]*g+m[2][2]*bl)
		}
		l := math.Cbrt(0.4122214708*r + 0.5363325363*g + 0.0514459929*bl)
		mm := math.Cbrt(0.2119034982*r + 0.6806995451*g + 0.1073969566*bl)
		s := math.Cbrt(0.0883024619*r + 0.2817188376*g + 0.6299787005*bl)
		return [3]float64{0.2104542553*l + 0.7936177850*mm - 0.0040720468*s, 1.9779984951*l - 2.4285922050*mm + 0.4505937099*s,
			0.0259040371*l + 0.7827717662*mm - 0.8086757660*s}
	}
	fills := []string{"viz-good", "viz-warn", "viz-bad"}
	for mode, tokens := range map[string]map[string]string{"light": light, "dark": dark} {
		for name, m := range sim {
			for i, a := range fills {
				for _, c := range fills[i+1:] {
					x, y := lab(tokens[a], &m), lab(tokens[c], &m)
					if d := 100 * math.Hypot(math.Hypot(x[0]-y[0], x[1]-y[1]), x[2]-y[2]); d < 8 {
						t.Errorf("%s, %s: --%s and --%s are %.1f apart, under 8", mode, name, a, c, d)
					}
				}
			}
		}
		if d := math.Abs(lab(tokens["viz-good"], nil)[0] - lab(tokens["viz-bad"], nil)[0]); d < 0.05 {
			t.Errorf("%s: caught and missed are %.3f apart in lightness, under 0.05", mode, d)
		}
	}
}

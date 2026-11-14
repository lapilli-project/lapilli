package serve

// A rule's page (docs/design/ui.md): its tabs, what to check, its panel
// and its strip, and the links that went to its sections before they were
// tabs.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// TestRuleTabOf: tab= is one of the five, or the overview: a value it
// doesn't have, in any case, is the overview, and the page doesn't say it.
func TestRuleTabOf(t *testing.T) {
	for q, want := range map[string]string{"": "overview", "tab=": "overview", "tab=firings": "firings", "tab=versions": "versions",
		"tab=proposals": "proposals", "tab=activity": "activity", "tab=overview": "overview", "tab=Firings": "overview",
		"tab=firings+": "overview", "tab=x&tab=firings": "overview", "tab=firings&tab=x": "firings", "tab=%3Cscript%3E": "overview"} {
		v, _ := url.ParseQuery(q)
		if got := ruleTabOf(v); got != want {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
	}
	s, _ := a11yServer(t)
	h := s.Handler()
	overview := tabsShown(t, h, "/rules/Errors")
	for _, q := range []string{"tab=zzz-unknown-tab", "tab=FIRINGS", "tab=" + url.QueryEscape("<b>x</b>")} {
		code, body, _ := get(t, h, "/rules/Errors?"+q)
		if code != http.StatusOK {
			t.Fatalf("%s: %d", q, code)
		}
		if v, _ := url.ParseQuery(q); strings.Contains(unlinked(body), v.Get("tab")) || strings.Contains(unlinked(body), "zzz") {
			t.Errorf("%s: the page says what it was asked", q)
		}
		if got := tabsShown(t, h, "/rules/Errors?"+q); !slices.Equal(got, overview) {
			t.Errorf("%s: sections %q, not the overview's %q", q, got, overview)
		}
	}
	// Each tab is a link with the page context, the one shown current.
	_, body, _ := get(t, h, "/rules/Errors?tab=versions&team=payments&weeks=4")
	for _, want := range []string{`<a href="/rules/Errors?team=payments&amp;weeks=4">Overview</a>`,
		`<a href="/rules/Errors?tab=firings&amp;team=payments&amp;weeks=4">Firings <span class="count">`,
		`<a href="/rules/Errors?tab=versions&amp;team=payments&amp;weeks=4" aria-current="page">Versions <span class="count">1</span></a>`,
		`<a href="/rules/Errors?tab=proposals&amp;team=payments&amp;weeks=4">Proposals <span class="count">1</span></a>`,
		`<a href="/rules/Errors?tab=activity&amp;team=payments&amp;weeks=4">Activity <span class="count">2</span></a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("tabs: no %s", want)
		}
	}
	if n := strings.Count(body, `aria-current="page"`); n != 2 { // the breadcrumb's, and the tab's
		t.Errorf("%d aria-current=page", n)
	}
}

// unlinked is a page but its languages' links, which send the URL asked
// back to it, escaped.
func unlinked(body string) string {
	return regexp.MustCompile(`href="/lang\?[^"]*"`).ReplaceAllString(body, "")
}

// tabsShown are the headings of the sections a rule's page shows.
func tabsShown(t *testing.T, h http.Handler, path string) []string {
	t.Helper()
	_, body, _ := get(t, h, path)
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range allNodes(doc) {
		if n.DataAtom == atom.H2 || n.DataAtom == atom.H3 {
			out = append(out, squashed(nodeText(n)))
		}
	}
	return out
}

// oldTexts are the texts the rule's page of v0.35.0 showed, as its
// template wrote them (git show 5f894fd:internal/serve/templates/rule.html),
// and what each is written as now, where it is reworded: "Over twelve
// weeks" left out, as the page says its period once, at its top.
var oldTexts = map[string]string{
	"%d of the last %d whole weeks had one.": "", "%d times": "",
	"A cloud's alarm, read with --alarms, that Lapilli doesn't replay: it has no record, since no history is tied to it.":           "",
	"A cloud's alarm, read with --alarms: its history, copied in under %s from %s on, is its record, as Lapilli doesn't replay it.": "",
	"A firing re-fired when it started within %s of the same alert last resolving.":                                                 "", "About this rule": "",
	"Add a task about this rule": "", "All its firings, by day": "",
	"Another rule caught every one of them as soon, at the same severity or above: taken away, none would have been missed or caught later.": "",
	"As the rules read last have it, when it fires. It was written by whoever writes the rules: shown as it is, never run or followed.":      "",
	"before the ledger began": "", "By receiver: %s.": "", "by the teams' patterns": "", "Change it": "", "Changes reviewed": "",
	"Description": "", "End": "", "excused": "", "Expression": "", "fired %d → %d, caught %d → %d, precision %s → %s": "", "Fired": "",
	"firing": "", "First caught by": "", "For": "", "from what it fired": "", "From": "", "Guesses, which may be wrong:": "",
	"Incident": "", "Incidents it caught": "",
	"It caught %d incidents: %d before any other rule, %d that no other rule caught (a noisy rule's catch doesn't count beside another's).": "",
	"Its alerts that re-fired most": "", "Its notification templates weren't checked: %s.": "", "kept %s": "", "Labels": "",
	"Latest firings": "", "No notification was sent for it over twelve weeks.": "No notification was sent for it.",
	"No team owns it.": "", "Noisy, %d weeks running.": `template "badges"`, "Noisy.": `template "badges"`,
	"None of its firings re-fired in twelve weeks.": "None of its firings re-fired.", "None.": "", "Not replayed: %s": "", "Note": "",
	"Nothing to fix in what it says, as far as can be told.": "", "Notifications are counted from %s, when the first told of it.": "",
	"Notified": "",
	"Of its firings over twelve weeks, %d came during a deploy of what they are about, %d of them catching something: precision %s with them, %s without.": "Of its firings, %d came during a deploy of what they are about, %d of them catching something: precision %s with them, %s without.",
	"Of the firings that caught nothing, %d came while people slept, and %d more off hours.":                                                               "",
	"Of the repeats, %d were for firings that caught nothing: what repeat_interval cost.":                                                                  "",
	"only in the replay": "", "only in what really fired": "",
	"Over the last week its replay differed from how it really fired: %d of %d firings alike. Its stones rest on that replay.":                                                                              "",
	"Over twelve weeks, %d notifications were sent for it: %d of them repeats, %d while people slept and %d more off hours; %d were for firings that caught nothing, and %d tell of no firing Lapilli has.": "%d notifications were sent for it: %d of them repeats, %d while people slept and %d more off hours; %d were for firings that caught nothing, and %d tell of no firing Lapilli has.",
	"Over twelve weeks, %d of its %d firings re-fired (%s).": "%d of its %d firings re-fired (%s).",
	"Over twelve weeks, beside what really fired: %d%% alike, of %d firings; %d only in the replay, %d only in what really fired; the alike ones started %s apart on average.": "Beside what really fired: %d%% alike, of %d firings; %d only in the replay, %d only in what really fired; the alike ones started %s apart on average.",
	// Its numbers are the panel's, its refires the firings tab's.
	"Over twelve weeks: fired %d times, %d caught something, %d nothing; precision %s, refired %d.": "%d of %d caught something",
	"Owned by %s.": "", "Propose it, for a person to approve": "",
	"Recorded in %s and copied in: Lapilli has what it fired, not its condition, so it can't say what a change to it would do.": "",
	"refired": "", "Refired": "", "Replayed over the week before %s, as it was and as it is:": "", "Runbook": "", "Save": "",
	"Severity %s.": "Severity", "Share": "", "Start": "", "Stone": "", "Summary": "", "taken away": "", "Tasks": "", "Team": "",
	"The last firings found only one way": "", "Tune %s": "", "Versions": "", "Week": "", "What is to become of it": "", "What it says": "",
	"Group %s.": "Group",
	// The partials it drew; its sparkline is the strip.
	`template "comments"`: "", `template "notified"`: "", `template "ruleproposals"`: `define "ruleproposals"`, `template "tasklist"`: "", `template "spark"`: "The last 84 days",
}

// rulePageParts are the template of a rule's page in parts: its header, on
// every tab, and each tab's, the proposals partial in its tab.
func rulePageParts(t *testing.T) map[string]string {
	t.Helper()
	read := func(name string) string {
		b, err := files.ReadFile("templates/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	page, props := read("rule.html"), read("proposals.html")
	props = props[strings.Index(props, `{{define "ruleproposals"}}`):]
	header, rest, ok := strings.Cut(page, `<nav class="ruletabs"`)
	if !ok {
		t.Fatal("rule.html: no tabs")
	}
	parts := map[string]string{"header": header}
	marks := regexp.MustCompile(`\{\{/\* tab: (\w+) \*/\}\}`)
	at := marks.FindAllStringSubmatchIndex(rest, -1)
	for i, m := range at {
		end := len(rest)
		if i+1 < len(at) {
			end = at[i+1][0]
		}
		parts[rest[m[2]:m[3]]] = rest[m[1]:end]
	}
	parts["proposals"] = strings.Replace(parts["proposals"], `{{template "ruleproposals" $}}`, props, 1)
	if len(parts) != len(ruleTabs)+1 {
		t.Fatalf("rule.html: parts %d", len(parts))
	}
	return parts
}

// TestRuleSectionsKept: every text the rule's page showed before its tabs
// is under one of them, or in the header every tab has, none lost; its
// sections are in the tabs they belong in, once; and each tab, rendered,
// shows its own sections alone.
func TestRuleSectionsKept(t *testing.T) {
	parts := rulePageParts(t)
	lit := func(x string) string {
		if strings.HasPrefix(x, "template ") || strings.HasPrefix(x, "define ") {
			return x
		}
		return strconv.Quote(x)
	}
	for old, now := range oldTexts {
		x := cmpOr(now, old)
		var in []string
		for name, part := range parts {
			if strings.Contains(part, lit(x)) {
				in = append(in, name)
			}
		}
		if len(in) == 0 {
			t.Errorf("%q (once %q) is under no tab", x, old)
		}
	}
	// The sections, each in its tab, and in no other.
	for tab, headings := range map[string][]string{
		"overview":  {"What to do first", "About this rule", "Incidents it caught", "What it says"},
		"firings":   {"Latest firings", "Refired", "Notifications", "While people slept, and off hours", "During deploys", "The replay beside what really fired"},
		"versions":  {"Versions", "From %s, beside the version before"},
		"proposals": {"Proposals", "Propose a change", "Changes reviewed"},
		"activity":  {"Tasks", `template "comments"`},
	} {
		for _, h := range headings {
			for name, part := range parts {
				has := strings.Contains(part, "<h2>{{t "+strconv.Quote(h)+"}}</h2>") || strings.Contains(part, ">{{t "+strconv.Quote(h)+"}}</h2>") ||
					strings.Contains(part, "<h3>{{t "+strconv.Quote(h)) || strings.HasPrefix(h, "template ") && strings.Contains(part, "{{"+h)
				if has != (name == tab) {
					t.Errorf("%q in %s: %v", h, name, has)
				}
			}
		}
	}

	// Rendered: a tab's sections are its own.
	s, _ := a11yServer(t)
	notifySome(t, s)
	h := s.Handler()
	for _, tab := range ruleTabs {
		for _, rule := range []string{"Errors", "Flapping"} {
			got := tabsShown(t, h, "/rules/"+rule+"?tab="+tab.id)
			for other, headings := range map[string][]string{"overview": {"About this rule", "Incidents it caught"}, "firings": {"Latest firings"},
				"versions": {"Versions"}, "proposals": {"Changes reviewed"}, "activity": {"Tasks", "Comments"}} {
				for _, x := range headings {
					if slices.Contains(got, x) != (other == tab.id) {
						t.Errorf("/rules/%s?tab=%s: %q shown %v", rule, tab.id, x, slices.Contains(got, x))
					}
				}
			}
		}
	}
	// What the fixture has, each on its tab.
	for path, want := range map[string][]string{
		"/rules/Errors?tab=firings":   {"notifications were sent for it", "Notifications are counted from", "Beside what really fired:", "The last firings found only one way"},
		"/rules/Flapping?tab=firings": {"re-fired (", "Its alerts that re-fired most", "during a deploy of what they are about"},
		"/rules/Errors":               {"It caught 1 incidents", `"for" 2m → 15m`, "What it says has 2 problems of fact."},
		"/rules/Errors?tab=proposals": {"A new for", "Propose deleting it"},
	} {
		_, body, _ := get(t, h, path)
		for _, w := range want {
			if !strings.Contains(html.UnescapeString(body), w) {
				t.Errorf("%s: no %q", path, w)
			}
		}
	}
}

// notifySome gives the a11y server's rule Errors notifications.
func notifySome(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: time.Unix(0, 0), To: now, Rule: "Errors", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range fs {
		if _, err := s.l.Notify(ctx, ledger.Notification{Source: "alertmanager", Receiver: "pager", Group: fmt.Sprint("g", i), At: f.Start.Add(time.Minute),
			Alerts: []ledger.Alert{{Rule: "Errors", Labels: f.Labels, Start: f.Start}}}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRuleOldLinks: a link to where a section of a rule's page was
// (#says, #proposals, #refired) still lands on it, or on the overview's
// pointer to its tab; every link the pages make to a part of a rule's page
// goes to a tab that has it.
func TestRuleOldLinks(t *testing.T) {
	s, paths := a11yServer(t)
	h := s.Handler()
	ids := func(path string) map[string]*html.Node {
		code, body, _ := get(t, h, path)
		if code != http.StatusOK {
			t.Fatalf("%s: %d", path, code)
		}
		doc, err := html.Parse(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*html.Node{}
		for _, n := range allNodes(doc) {
			if id := nodeAttr(n, "id"); id != "" {
				out[id] = n
			}
		}
		return out
	}
	over := ids("/rules/Errors")
	if over["says"] == nil {
		t.Error("/rules/Errors#says: nothing there")
	}
	for id, to := range map[string]string{"proposals": "/rules/Errors?tab=proposals#proposals", "refired": "/rules/Errors?tab=firings#refired"} {
		n := over[id]
		if n == nil || nodeAttr(n, "class") != "moved" {
			t.Errorf("/rules/Errors#%s: no pointer to its tab", id)
			continue
		}
		var href string
		for _, c := range allNodes(n) {
			if c.DataAtom == atom.A {
				href = nodeAttr(c, "href")
			}
		}
		if href != to {
			t.Errorf("/rules/Errors#%s points to %q, not %q", id, href, to)
		}
		path, frag, _ := strings.Cut(to, "#")
		if ids(path)[frag] == nil {
			t.Errorf("%s: no #%s", path, frag)
		}
	}
	// Every link to a part of a rule's page, on every page, finds it.
	frag := regexp.MustCompile(`href="(/rules/[^"#]+)#([^"]+)"`)
	found := 0
	for _, p := range append(slices.Clone(paths), "/rules?weeks=12&perf=silent") {
		_, body, _ := get(t, h, p)
		for _, m := range frag.FindAllStringSubmatch(body, -1) {
			found++
			to := html.UnescapeString(m[1])
			if ids(to)[m[2]] == nil {
				t.Errorf("%s links to %s#%s, which has none", p, to, m[2])
			}
		}
	}
	if found < 20 {
		t.Errorf("only %d links to a part of a rule's page", found)
	}
}

// TestRuleConds: what to check, as the header lists it, in order, each
// with where it is explained; none, and there is no list.
func TestRuleConds(t *testing.T) {
	v := ruleView{ruleLine: ruleLine{Unsupported: "why", Source: "cloud", Differs: true, Content: contentCount{Facts: 2}},
		Unseen: []string{"Replayed in part: x."}, ruleChanges: ruleChanges{Proposals: []proposalLine{
			{Proposal: ledger.Proposal{State: ledger.StateProposed}, Stale: true}, {Proposal: ledger.Proposal{State: ledger.StateMerged}, Stale: true},
			{Proposal: ledger.Proposal{State: ledger.StateApplied}, Stale: true}, {Proposal: ledger.Proposal{State: ledger.StateProposed}}}}}
	var kinds []string
	for _, c := range condsOf(v.condsIn()) {
		kinds = append(kinds, fmt.Sprint(c.Kind, c.N, c.Text))
	}
	if want := []string{"facts2", "differs0", "unreplayed0why", "unseen0Replayed in part: x.", "recorded0cloud", "stale2"}; !slices.Equal(kinds, want) {
		t.Errorf("conds %q, want %q", kinds, want)
	}
	v.Tie = &tieView{Why: "no history"}
	if c := condsOf(v.condsIn()); c[4].Kind != "untied" || c[4].Text != "no history" {
		t.Errorf("an alarm with no history: %+v", c[4])
	}
	v.Tie.Source = "cw"
	if c := condsOf(v.condsIn()); c[4].Kind != "alarm" {
		t.Errorf("an alarm with its history: %+v", c[4])
	}
	if c := condsOf(condsIn{}); len(c) != 0 {
		t.Errorf("nothing to check: %+v", c)
	}

	s, _ := a11yServer(t)
	h := s.Handler()
	_, body, _ := get(t, h, "/rules/Errors")
	for _, want := range []string{`<summary><span aria-hidden="true">⚠&#xFE0E;</span> 2 to check</summary>`,
		`What it says has 2 problems of fact. <a href="/rules/Errors#says">What it says</a>`,
		`Its stones rest on that replay. <a href="/rules/Errors?tab=firings#check">The replay beside what really fired</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("/rules/Errors: no %s", want)
		}
	}
	// What it says is to check with the rest, not a badge of its own.
	if strings.Contains(body, `class="badge check"`) {
		t.Error("/rules/Errors: a badge of what to check beside the list of it")
	}
	if _, body, _ := get(t, h, "/rules/Noise"); strings.Contains(body, `class="checks"`) {
		t.Error("/rules/Noise: a list of what to check, with nothing to")
	}
}

// TestRuleActions: a rule's page offers to propose a change, add a task and
// edit the note; to a reader who may not, each is disabled, with why, said
// once for those disabled alike.
func TestRuleActions(t *testing.T) {
	s, _ := a11yServer(t)
	_, body, _ := get(t, s.Handler(), "/rules/Errors?team=payments")
	for _, want := range []string{`<a class="button" href="/rules/Errors?tab=proposals&amp;team=payments#propose">Propose a change</a>`,
		`<a class="button quiet" href="/tasks?kind=tune&amp;rule=Errors&amp;team=payments&amp;title=Tune&#43;Errors">Add a task</a>`,
		`<a class="button quiet" href="/rules/Errors?edit=note&amp;team=payments#note">Edit the note</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s", want)
		}
	}
	// The note's form is opened by its link, and the proposals tab has the
	// form the first goes to.
	if _, body, _ := get(t, s.Handler(), "/rules/Errors?edit=note"); !strings.Contains(body, `<details open><summary>Change it</summary>`) {
		t.Error("edit=note: the note's form isn't open")
	}
	if _, body, _ := get(t, s.Handler(), "/rules/Errors?edit=x"); strings.Contains(body, `<details open><summary>Change it</summary>`) {
		t.Error("edit=x: the note's form is open")
	}
	if _, body, _ := get(t, s.Handler(), "/rules/Errors?tab=proposals"); !strings.Contains(body, `id="propose"`) {
		t.Error("the proposals tab has no #propose")
	}

	s.cfg.ReadOnly = true
	s.cache.Lock()
	s.cache.gen = "" // as a server started with --read-only
	s.cache.Unlock()
	_, body, _ = get(t, s.Handler(), "/rules/Errors")
	want := `<p class="rule-actions"><button type="button" disabled aria-describedby="rule-why-1">Propose a change</button> ` +
		`<button type="button" disabled aria-describedby="rule-why-1">Add a task</button> <button type="button" disabled aria-describedby="rule-why-1">Edit the note</button> ` +
		`<span id="rule-why-1" class="why">Read-only: lapilli serve was started with --read-only</span> </p>`
	if !strings.Contains(body, want) {
		t.Errorf("read-only: no %s", want)
	}

	// A team's editor adds their team's tasks and edits its notes, and
	// proposes nothing.
	r, pay, _ := rolesServer(t, nil)
	if _, err := r.l.PutGrant(context.Background(), ledger.Grant{Who: "user:pat@example.com", TeamID: pay}, "boss"); err != nil {
		t.Fatal(err)
	}
	if err := r.l.PutNote(context.Background(), ledger.Note{Rule: "Errors", TeamID: pay}, "boss"); err != nil {
		t.Fatal(err)
	}
	_, body, _ = ui(t, r.Handler(), http.MethodGet, "/rules/Errors", "pat@example.com", "", nil)
	for _, want := range []string{`<button type="button" disabled aria-describedby="rule-why-1">Propose a change</button>`,
		`<span id="rule-why-1" class="why">Editors only</span>`, `">Add a task</a>`, `">Edit the note</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("a team's editor: no %s", want)
		}
	}
}

// TestRuleBar: the bar of a rule's page picks the period for it, in the tab
// it shows, and sends nothing else it was asked.
func TestRuleBar(t *testing.T) {
	s, _ := a11yServer(t)
	_, body, _ := get(t, s.Handler(), "/rules/Errors?tab=firings&zzz=1&edit=note&team=payments")
	want := `<form method="get" action="/rules/Errors" class="ctx" aria-label="Period"><input type="hidden" name="tab" value="firings"><input type="hidden" name="team" value="payments">`
	if !strings.Contains(body, want) || strings.Contains(unlinked(body), "zzz") {
		t.Errorf("the bar: no %s, or zzz in it", want)
	}
	if !strings.Contains(body, `<option value="12" selected>12 weeks</option>`) {
		t.Error("a rule's page isn't twelve weeks unless asked")
	}
	_, body, _ = get(t, s.Handler(), "/rules/Errors?weeks=4")
	if !strings.Contains(body, `<option value="4" selected>4 weeks</option>`) || !strings.Contains(body, "The last 4 weeks, from 2026-10-12.") {
		t.Error("weeks=4: not four weeks")
	}
	_, body, _ = get(t, s.Handler(), "/rules/Errors?weeks=1")
	if !strings.Contains(body, "The last week, from 2026-11-02.") {
		t.Error("weeks=1: not the last week")
	}
}

// TestRuleBadgeLegend: the rules list says what each badge means, named
// and signed as the facets and the badges are.
func TestRuleBadgeLegend(t *testing.T) {
	s := facetServer(t)
	_, body, _ := get(t, s.Handler(), "/rules")
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var legend *html.Node
	for _, n := range allNodes(doc) {
		if n.DataAtom == atom.Details && nodeAttr(n, "class") == "badge-legend" {
			legend = n
		}
	}
	if legend == nil {
		t.Fatal("no legend")
	}
	var got []string
	for _, n := range allNodes(legend) {
		if n.DataAtom == atom.Dt {
			got = append(got, squashed(nodeText(n)))
		}
	}
	want := []string{"● Noisy", "○ OK", "– Silent", "⧉ Spare", "≠ Differs", "◌ Not replayed", "⇣ Recorded only", "✓ keep", "± tune", "⊘ retire",
		"✎ Proposed", "⚠︎ Problems to check"}
	if !slices.Equal(got, want) {
		t.Errorf("legend %q, want %q", got, want)
	}
	// The same words as the facets'.
	for _, f := range facetsOf {
		for i, x := range f.values {
			if _, ok := badgeMeans[f.param+"/"+x]; ok && !slices.ContainsFunc(got, func(g string) bool { return strings.HasSuffix(g, " "+f.labels[i]) }) {
				t.Errorf("%s=%s, %q, isn't in the legend", f.param, x, f.labels[i])
			}
		}
	}
	// And the signs the badges have.
	for _, badge := range []string{`<span aria-hidden="true">●</span> Noisy`, `<span aria-hidden="true">⇣</span> Recorded only`, `<span aria-hidden="true">±</span> tune`,
		`<span aria-hidden="true">✎</span> Proposed`} {
		if strings.Count(body, badge) < 2 {
			t.Errorf("%s: not both a badge and in the legend", badge)
		}
	}
}

// TestVersionDiff: each version beside the one before, line by line: one
// changed, one taken away, one written again.
func TestVersionDiff(t *testing.T) {
	a := change{Expr: "rate(x[5m])\n  > 1", For: "5m", Labels: "severity=page team=a"}
	b := change{Expr: "rate(x[5m])\n  > 2", For: "10m", Keep: "1m", Labels: "severity=page team=a"}
	text := func(ds []diffLine) string {
		var out []string
		for _, d := range ds {
			out = append(out, d.Op+d.Text)
		}
		return strings.Join(out, "\n")
	}
	if got, want := text(versionDiff(a, b)), "@@ -1,5 +1,6 @@\n expr: rate(x[5m])\n-        > 1\n-for: 5m\n+        > 2\n+for: 10m\n+keep_firing_for: 1m\n label: severity=page\n label: team=a"; got != want {
		t.Errorf("a change:\n%s\nwant\n%s", got, want)
	}
	gone := change{Gone: true}
	if got := text(versionDiff(b, gone)); !strings.HasPrefix(got, "@@ -1,6 +1,0 @@\n-expr:") || strings.Contains(got, "\n+") {
		t.Errorf("taken away:\n%s", got)
	}
	if got := text(versionDiff(gone, a)); !strings.HasPrefix(got, "@@ -1,0 +1,5 @@\n+expr:") || strings.Contains(got, "\n-") {
		t.Errorf("written again:\n%s", got)
	}
}

// TestWindowDays: the days of a rule's strip a quiet window covers are
// those the ledger's own Covers says it covers some time of, in UTC and in
// zones whose clocks change over the strip: one that skips midnight
// (America/Santiago, 2026-09-06), one that goes back an hour at night
// (America/New_York, Europe/London) and one that goes forward half an hour
// (Australia/Lord_Howe). For a window once, every day (until a day), every
// week, every month and by cron, often or once a year; each starting at,
// or lasting over, the times the clocks skip or repeat.
func TestWindowDays(t *testing.T) {
	for _, zone := range []string{"UTC", "America/Santiago", "America/New_York", "Europe/London", "Australia/Lord_Howe"} {
		t.Run(zone, func(t *testing.T) {
			loc, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			s, _ := server(t, func(c *Config) { c.Loc = loc })
			ctx := context.Background()
			o := s.cfg.Options
			first := s.l.Day(now) - stripDays + 1
			from, to := s.l.Midnight(first), s.l.Midnight(first+stripDays)
			at := func(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, loc) }
			for _, w := range []ledger.Window{
				{Title: "once, over the clocks' change", Start: at(2026, 9, 5, 22, 0), End: at(2026, 9, 6, 3, 0)},
				{Title: "once, before", Start: from.Add(-30 * time.Hour), End: from.Add(-2 * time.Hour)},
				{Title: "once, into it", Start: from.Add(-3 * time.Hour), End: from.Add(2 * time.Hour)},
				{Title: "daily 00:25, two hours", Start: at(2026, 8, 1, 0, 25), End: at(2026, 8, 1, 2, 25), Every: "day"},
				{Title: "daily 00:25, half an hour", Start: at(2026, 8, 1, 0, 25), End: at(2026, 8, 1, 0, 55), Every: "day"},
				{Title: "daily 23:30, over midnight, until", Start: at(2026, 8, 1, 23, 30), End: at(2026, 8, 2, 1, 0), Every: "day", Until: at(2026, 10, 30, 0, 0)},
				{Title: "daily 01:30, an hour", Start: at(2026, 8, 1, 1, 30), End: at(2026, 8, 1, 2, 30), Every: "day"},
				{Title: "daily 02:15, twenty minutes", Start: at(2026, 8, 1, 2, 15), End: at(2026, 8, 1, 2, 35), Every: "day"},
				{Title: "weekly, Sundays 00:25, from the Sunday before the clocks' change", Start: at(2026, 8, 30, 0, 25), End: at(2026, 8, 30, 2, 25), Every: "week"},
				{Title: "weekly, Saturdays 23:00, five hours", Start: at(2026, 8, 1, 23, 0), End: at(2026, 8, 2, 4, 0), Every: "week", Until: at(2026, 10, 10, 0, 0)},
				{Title: "monthly, the 6th 00:10", Start: at(2026, 7, 6, 0, 10), End: at(2026, 7, 6, 1, 40), Every: "month"},
				{Title: "monthly, the first Sunday", Start: at(2026, 7, 5, 1, 30), End: at(2026, 7, 5, 2, 30), Every: "month", On: "first sun"},
				{Title: "cron 00:25 daily", Start: at(2026, 8, 1, 0, 25), End: at(2026, 8, 1, 2, 25), Every: "cron", On: "25 0 * * *"},
				{Title: "cron twice a week, until", Start: at(2026, 3, 3, 1, 15), End: at(2026, 3, 3, 1, 45), Every: "cron", On: "15 1 * * 2,5", Until: at(2026, 10, 1, 0, 0)},
				{Title: "cron yearly, the clocks' change", Start: at(2025, 9, 6, 0, 0), End: at(2025, 9, 6, 3, 0), Every: "cron", On: "0 0 6 9 *"},
				{Title: "cron nightly over midnight, until the day before", Start: at(2026, 3, 3, 23, 0), End: at(2026, 3, 4, 1, 0), Every: "cron", On: "0 23 * * *", Until: from.Add(-24 * time.Hour)},
				// Six times a day, more than the strip walks: a day at a
				// time, until a day.
				{Title: "cron six times a night, until", Start: at(2026, 3, 3, 0, 0), End: at(2026, 3, 3, 0, 20), Every: "cron", On: "0 0-5 * * *", Until: at(2026, 10, 20, 0, 0)},
			} {
				if _, err := s.l.PutWindow(ctx, w, "ana", o); err != nil {
					t.Fatalf("%s: %v", w.Title, err)
				}
			}
			ws, err := s.l.Windows(ctx, from.Add(-48*time.Hour), to)
			if err != nil {
				t.Fatal(err)
			}
			if len(ws) != 17 {
				t.Fatalf("%d windows", len(ws))
			}
			for _, w := range ws {
				got := map[int64]bool{}
				windowDays(w, s.l, first, stripDays, func(i int) {
					if i < 0 || i >= stripDays {
						t.Fatalf("%s: day %d of %d", w.Title, i, stripDays)
					}
					got[first+int64(i)] = true
				})
				// The days Covers says it covers some time of: every time
				// it covers starts and ends on five minutes, as the zones'
				// days do.
				want := map[int64]bool{}
				for m := from; m.Before(to); m = m.Add(5 * time.Minute) {
					if w.Covers(m) {
						want[s.l.Day(m)] = true
					}
				}
				if len(want) == 0 && w.Title != "once, before" && !strings.Contains(w.Title, "the day before") {
					t.Errorf("%s covers nothing of the strip", w.Title)
				}
				for d := first; d < first+stripDays; d++ {
					if got[d] != want[d] {
						t.Errorf("%s: %s marked %v, covered %v", w.Title, s.l.Midnight(d).In(loc).Format("2006-01-02"), got[d], want[d])
					}
				}
			}
		})
	}
}

// TestFiringsDay: /firings?day= shows that day's firings, from where the
// day starts, also where its midnight is skipped.
func TestFiringsDay(t *testing.T) {
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := server(t, func(c *Config) { c.Loc = loc })
	ctx := context.Background()
	o := s.cfg.Options
	for _, f := range []struct {
		rule string
		at   time.Time
	}{{"Saturday", time.Date(2026, 9, 5, 21, 0, 0, 0, loc)}, {"Sunday", time.Date(2026, 9, 6, 10, 0, 0, 0, loc)}} {
		if err := s.l.Record(ctx, ledger.Found{From: f.at.Add(-time.Hour), To: f.at.Add(time.Hour), Firings: []firing.Firing{{Rule: f.rule, Labels: map[string]string{"service": "x"}, Start: f.at, End: f.at.Add(time.Minute)}},
			Run: ledger.Run{Kind: "backfill", Started: f.at, Finished: f.at, From: f.at.Add(-time.Hour), To: f.at.Add(time.Hour)}}, o); err != nil {
			t.Fatal(err)
		}
	}
	for day, want := range map[string]string{"2026-09-06": "Sunday", "2026-09-05": "Saturday"} {
		r := httptest.NewRequest(http.MethodGet, "/firings?day="+day, nil)
		v, err := s.firingsPage(r)
		if err != nil {
			t.Fatal(err)
		}
		fv := v.(firingsView)
		if fv.Day != day || len(fv.Lines) != 1 || fv.Lines[0].Rule != want {
			t.Errorf("/firings?day=%s: the day %s, %d firings %+v", day, fv.Day, len(fv.Lines), fv.Lines)
		}
	}
	if d, err := dateDay("2026-09-06"); err != nil || s.l.Midnight(d).In(loc).Format("2006-01-02 15:04") != "2026-09-06 01:00" {
		t.Errorf("2026-09-06 starts at %s, %v", s.l.Midnight(d).In(loc), err)
	}
}

// TestDayKind: a day of the strip is what its stones were: one that caught
// something, whatever else; else one that caught nothing; else excused; else
// no firing.
func TestDayKind(t *testing.T) {
	for sp, want := range map[ledger.Span]string{
		{Firings: 3, White: 1, Excused: 2}: "caught", {Firings: 1, White: 1}: "caught", {Firings: 2, Excused: 5}: "nothing",
		{Firings: 1}: "nothing", {Excused: 1}: "excused", {}: "none", {Caught: 1}: "none",
	} {
		if got := dayKind(sp); got != want {
			t.Errorf("%+v: %s, want %s", sp, got, want)
		}
	}
}

// glanceServer is a ledger of a rule over five weeks: in each, firings that
// caught something and some that didn't, a firing a quiet window excused,
// incidents it caught; a deploy of what it is about and one of something
// else; a version of it changed; a weekly quiet window over it, and one of
// another rule's.
func glanceServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	s, rules := server(t, func(c *Config) { c.Backfill, c.CheckReplay = time.Hour, true })
	s.h = &alertsHistory{}
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte("groups:\n  - name: shop\n    rules:\n      - {alert: Errors, expr: errors > 5, for: 2m, labels: {severity: page}}\n"), 0o644)
	s.now = func() time.Time { return now.Add(-20 * 24 * time.Hour) }
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte("groups:\n  - name: shop\n    rules:\n      - {alert: Errors, expr: errors > 9, for: 2m, labels: {severity: page}}\n"), 0o644)
	s.now = func() time.Time { return now.Add(-10 * 24 * time.Hour) }
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	o := s.cfg.Options
	var incs []incident.Incident
	first := s.l.Midnight(s.l.Day(now) - 34)
	for d := 0; d < 35; d++ {
		day := first.Add(time.Duration(d) * 24 * time.Hour)
		var fs []firing.Firing
		// A firing that caught nothing every day; one that caught
		// something every fifth, and yesterday, a Monday; on the Saturday
		// before, two that caught one incident, in the afternoon.
		fs = append(fs, firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "search"}, Start: day.Add(3 * time.Hour), End: day.Add(3*time.Hour + 5*time.Minute)})
		if d%5 == 0 || d == 31 || d == 33 {
			at := day.Add(14 * time.Hour)
			fs = append(fs, firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at, End: at.Add(10 * time.Minute)})
			if d == 31 {
				fs = append(fs, firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "checkout", "pod": "b"}, Start: at, End: at.Add(10 * time.Minute)})
			}
			incs = append(incs, incident.Incident{ID: fmt.Sprint("INC-", d), Start: at.Add(time.Minute), End: at.Add(30 * time.Minute), Labels: incident.Labels{"service": {"checkout"}}})
		}
		if err := s.l.Record(ctx, ledger.Found{From: day, To: day.Add(24*time.Hour - time.Second), Firings: fs,
			Run: ledger.Run{Kind: "backfill", Started: day, Finished: day, From: day, To: day.Add(24 * time.Hour)}}, o); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.l.SyncFile(ctx, incs, o); err != nil {
		t.Fatal(err)
	}
	o.DeployAfter = 10 * time.Minute
	if _, err := s.l.SyncDeploys(ctx, []ledger.Deploy{{Start: first.Add(30*24*time.Hour + 13*time.Hour + 50*time.Minute), End: first.Add(30*24*time.Hour + 13*time.Hour + 55*time.Minute), Labels: map[string]string{"service": "checkout"}},
		{Start: first.Add(31 * 24 * time.Hour), End: first.Add(31*24*time.Hour + 5*time.Minute), Labels: map[string]string{"service": "elsewhere"}}}, o, ledger.DeploysRead{}); err != nil {
		t.Fatal(err)
	}
	// Every Sunday 02:00-04:00 for this rule's search alerts, which excuses
	// that day's 03:00 firing; one for another rule; and two once, before
	// it fired: one for its own labels, one for labels it hasn't.
	for _, w := range []ledger.Window{
		{Title: "sundays", Start: time.Date(2026, 6, 7, 2, 0, 0, 0, time.UTC), End: time.Date(2026, 6, 7, 4, 0, 0, 0, time.UTC), Every: "week",
			Labels: incident.Labels{"service": {"search"}}},
		{Title: "another's", Start: time.Date(2026, 6, 9, 2, 0, 0, 0, time.UTC), End: time.Date(2026, 6, 9, 4, 0, 0, 0, time.UTC), Every: "week", Rules: "^Other$"},
		{Title: "its own", Start: first.Add(-20*24*time.Hour + time.Hour), End: first.Add(-20*24*time.Hour + 2*time.Hour), Labels: incident.Labels{"severity": {"page"}}},
		{Title: "not its own", Start: first.Add(-25*24*time.Hour + time.Hour), End: first.Add(-25*24*time.Hour + 2*time.Hour), Labels: incident.Labels{"service": {"search"}}},
	} {
		if _, err := s.l.PutWindow(ctx, w, "ana", o); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	// Its firings of the last six days notified once each, and twice the
	// last two; its replay beside what really fired over them, a day at a
	// time, alike but for one firing more really fired yesterday; and an
	// incident it nearly caught this week, and another rule's.
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: s.l.Midnight(s.l.Day(now) - 6), To: now, Rule: "Errors", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range fs {
		for k := 0; k < 1+map[bool]int{true: 1}[!f.Start.Before(s.l.Midnight(s.l.Day(now)-2))]; k++ {
			if _, err := s.l.Notify(ctx, ledger.Notification{Source: "alertmanager", Receiver: "oncall", Group: fmt.Sprint("g", i), At: f.Start.Add(time.Minute + time.Duration(k)*4*time.Hour),
				Alerts: []ledger.Alert{{Rule: "Errors", Labels: f.Labels, Start: f.Start}}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, seq, err := s.l.AgainFrom(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for d := s.l.Day(now) - 6; d < s.l.Day(now); d++ {
		fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: s.l.Midnight(d), To: s.l.Midnight(d + 1), Rule: "Errors", Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		x := &verdict.SpanRule{Replayed: len(fs), Real: len(fs), Alike: len(fs)}
		if d >= s.l.Day(now)-1 {
			x.Real++
		}
		if err := s.l.PutCheck(ctx, s.l.Midnight(d), &verdict.SpanCheck{Rules: map[string]*verdict.SpanRule{"Errors": x}}, nil, s.l.Midnight(d+1), seq); err != nil {
			t.Fatal(err)
		}
	}
	week := s.l.Midnight(ledger.Week(s.l.Day(now)))
	if err := s.l.ReplaceResult(ctx, "near_misses", now, week, week.Add(7*24*time.Hour), []verdict.NearMiss{{Incident: "INC-30", Rule: "Errors"},
		{Incident: "INC-31", Rule: "Other"}}); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestRuleGlance: the panel's numbers over the period, beside as long a
// span before it where the ledger holds that, and none where it doesn't;
// the strip's days, each the stones the ledger has for it, marked where
// the rule changed, a deploy of what it is about came, and a quiet window
// covered it; the table under it, the same numbers.
func TestRuleGlance(t *testing.T) {
	s := glanceServer(t)
	h := s.Handler()
	view := func(q string) ruleView {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/rules/Errors?"+q, nil)
		r.SetPathValue("name", "Errors")
		v, err := s.rulePage(r)
		if err != nil {
			t.Fatal(err)
		}
		return v.(ruleView)
	}
	// Its severity, which the ledger hasn't, from its labels, as the list
	// has it.
	if v := view(""); !slices.Contains(v.Facts, ruleFact{"severity", "page"}) || v.Severity != "" {
		t.Errorf("facts %+v; severity %q", v.Facts, v.Severity)
	}
	// Today is a Tuesday, noon: the last week is Monday and today, a day and
	// a half; as long before, from Saturday noon.
	v := view("weeks=1")
	sum := func(lo, hi time.Time) (n, white, excused, caught int) {
		for _, d := range v.Strip.Days {
			day, _ := time.ParseInLocation("2006-01-02", d.Day, s.cfg.Loc)
			if !day.Before(lo) && day.Before(hi) {
				n, white, excused, caught = n+d.Firings, white+d.White, excused+d.Excused, caught+d.Caught
			}
		}
		return
	}
	// The firings and catches of a span, from the ledger, to the second.
	fired := func(lo, hi time.Time) (n, white, caught int) {
		fs, err := s.l.Firings(context.Background(), ledger.FiringQuery{From: lo, To: hi, Rule: "Errors", Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fs {
			if !f.Excused {
				n++
				if f.White {
					white++
				}
			}
		}
		ins, err := s.l.CaughtBy(context.Background(), "Errors", 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range ins {
			if !in.Start.Before(lo) && in.Start.Before(hi) {
				caught++
			}
		}
		return
	}
	if want := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC); !v.From.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) || !v.Before.Equal(want) {
		t.Fatalf("weeks=1: from %s, before from %s, want %s: as long as from %s to now", v.From, v.Before, want, v.From)
	}
	n, white, _, caught := sum(v.From, now.Add(24*time.Hour))
	if n != v.Firings || white != v.White || caught != v.Incidents || v.Panel.Firings != v.Firings {
		t.Errorf("weeks=1: the strip's days have %d, %d, %d; the panel %d, %d, %d", n, white, caught, v.Firings, v.White, v.Incidents)
	}
	bn, bwhite, bcaught := fired(v.Before, v.From)
	if got := v.Panel.FiringsBy; !got.On || got.By != v.Firings-bn {
		t.Errorf("weeks=1: firings changed %+v, want %d", got, v.Firings-bn)
	}
	if got := v.Panel.IncidentsBy; !got.On || got.By != v.Incidents-bcaught {
		t.Errorf("weeks=1: incidents changed %+v, want %d", got, v.Incidents-bcaught)
	}
	now1, _ := sharePct(v.White, v.Firings)
	then, _ := sharePct(bwhite, bn)
	if got := v.Panel.PrecisionBy; !got.On || got.By != now1-then || now1 == then {
		t.Errorf("weeks=1: precision %d%% changed %+v, from %d%%", now1, got, then)
	}
	// Two firings caught one incident the Saturday before.
	if bwhite != 2 || bcaught != 1 {
		t.Errorf("weeks=1: before, %d caught something, %d incidents; want 2 and 1", bwhite, bcaught)
	}
	// The notifications: twice for each firing since Sunday, once before.
	ns, err := s.l.Notifications(context.Background(), v.Before, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	cur, was := 0, 0
	for _, x := range ns {
		if x.At.Before(v.From) {
			was++
		} else {
			cur++
		}
	}
	if got := v.Panel.NotificationsBy; !got.On || got.By != cur-was || v.Notifications != cur || cur != 2*n || was == 0 {
		t.Errorf("weeks=1: notifications %d changed %+v, want %d (twice %d firings) and %+d", v.Notifications, got, cur, n, cur-was)
	}
	// The backtest: all alike before, one firing a day not, since.
	if c := v.Check; c == nil || v.Panel.Backtest != fmt.Sprintf("%d%%", c.Share) || c.Share >= 100 || !v.Panel.BacktestBy.On || v.Panel.BacktestBy.By != c.Share-100 {
		t.Errorf("weeks=1: backtest %s changed %+v, its check %+v", v.Panel.Backtest, v.Panel.BacktestBy, v.Check)
	}
	// It nearly caught one incident this week; another rule, another.
	if v.Panel.Near != 1 {
		t.Errorf("weeks=1: %d nearly caught, want 1", v.Panel.Near)
	}
	// Four weeks, and the four before them: the ledger has five weeks, so
	// not all of them, and nothing is set beside.
	if v := view("weeks=4"); !v.Before.IsZero() || v.Panel.FiringsBy.On || v.Panel.PrecisionBy.On {
		t.Errorf("weeks=4: beside %s %+v, which the ledger hasn't whole", v.Before, v.Panel.FiringsBy)
	}
	// The strip: the Sundays covered and excused; the deploy of checkout,
	// and not the other; the change of version.
	first := func(d int64) string { return s.l.Midnight(s.l.Day(now) - 34 + d).Format("2006-01-02") }
	v = view("")
	marked := map[string][]string{}
	for _, d := range v.Strip.Days {
		if d.Quiet {
			marked["quiet"] = append(marked["quiet"], d.Day)
		}
		if d.Deploys > 0 {
			marked["deploy"] = append(marked["deploy"], fmt.Sprint(d.Day, "×", d.Deploys))
		}
		if d.Changed {
			marked["changed"] = append(marked["changed"], d.Day)
		}
		if t0, _ := time.Parse("2006-01-02", d.Day); t0.Weekday() == time.Sunday && d.Firings+d.Excused > 0 && d.Excused == 0 {
			t.Errorf("%s, a Sunday: its 03:00 firing not excused", d.Day)
		}
	}
	// Quiet: the Sundays a window excused its firings on, the ledger's
	// say; and the day a window for its own labels covered, before it
	// fired; not the Sundays before it fired, as its own labels aren't
	// the window's, nor the day of the window for labels it hasn't.
	if want := []string{first(-20), "2026-10-04", "2026-10-11", "2026-10-18", "2026-10-25", "2026-11-01"}; !slices.Equal(marked["quiet"], want) {
		t.Errorf("quiet %q, want %q", marked["quiet"], want)
	}
	if want := []string{s.l.Midnight(s.l.Day(now)-4).Format("2006-01-02") + "×1"}; !slices.Equal(marked["deploy"], want) {
		t.Errorf("deploys %q, want %q", marked["deploy"], want)
	}
	if want := []string{now.Add(-10 * 24 * time.Hour).Format("2006-01-02")}; !slices.Equal(marked["changed"], want) {
		t.Errorf("changed %q, want %q", marked["changed"], want)
	}
	if len(v.Strip.Days) != stripDays || v.Strip.Days[stripDays-1].Day != now.Format("2006-01-02") {
		t.Errorf("%d days, the last %s", len(v.Strip.Days), v.Strip.Days[len(v.Strip.Days)-1].Day)
	}
	for _, d := range v.Strip.Days {
		if d.Kind != dayKind(d.Span) {
			t.Errorf("%s: %s, its stones %+v", d.Day, d.Kind, d.Span)
		}
	}

	// The page: a cell a day, each saying its numbers, and the table's row
	// for that day the same; the legend names its marks.
	_, body, _ := get(t, h, "/rules/Errors")
	for _, mark := range []string{"</svg>the rule changed</li>", "</svg>fired during a deploy of what it is about</li>", "</svg>a quiet window covered it</li>"} {
		if !strings.Contains(body, mark) {
			t.Errorf("the legend has no %q", mark)
		}
	}
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	title := regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}): (\d+) fired, (\d+) caught something, (\d+) caught nothing, (\d+) excused(.*)$`)
	drawn, rows := map[string][]string{}, map[string][]string{}
	for _, n := range allNodes(doc) {
		switch {
		case n.Data == "title" && nodeWithin(n, atom.Svg):
			m := title.FindStringSubmatch(nodeText(n))
			if m == nil {
				t.Fatalf("a cell says %q", nodeText(n))
			}
			marks := strings.TrimPrefix(m[6], "; ")
			marks = strings.ReplaceAll(marks, "; ", ", ")
			if marks == "" {
				marks = "–"
			}
			drawn[m[1]] = []string{m[2], m[3], m[4], m[5], marks}
			// Its cell is drawn as its stones are.
			var cls []string
			for c := n.NextSibling; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode {
					cls = append(cls, c.Data+"."+nodeAttr(c, "class"))
				}
			}
			kind := map[bool]string{true: "caught"}[m[3] != "0"]
			switch {
			case kind != "":
			case m[2] != "0":
				kind = "nothing"
			case m[5] != "0":
				kind = "excused"
			default:
				kind = "none"
			}
			if !slices.Contains(cls, "rect."+kind) || (kind == "caught") != slices.Contains(cls, "circle.hole") {
				t.Errorf("%s: drawn %q, its stones %s", m[1], cls, kind)
			}
		case n.DataAtom == atom.Tr && nodeWithin(n, atom.Tbody) && nodeWithin(n, atom.Details):
			var cells []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode {
					cells = append(cells, squashed(nodeText(c)))
				}
			}
			if len(cells) == 7 {
				rows[cells[0]] = []string{cells[1], cells[2], cells[3], cells[4], cells[6]}
			}
		}
	}
	if len(drawn) != stripDays || len(rows) != stripDays {
		t.Fatalf("%d cells drawn, %d rows", len(drawn), len(rows))
	}
	for d, x := range drawn {
		if !slices.Equal(rows[d], x) {
			t.Errorf("%s: drawn %q, the table %q", d, x, rows[d])
		}
	}
}

// TestRulePanelText: a cell with nothing to say says "–", and the panel
// says which period it is of.
func TestRulePanelText(t *testing.T) {
	s := facetServer(t)
	code, body, _ := get(t, s.Handler(), "/rules/R20")
	if code != http.StatusOK {
		t.Fatalf("/rules/R20: %d", code)
	}
	for _, want := range []string{"The last 12 weeks, from 2026-08-17.", `<dd class="big">–</dd>`, "no firing", "no notification kept", "not set beside what really fired"} {
		if !strings.Contains(body, want) {
			t.Errorf("/rules/R20: no %s", want)
		}
	}
	if strings.Contains(body, `<dd class="big">0%</dd>`) || strings.Contains(body, "Beside each number") {
		t.Error("/rules/R20: 0% for a rule that didn't fire, or a change beside nothing")
	}
	// The strip's legend names the marks it has, and no other.
	for _, mark := range []string{"the rule changed", "fired during a deploy of what it is about", "a quiet window covered it"} {
		if strings.Contains(body, mark) {
			t.Errorf("/rules/R20: the legend names %q, which it hasn't", mark)
		}
	}
}

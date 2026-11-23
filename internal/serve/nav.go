package serve

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"sync"
)

// The frame every page is drawn in (docs/design/ui.md): the pages in four
// groups, where the reader is, the team and period looked at, and what a
// page's main purpose lets the reader do.

// navItem is a page in the sidebar; Count, when it has one, is how many
// wait there: open tasks, proposals to approve.
type navItem struct {
	Page, Href, Name string
	Count            int
	CountText        string
}

// navGroup is a group of pages, named; a link itself only to a page of
// its own, Href (Administration's), Here when the reader is on it.
type navGroup struct {
	ID, Name, Href string
	Here           bool
	Items          []navItem
}

// crumbOf is where a page is: its group, and the page it is one of, and
// whether it is an item of that page (a rule of the rules).
type crumbOf struct {
	Group, Name, Href string
	Item              bool
}

// groupPages are the groups that have a page of their own, which their
// name in the sidebar and in the breadcrumbs links to: Administration's.
var groupPages = map[string]string{"Administration": "/manage"}

// GroupHref is the page of a page's group, "" for a group with none.
func (c crumbOf) GroupHref() string { return groupPages[c.Group] }

var crumbsOf = map[string]crumbOf{
	"overview": {"Monitoring", "Overview", "/", false}, "rules": {"Monitoring", "Rules", "/rules", false}, "rule": {"Monitoring", "Rules", "/rules", true},
	"incidents": {"Monitoring", "Incidents", "/incidents", false}, "incident": {"Monitoring", "Incidents", "/incidents", true},
	"firings": {"Monitoring", "Alert history", "/firings", false},
	"marked":  {"Monitoring", "Marked windows", "/marked", false}, "markedone": {"Monitoring", "Marked windows", "/marked", true},
	"tasks": {"Improve", "Tasks", "/tasks", false}, "task": {"Improve", "Tasks", "/tasks", true},
	"proposals": {"Improve", "Proposals", "/proposals", false}, "proposal": {"Improve", "Proposals", "/proposals", true},
	"windows": {"Improve", "Quiet windows", "/windows", false}, "window": {"Improve", "Quiet windows", "/windows", true},
	"reports": {"Reporting", "Reports", "/reports", false}, "report": {"Reporting", "Reports", "/reports", true},
	"outcomes": {"Reporting", "Outcomes", "/outcomes", false}, "goals": {"Reporting", "Goals", "/goals", false},
	"teams": {"Administration", "Teams", "/teams", false}, "roles": {"Administration", "Roles", "/roles", false},
	"settings": {"Administration", "Settings", "/settings", false}, "runs": {"Administration", "Run history", "/runs", false},
	"sourcelist": {"Administration", "Sources", "/sources", false}, "activity": {"Administration", "Run history", "/runs", true},
	"orphans": {"Administration", "Cleanup", "/orphans", false},
	"manage":  {"", "Administration", "/manage", false},
	"search":  {"", "Search", "/search", false},
	"error":   {"", "Overview", "/", true}, "signedout": {"", "", "", true},
}

// navPage is the page of the sidebar a page is under, and its name: a
// rule's, Rules.
func navPage(name string) (string, string) {
	c, ok := crumbsOf[name]
	if !ok || c.Group == "" {
		return "", ""
	}
	for _, g := range navLayout {
		for _, it := range g.items {
			if it.href == c.Href {
				return it.page, it.name
			}
		}
	}
	return "", ""
}

// navLayout is the sidebar: Monitoring, Improve, Reporting,
// Administration. Each group and page is named by a noun, what it holds,
// as a console's sidebar is, not by what a reader does there; a page's
// title and breadcrumb are its name here. The URLs are as they were.
var navLayout = []struct {
	id, name string
	items    []struct{ page, href, name string }
}{
	{"see", "Monitoring", []struct{ page, href, name string }{{"overview", "/", "Overview"}, {"rules", "/rules", "Rules"}, {"incidents", "/incidents", "Incidents"}, {"marked", "/marked", "Marked windows"}, {"firings", "/firings", "Alert history"}}},
	{"fix", "Improve", []struct{ page, href, name string }{{"tasks", "/tasks", "Tasks"}, {"proposals", "/proposals", "Proposals"}, {"windows", "/windows", "Quiet windows"}}},
	{"report", "Reporting", []struct{ page, href, name string }{{"reports", "/reports", "Reports"}, {"outcomes", "/outcomes", "Outcomes"}, {"goals", "/goals", "Goals"}}},
	{"manage", "Administration", []struct{ page, href, name string }{{"teams", "/teams", "Teams"}, {"roles", "/roles", "Roles"}, {"settings", "/settings", "Settings"}, {"sourcelist", "/sources", "Sources"}, {"runs", "/runs", "Run history"}, {"orphans", "/orphans", "Cleanup"}}},
}

// bare reports whether a page is shown in the frame alone: an error, or
// the page after signing out, which anyone may be shown. It has no count
// of what waits, and none of the page context: a stranger refused reads
// nothing of the ledger from it, nor whether the team the URL names is
// there.
func bare(name string) bool { return name == "error" || name == "signedout" }

// nav is the sidebar for a page: Roles only for those who may open it, an
// admin, and the counts for the team in effect, but on a bare page.
func (s *Server) nav(ctx context.Context, p page) []navGroup {
	var open map[int64]int
	var approve map[string]int
	if !bare(p.Page) {
		open, approve = s.waiting(ctx)
	}
	var out []navGroup
	for _, g := range navLayout {
		ng := navGroup{ID: g.id, Name: g.name}
		if h := groupPages[g.name]; h != "" {
			ng.Href, ng.Here = p.Ctx.Link(h), crumbsOf[p.Page] == crumbOf{Name: g.name, Href: h}
		}
		for _, it := range g.items {
			if it.page == "roles" && !p.Status.Admin {
				continue
			}
			ni := navItem{Page: it.page, Href: p.Ctx.Link(it.href), Name: it.name}
			switch it.page {
			case "tasks":
				if p.Ctx.Team == "" {
					for _, n := range open {
						ni.Count += n
					}
				} else if o, err := s.owners(ctx); err == nil {
					if t, ok := o.team(p.Ctx.Team); ok {
						ni.Count = open[t.ID]
					}
				}
				ni.CountText = "%d open"
			case "proposals":
				// Those of the team in effect, as the list shows them.
				if p.Ctx.Team == "" {
					for _, n := range approve {
						ni.Count += n
					}
				} else if o, err := s.owners(ctx); err == nil {
					for rule, n := range approve {
						if o.teamOf(rule) == p.Ctx.Team {
							ni.Count += n
						}
					}
				}
				ni.CountText = "%d to approve"
			}
			ng.Items = append(ng.Items, ni)
		}
		out = append(out, ng)
	}
	return out
}

// waitingCache keeps the open tasks, by team, and the proposals waiting for
// an approval, by rule, until the ledger changes: every page's sidebar
// shows them.
type waitingCache struct {
	sync.Mutex
	gen     int64
	ok      bool
	open    map[int64]int
	approve map[string]int
}

// waiting are the open tasks, by team (0 for none), and the proposals
// waiting for an approval, by rule; none when they can't be read.
func (s *Server) waiting(ctx context.Context) (map[int64]int, map[string]int) {
	gen := s.l.Gen()
	s.wait.Lock()
	if s.wait.ok && s.wait.gen == gen {
		open, approve := s.wait.open, s.wait.approve
		s.wait.Unlock()
		return open, approve
	}
	s.wait.Unlock()
	open, err := s.l.OpenTasks(ctx)
	if err != nil {
		return nil, nil
	}
	approve, err := s.l.ProposedByRule(ctx)
	if err != nil {
		return nil, nil
	}
	s.wait.Lock()
	s.wait.gen, s.wait.ok, s.wait.open, s.wait.approve = gen, true, open, approve
	s.wait.Unlock()
	return open, approve
}

// barView is the bar at the top of a page: the team and period it is shown
// for, where the page honours them, and the search.
type barView struct {
	// Action is the page the team and period are chosen for; "" where the
	// page shows neither. Label names the form: what it chooses.
	Action, Label string
	Team, Weeks   bool
	// WeeksIn is the period in effect.
	WeeksIn int
	// Keep are the page's other parameters, sent again with a choice.
	Keep []field
	// Search is the search's context, as hidden fields, and Q what was
	// searched for, on the search page.
	Search []field
	Q      string
	// Teams and Periods are the bar's choices, each a link: picked, the page
	// is shown for it at once, with no script and no button to press after
	// (bundle 69).
	Teams, Periods []barLink
}

// barKeepBudget is the most of a page's other parameters, encoded, that
// each of the bar's choices carries: past it, a choice carries the team,
// the weeks and, on a rule's page, its tab alone, so that a query of junk isn't written
// out again for every team (bundle 69's review).
const barKeepBudget = 1 << 10

// barLink is one of the bar's choices: Text a message to translate, or,
// Raw, a team's name; Mine marks the reader's own team.
type barLink struct {
	Href, Text         string
	Raw, Mine, Current bool
}

// barHref is the page the bar chooses for, with what it carries and the
// team and weeks given: set says team= is sent, empty for all teams;
// weeks 0 sends none.
func (b barView) barHref(team string, set bool, weeks int) string {
	q := url.Values{}
	for _, f := range b.Keep {
		q.Add(f.Name, f.Value)
	}
	if set {
		q.Set("team", team)
	}
	if weeks != 0 {
		q.Set("weeks", strconv.Itoa(weeks))
	}
	if len(q) == 0 {
		return b.Action
	}
	return b.Action + "?" + q.Encode()
}

// barOf is a page's bar.
func barOf(name string, r *http.Request, c pageCtx) barView {
	// The search, from a page of a period of its own, has its weeks: a rule
	// it opens is over them.
	b := barView{Search: c.ofPage(name).hidden(), WeeksIn: 4}
	if name == "rule" {
		b.WeeksIn = weeks // a rule's page shows twelve weeks, as it always has
	}
	if c.Weeks != 0 {
		b.WeeksIn = c.Weeks
	}
	if name == "search" {
		b.Q = searchOf(r.URL.Query()["q"]).Q
	}
	b.Team = teamPages[name] && len(c.Teams) > 0
	b.Weeks = weeksPages[name]
	if b.Team || b.Weeks {
		b.Action = crumbsOf[name].Href
		switch {
		case b.Team && b.Weeks:
			b.Label = "Team and period"
		case b.Team:
			b.Label = "Team"
		default:
			b.Label = "Period"
		}
		// What a bulk write did is said once, not carried on.
		b.Keep = keepParams(r.URL.Query(), append([]string{"team", "weeks"}, bulkParams...)...)
		if name == "rule" {
			// A rule's own page, in the tab it shows: nothing else it was
			// asked is sent again.
			b.Action, b.Keep = r.URL.EscapedPath(), nil
			if tab := ruleTabOf(r.URL.Query()); tab != "overview" {
				b.Keep = []field{{"tab", tab}}
			}
		}
		// What the bar doesn't offer, it carries as it came.
		for _, f := range c.hidden() {
			if f.Name == "team" && !b.Team || f.Name == "weeks" && !b.Weeks {
				b.Keep = append(b.Keep, f)
			}
		}
		keep := url.Values{}
		for _, f := range b.Keep {
			keep.Add(f.Name, f.Value)
		}
		if len(keep.Encode()) > barKeepBudget {
			var small []field
			for _, f := range b.Keep {
				// The team and weeks carried are known values; a rule's tab
				// is the one its page names (ruleTabOf), not the reader's.
				if f.Name == "team" || f.Name == "weeks" || f.Name == "tab" && name == "rule" {
					small = append(small, f)
				}
			}
			b.Keep = small
		}
		// A choice of one keeps the other as the reader chose it: the weeks
		// asked for, or none, so the page's own period stands; the team
		// asked for, or none, so the reader's own stands.
		keepWeeks, keepTeam, teamSet := 0, "", false
		if b.Weeks {
			keepWeeks = c.Weeks
		}
		if b.Team && c.TeamSet {
			keepTeam, teamSet = c.Team, true
		}
		if b.Team {
			b.Teams = append(b.Teams, barLink{Href: b.barHref("", true, keepWeeks), Text: "All teams", Current: c.Team == ""})
			for _, name := range c.Teams {
				b.Teams = append(b.Teams, barLink{Href: b.barHref(name, true, keepWeeks), Text: name, Raw: true,
					Mine: c.Mine && name == c.Team, Current: name == c.Team})
			}
		}
		if b.Weeks {
			for _, w := range []struct {
				n    int
				text string
			}{{1, "1 week"}, {4, "4 weeks"}, {12, "12 weeks"}} {
				b.Periods = append(b.Periods, barLink{Href: b.barHref(keepTeam, teamSet, w.n), Text: w.text, Current: w.n == b.WeeksIn})
			}
		}
	}
	return b
}

// pageAction is a page's main action, in its header: a link to the form
// on the page, or, when the reader may not use it, the button disabled
// with why beside it, never hidden. Where a page has several (a rule's),
// ID is the id of the why; Quiet, one drawn less loudly than the first.
// Again says, of one the off template draws, that another by the same id
// says why (in a list, the bar before it; in a rule's header, the last
// disabled for the same reason, after it): it is the button alone, tied
// to that sentence.
type pageAction struct {
	Label, Href, Why, ID string
	// WhyOf is the name Why says, a team's, when it says one.
	WhyOf        string
	Quiet, Again bool
}

// mainAction is a page's main action. need is who may: "editor", "admin", a
// team's editor too ("team"); may, whether the reader may, as the page
// shows its form; none, a reason the form isn't there for anyone.
func mainAction(p page, label string, may bool, need, none string) pageAction {
	// The form is on the page: a link to it alone, which doesn't load the
	// page again, with fewer parameters than it has.
	a := pageAction{Label: label, Href: "#add"}
	switch {
	case may && none == "":
		return a
	case may:
		a.Why = none
	case p.Status.Why != "":
		a.Why = p.Status.Why
	case need == "admin":
		a.Why = "Admins only"
	default:
		a.Why = "Editors only"
	}
	a.Href = ""
	return a
}

// why is why the reader may change nothing here, when it isn't their role:
// the server is read-only, gives no one a role, or no one signed in.
func (s *Server) why(r *http.Request) string {
	switch {
	case s.cfg.ReadOnly:
		return "Read-only: lapilli serve was started with --read-only"
	case len(s.cfg.Editors)+len(s.cfg.Admins) == 0 && !s.open.Load():
		return "Read-only: no one is given a role here"
	case len(s.cfg.Editors)+len(s.cfg.Admins) > 0 && s.user(r) == "anonymous":
		return "Read-only: no one is signed in"
	}
	return ""
}

// emptyView is a page, or a part of one, with nothing to show, and why: in
// three kinds, first (nothing judged yet), notfound (nothing matches what
// was asked, with the ways out) and done (nothing left to do). Title and
// Text are texts to translate.
type emptyView struct {
	Kind, Title, Text string
	Links             []emptyLink
	// Clear says all is well, as nothing to look at on the overview: its
	// title ok, with its sign. Nothing else left to do (no proposals yet)
	// is said in no colour.
	Clear bool
}

// emptyLink is a way out of an empty state; Name is a text to translate.
type emptyLink struct{ Name, Href string }

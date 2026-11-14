package serve

import (
	"context"
	"net/http"
	"sync"

	"lapilli.dev/internal/ledger"
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
// its own, Href (Manage's), Here when the reader is on it.
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
// name in the sidebar and in the breadcrumbs links to: Manage's.
var groupPages = map[string]string{"Manage": "/manage"}

// GroupHref is the page of a page's group, "" for a group with none.
func (c crumbOf) GroupHref() string { return groupPages[c.Group] }

var crumbsOf = map[string]crumbOf{
	"overview": {"See", "Overview", "/", false}, "rules": {"See", "Rules", "/rules", false}, "rule": {"See", "Rules", "/rules", true},
	"incidents": {"See", "Incidents", "/incidents", false}, "incident": {"See", "Incidents", "/incidents", true},
	"firings": {"See", "Firings", "/firings", false},
	"tasks":   {"Fix", "Tasks", "/tasks", false}, "task": {"Fix", "Tasks", "/tasks", true},
	"proposals": {"Fix", "Proposals", "/proposals", false}, "proposal": {"Fix", "Proposals", "/proposals", true},
	"windows": {"Fix", "Quiet windows", "/windows", false}, "window": {"Fix", "Quiet windows", "/windows", true},
	"reports": {"Report", "Reports", "/reports", false}, "report": {"Report", "Reports", "/reports", true},
	"outcomes": {"Report", "Outcomes", "/outcomes", false},
	"teams":    {"Manage", "Teams", "/teams", false}, "roles": {"Manage", "Roles", "/roles", false},
	"settings": {"Manage", "Settings", "/settings", false}, "runs": {"Manage", "Runs", "/runs", false},
	"manage": {"", "Manage", "/manage", false},
	"search": {"", "Search", "/search", false},
	"error":  {"", "Overview", "/", true}, "signedout": {"", "", "", true},
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

// navLayout is the sidebar: See, Fix, Report, Manage.
var navLayout = []struct {
	id, name string
	items    []struct{ page, href, name string }
}{
	{"see", "See", []struct{ page, href, name string }{{"overview", "/", "Overview"}, {"rules", "/rules", "Rules"}, {"incidents", "/incidents", "Incidents"}, {"firings", "/firings", "Firings"}}},
	{"fix", "Fix", []struct{ page, href, name string }{{"tasks", "/tasks", "Tasks"}, {"proposals", "/proposals", "Proposals"}, {"windows", "/windows", "Quiet windows"}}},
	{"report", "Report", []struct{ page, href, name string }{{"reports", "/reports", "Reports"}, {"outcomes", "/outcomes", "Outcomes"}}},
	{"manage", "Manage", []struct{ page, href, name string }{{"teams", "/teams", "Teams"}, {"roles", "/roles", "Roles"}, {"settings", "/settings", "Settings"}, {"runs", "/runs", "Runs"}}},
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
	var approve int
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
				ni.Count, ni.CountText = approve, "%d to approve"
			}
			ng.Items = append(ng.Items, ni)
		}
		out = append(out, ng)
	}
	return out
}

// waitingCache keeps the open tasks, by team, and the proposals waiting for
// an approval, until the ledger changes: every page's sidebar shows them.
type waitingCache struct {
	sync.Mutex
	gen     int64
	ok      bool
	open    map[int64]int
	approve int
}

// waiting are the open tasks, by team (0 for none), and how many proposals
// wait for an approval; none when they can't be read.
func (s *Server) waiting(ctx context.Context) (map[int64]int, int) {
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
		return nil, 0
	}
	ps, err := s.l.Proposals(ctx, ledger.ProposalQuery{States: []string{ledger.StateProposed}, Limit: 1000})
	if err != nil {
		return nil, 0
	}
	s.wait.Lock()
	s.wait.gen, s.wait.ok, s.wait.open, s.wait.approve = gen, true, open, len(ps)
	s.wait.Unlock()
	return open, len(ps)
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
		b.Keep = keepParams(r.URL.Query(), "team", "weeks")
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
	}
	return b
}

// pageAction is a page's main action, in its header: a link to the form
// on the page, or, when the reader may not use it, the button disabled
// with why beside it, never hidden. Where a page has several (a rule's),
// ID is the id of the why, Said whether this one says it, the first
// disabled for it; Quiet, one drawn less loudly than the first.
type pageAction struct {
	Label, Href, Why, ID string
	// WhyOf is the name Why says, a team's, when it says one.
	WhyOf       string
	Said, Quiet bool
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
}

// emptyLink is a way out of an empty state; Name is a text to translate.
type emptyLink struct{ Name, Href string }

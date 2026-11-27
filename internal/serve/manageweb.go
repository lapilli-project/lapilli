package serve

import (
	"net/http"
	"time"
)

// The Administration page, /manage (docs/design/ui.md): what the
// sidebar's Administration group holds, a card each with a line on what it
// is for, as Argo CD's settings open; Roles only for admins, who alone may
// open it, as the sidebar has it, in its order, the reports after them. The group's name in the sidebar links to
// it, and the breadcrumbs of its pages start with it.

// manageCard is a card of the Administration page: the page, its name and
// what it is for (texts to translate), and a fact of it, when one is cheap
// to say.
type manageCard struct {
	Href, Name, About string
	// Fact is a text to translate, with At put in when it is set, else N;
	// One, the text for N of one.
	Fact, One string
	N         int
	At        time.Time
	// Warn is a text to translate, of something wrong there, and Kind
	// its status: bad when nothing of it can be used, warn otherwise.
	Warn, Kind string
}

type manageView struct {
	Cards []manageCard
	Loc   *time.Location
	// Notify is where notices go, and whether they went.
	Notify *notifyStatus
}

func (s *Server) managePage(r *http.Request) (any, error) {
	ctx := r.Context()
	o, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	reports, err := s.l.Scheduleds(ctx)
	if err != nil {
		return nil, err
	}
	cursor, err := s.l.Cursor(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.sourceList(ctx)
	if err != nil {
		return nil, err
	}
	teams := manageCard{Href: "/teams", Name: "Teams", About: "Who owns which rules and incidents, and each team's hours.", Fact: "%d teams", One: "1 team", N: len(o.teams)}
	roles := manageCard{Href: "/roles", Name: "Roles", About: "Who may change what, and a team's own editors."}
	settings := manageCard{Href: "/settings", Name: "Settings", About: "How firings are judged, whether changes to the rules are paused, and what --config declares."}
	sources := manageCard{Href: "/sources", Name: "Sources", About: "Where the rules, the firings and the incidents are read from, and whether each is alive.", Fact: "%d sources", One: "1 source", N: len(items)}
	runs := manageCard{Href: "/runs", Name: "Run history", About: "Each replay and what was copied in, and what failed."}
	reportsCard := manageCard{Href: "/reports", Name: "Reports", About: "Who gets which report, where, and the weekly digest.", Fact: "%d reports", One: "1 report", N: len(reports)}
	cleanup := manageCard{Href: "/orphans", Name: "Cleanup", About: "Notes, tasks, comments and proposals of rules no longer in effect, deleted or renamed: moved to a rule, or deleted."}
	if len(items) == 0 {
		sources.Fact = ""
	}
	for _, it := range items {
		if it.State == "failing" || it.Down {
			sources.Warn, sources.Kind = "A source is failing or down.", "bad"
			break
		}
	}
	if !cursor.IsZero() {
		runs.Fact, runs.At = "Firings are in up to %s.", cursor
	}
	if until := s.frozenUntil(ctx); !until.IsZero() {
		// A change freeze holds now (bounds.go): until when.
		settings.Fact, settings.At = "A change freeze holds changes to the rules until %s.", until
	}
	switch c := s.shownStatus().Config; {
	case c == nil || len(c.Problems) == 0:
	case c.Whole:
		settings.Warn, settings.Kind = "The config file can't be used: the ledger keeps what it last declared.", "bad"
	default:
		settings.Warn, settings.Kind = "Some of what the config file declares can't be kept.", "warn"
	}
	// The sidebar's order, Roles for admins alone, then the reports.
	v := manageView{Loc: s.cfg.Loc, Cards: []manageCard{teams}}
	if s.role(r) >= admin {
		v.Cards = append(v.Cards, roles)
	}
	v.Cards = append(v.Cards, settings, sources, runs, reportsCard, cleanup)
	v.Notify, err = s.notifyStatus(ctx, lang(r))
	return v, err
}

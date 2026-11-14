package serve

import (
	"net/http"
	"time"
)

// The Manage page (docs/design/ui.md): what the sidebar's Manage group
// holds, a card each with a line on what it is for, as Argo CD's settings
// open; Roles only for admins, who alone may open it, as the sidebar has
// it. The group's name in the sidebar links to it, and the breadcrumbs of
// its pages start with it.

// manageCard is a card of the Manage page: the page, its name and what it
// is for (texts to translate), and a fact of it, when one is cheap to say.
type manageCard struct {
	Href, Name, About string
	// Fact is a text to translate, with At put in when it is set, else N;
	// One, the text for N of one.
	Fact, One string
	N         int
	At        time.Time
}

type manageView struct {
	Cards []manageCard
	Loc   *time.Location
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
	v := manageView{Loc: s.cfg.Loc, Cards: []manageCard{
		{Href: "/teams", Name: "Teams", About: "Who owns which rules and incidents, and each team's hours.", Fact: "%d teams", One: "1 team", N: len(o.teams)},
		{Href: "/roles", Name: "Roles", About: "Who may change what, and a team's own editors."},
		{Href: "/settings", Name: "Settings", About: "How firings are judged, and whether changes to the rules are paused."},
		{Href: "/runs", Name: "Runs", About: "Each replay and what was copied in, and what failed."},
		{Href: "/reports", Name: "Reports", About: "Who gets which report, where, and the weekly digest.", Fact: "%d reports", One: "1 report", N: len(reports)},
	}}
	if !cursor.IsZero() {
		v.Cards[3].Fact, v.Cards[3].At = "Firings are in up to %s.", cursor
	}
	if s.role(r) < admin {
		v.Cards = append(v.Cards[:1], v.Cards[2:]...) // Roles, for admins alone
	}
	return v, nil
}

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
	// Warn is a text to translate, of something wrong there.
	Warn string
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
	v := manageView{Loc: s.cfg.Loc, Cards: []manageCard{
		{Href: "/teams", Name: "Teams", About: "Who owns which rules and incidents, and each team's hours.", Fact: "%d teams", One: "1 team", N: len(o.teams)},
		{Href: "/roles", Name: "Roles", About: "Who may change what, and a team's own editors."},
		{Href: "/settings", Name: "Settings", About: "How firings are judged, whether changes to the rules are paused, and what --config declares."},
		{Href: "/runs", Name: "Runs", About: "Each replay and what was copied in, and what failed."},
		{Href: "/reports", Name: "Reports", About: "Who gets which report, where, and the weekly digest.", Fact: "%d reports", One: "1 report", N: len(reports)},
		{Href: "/orphans", Name: "Orphans", About: "Notes, tasks, comments and proposals of rules no longer in effect, deleted or renamed: moved to a rule, or deleted."},
	}}
	if !cursor.IsZero() {
		v.Cards[3].Fact, v.Cards[3].At = "Firings are in up to %s.", cursor
	}
	if c := s.shownStatus().Config; c.ErrText("en") != "" {
		v.Cards[2].Warn = "The config file can't be used: the ledger keeps what it last declared."
	}
	if s.role(r) < admin {
		v.Cards = append(v.Cards[:1], v.Cards[2:]...) // Roles, for admins alone
	}
	v.Notify, err = s.notifyStatus(ctx, lang(r))
	return v, err
}

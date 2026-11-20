package serve

import (
	"context"
	"net/http"
	"slices"
	"time"

	"lapilli.dev/internal/ledger"
)

// What the pages say of a team's boundaries of change (bounds.go): a
// proposal's page, its team, a freeze that holds it and a team that owns
// it now that didn't as it was proposed, each action it stops shown
// disabled with why; the Settings page, the freezes and when each holds;
// the Administration page's Settings card, a freeze in effect.

// proposalBounds is what a proposal's page says of its team's boundaries:
// Team, the team that owns its rule now, "" for none; Frozen, the freeze
// that holds it, nil for none; Moved, that one team owned its rule as it
// was proposed (MovedFrom) and another, or none (MovedTo ""), does now, so
// that no one approves it; Unread, that the boundaries can't be read, so
// that nothing they would hold goes ahead meanwhile (fail closed).
type proposalBounds struct {
	Team               string
	Frozen             *frozenView
	Moved              bool
	MovedFrom, MovedTo string
	Unread             bool
	// approver says the reader may approve it, as its team is now
	// (approverOf); teamApprovers, that its team declares approvers.
	approver, teamApprovers bool
}

// boundsOf says what the boundaries say of a proposal, on its page, and
// takes away the forms they stop: approving it while a freeze holds it or
// another team owns it now, asking to undo it while a freeze holds it; and
// both while the boundaries can't be read, said so, the page drawn all
// the same (dropping and closing it go on).
func (s *Server) boundsOf(r *http.Request, p ledger.Proposal, v *proposalView) error {
	ctx := r.Context()
	now, err := s.teamNow(ctx, p)
	if err != nil {
		return err
	}
	v.Team = now.Name
	v.approver = s.approverOf(r, p)
	b, err := s.bounds(ctx)
	if err != nil {
		s.log.Warn("a proposal's page: the change boundaries can't be read; approving and undoing are held", "proposal", p.ID, "err", err)
		v.Unread = true
		v.CanApprove, v.Approve, v.CanUndo, v.Undo = false, "", false, ""
		return nil
	}
	if now.Name != "" {
		v.teamApprovers = len(b.Teams[now.Name].Approvers) > 0
	}
	if p.State == ledger.StateProposed {
		if from, ok := teamMoved(proposalEvidence(p).ownedBy, now); ok {
			v.Moved, v.MovedFrom, v.MovedTo = true, from.Name, now.Name
		}
	}
	if slices.Contains([]string{ledger.StateProposed, ledger.StateApproved, ledger.StateReady, ledger.StateApplied}, p.State) {
		if v.Frozen, err = s.frozenNow(ctx, p); err != nil {
			return err
		}
	}
	if v.Frozen != nil || v.Moved {
		v.CanApprove, v.Approve = false, ""
	}
	if v.Frozen != nil {
		v.CanUndo, v.Undo = false, ""
	}
	return nil
}

// frozenNow is the freeze that holds a proposal now, nil for none: of its
// rule's team now, and, waiting for approval or to be carried out, of the
// teams that owned it as it was proposed; of an applied one, whose undo
// would be asked for now, of its team now.
func (s *Server) frozenNow(ctx context.Context, p ledger.Proposal) (*frozenView, error) {
	b, err := s.bounds(ctx)
	if err != nil {
		return nil, err
	}
	now, bare, err := s.teamsNow(ctx, p)
	if err != nil {
		return nil, err
	}
	w := proposalEvidence(p).ownedBy
	if p.State == ledger.StateApplied {
		w = ownedBy{}
	}
	if f, until, ok := b.frozen(teamsOf(w, now, bare), s.wall()); ok {
		return &frozenView{Title: f.Title, Team: f.Team, Until: until}, nil
	}
	return nil, nil
}

// teamEditsProposal reports whether the reader, an editor of some teams
// but not of every team, edits the team of a proposal's rule, now and as
// it was proposed: they may backtest it again.
func (s *Server) teamEditsProposal(r *http.Request, p ledger.Proposal) bool {
	teams := s.editsTeams(r)
	return len(teams) > 0 && s.proposalInScope(r.WithContext(ledger.WithTeams(r.Context(), teams)), p) == nil
}

// ownedNow is who owns a proposal's rule now, as an undo of it keeps it:
// the undo is that team's, asked for now; and the team its name and
// labels give it without its note, when that is another (no one team's).
func (s *Server) ownedNow(ctx context.Context, p ledger.Proposal) (ownedBy, error) {
	now, bare, err := s.teamsNow(ctx, p)
	if err != nil {
		return ownedBy{Owned: true}, err
	}
	w := ownedBy{Owned: true}
	for _, t := range []teamRef{now, bare} {
		if t.ID != 0 && !slices.Contains(w.Owners, t) {
			w.Owners = append(w.Owners, t)
		}
	}
	return w, nil
}

// freezeView is a change freeze as the Settings page lists it: its title,
// its team ("" for everyone's), how it repeats, and whether it holds now
// (Until, when it ends), or when it next does (Next to NextEnd); neither
// once it is over.
type freezeView struct {
	Title, Team   string
	W             ledger.Window
	Until         time.Time
	Next, NextEnd time.Time
}

// freezesView are the freezes the boundaries hold, as the Settings page
// lists them.
func (s *Server) freezesView(b *bounds) []freezeView {
	at := s.wall()
	var out []freezeView
	for _, f := range b.Freezes {
		x := freezeView{Title: f.Title, Team: f.Team, W: f.w}
		if end, ok := f.w.EndOf(at); ok {
			x.Until = end
		} else if start, end, ok := f.w.NextAfter(at); ok {
			x.Next, x.NextEnd = start, end
		}
		out = append(out, x)
	}
	return out
}

// boundsNoTeam are the names the boundaries give approvers, changes_in or
// a change freeze to that no team has: the file's team of that name
// wasn't kept (the file's problems say why), so what is given to it holds
// nothing. The Settings page says so.
func (s *Server) boundsNoTeam(ctx context.Context, b *bounds) ([]string, error) {
	o, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	add := func(name string) {
		if _, ok := o.team(name); name != "" && !ok && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	for _, n := range sortedKeys(b.Teams) {
		add(n)
	}
	for _, f := range b.Freezes {
		add(f.Team)
	}
	return out, nil
}

// boundsShown is what the Settings page says of the boundaries: the
// freezes, the names given boundaries no team has, and, when they can't
// be read, that (fail closed: nothing they would hold goes ahead).
func (s *Server) boundsShown(ctx context.Context, v *settingsView) error {
	b, err := s.bounds(ctx)
	if err != nil {
		s.log.Warn("the Settings page: the change boundaries can't be read", "err", err)
		v.BoundsUnread = true
		return nil
	}
	v.Freezes = s.freezesView(b)
	v.BoundsNoTeam, err = s.boundsNoTeam(ctx, b)
	return err
}

// frozenUntil is when the freezes that hold now end, everyone's or a
// team's, for the Administration page's Settings card: the one that ends
// last, zero for none.
func (s *Server) frozenUntil(ctx context.Context) time.Time {
	b, err := s.bounds(ctx)
	if err != nil {
		return time.Time{}
	}
	var teams []string
	for _, f := range b.Freezes {
		teams = append(teams, f.Team)
	}
	_, until, _ := b.frozen(teams, s.wall())
	return until
}

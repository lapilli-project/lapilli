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
// that no one approves it.
type proposalBounds struct {
	Team               string
	Frozen             *frozenView
	Moved              bool
	MovedFrom, MovedTo string
	// approver says the reader may approve it, as its team is now
	// (approverOf); teamApprovers, that its team declares approvers.
	approver, teamApprovers bool
}

// boundsOf says what the boundaries say of a proposal, on its page, and
// takes away the forms they stop: approving it while a freeze holds it or
// another team owns it now, asking to undo it while a freeze holds it.
func (s *Server) boundsOf(r *http.Request, p ledger.Proposal, v *proposalView) error {
	ctx := r.Context()
	now, err := s.teamNow(ctx, p)
	if err != nil {
		return err
	}
	v.Team = now.Name
	v.approver = s.approverOf(r, p)
	if b, err := s.bounds(ctx); err == nil && now.Name != "" {
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
	now, err := s.teamNow(ctx, p)
	if err != nil {
		return nil, err
	}
	w := proposalEvidence(p).ownedBy
	if p.State == ledger.StateApplied {
		w = ownedBy{}
	}
	if f, until, ok := b.frozen(teamsOf(w, now), s.wall()); ok {
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
// the undo is that team's, asked for now.
func (s *Server) ownedNow(ctx context.Context, p ledger.Proposal) (ownedBy, error) {
	now, err := s.teamNow(ctx, p)
	if err != nil || now.ID == 0 {
		return ownedBy{Owned: true}, err
	}
	return ownedBy{Owned: true, Owners: []teamRef{now}}, nil
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

// freezesView are the freezes the ledger holds, as the Settings page lists
// them, and an error when they can't be read.
func (s *Server) freezesView(ctx context.Context) ([]freezeView, error) {
	b, err := s.bounds(ctx)
	if err != nil {
		return nil, err
	}
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
	return out, nil
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

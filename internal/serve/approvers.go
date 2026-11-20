package serve

import (
	"net/http"
	"slices"
	"strings"

	"lapilli.dev/internal/ledger"
)

// Who may approve a change, as it is (docs/design/ui.md, "First use"): the
// Roles page said "(the admins)" whether or not changes were on, and a
// proposal's page said only that the reader may not, so that an admin
// alone, who had made a proposal, didn't learn that no one could approve
// it. Both say who may, as approver allows: those --approvers names, or
// the admins without it, or no one, and why.

// approversIn are who may approve a change in effect: No, why no one may
// ("changes", "read-only", "roles"); else From, the flag that names them
// ("approvers", or "admins" when --approvers isn't given), and List, them,
// as the flag has them.
type approversIn struct {
	No, From string
	List     []string
}

func (s *Server) approversIn() approversIn {
	switch {
	case !s.cfg.Changes:
		return approversIn{No: "changes"}
	case s.cfg.ReadOnly:
		return approversIn{No: "read-only"}
	case len(s.cfg.Approvers) > 0:
		return approversIn{From: "approvers", List: s.cfg.Approvers}
	case len(s.cfg.Admins) > 0:
		return approversIn{From: "admins", List: s.cfg.Admins}
	}
	return approversIn{No: "roles"}
}

// mayApprove is who may approve a proposal waiting for approval, as its
// page says it: who may approve a change (In), but the one it was made by
// or for and those who approved it already, Left, as the flag names them.
// Named is whether the page says who they are, and why none may: to an
// admin alone, who reads the flags on the Roles page. Anyone else is told
// only that an approver other than its maker approves it, or that no one
// can yet and an admin sees why: who the approvers and the admins are, and
// whether its maker is the only one, are the Roles page's, an admin's.
// Open says one of them may be anyone, as listed takes them: a group,
// everyone of a domain, everyone, or a name with neither user: nor group:,
// which is a group too where there is one of that name. Need is how many
// more approvals it needs; Approved, that someone approved it. When fewer
// are left than it needs, and none is open, it can't be approved until the
// flags change.
//
// Team is its team, whose approvers --config declares, when it is one
// team's (bounds.go): TeamLeft, of those, the ones left, TeamOpen, that
// one of them may be more than a person. They may approve it beside the
// approvers of every team, and without them.
type mayApprove struct {
	In       approversIn
	Named    bool
	Left     []string
	Open     bool
	Need     int
	Approved bool
	Team     string
	TeamLeft []string
	TeamOpen bool
}

// Stuck says fewer are left who may approve it than it needs: none, when
// no one approved it yet, is No one. One listed both for every team and
// for its team is one person.
func (m mayApprove) Stuck() bool {
	return m.In.No == "" && !m.Open && !m.TeamOpen && len(m.everyone()) < m.Need
}

// everyone are those left who may approve it, each once.
func (m mayApprove) everyone() []string {
	out := slices.Clone(m.Left)
	for _, x := range m.TeamLeft {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// NoOne says no one at all may approve it but the one it was made by or
// for.
func (m mayApprove) NoOne() bool { return m.Stuck() && len(m.everyone()) == 0 && !m.Approved }

func (s *Server) mayApproveOf(r *http.Request, p ledger.Proposal, need int) *mayApprove {
	if p.State != ledger.StateProposed || p.Path == ledger.PathNone || p.Kind == ledger.KindUndo {
		return nil
	}
	m := &mayApprove{In: s.approversIn(), Named: s.role(r) == admin, Need: max(0, need-len(p.Approvals)), Approved: len(p.Approvals) > 0}
	if m.In.No == "changes" {
		return nil // the approval's button says so
	}
	out := []string{ledger.FoldName(p.MadeBy)}
	for _, a := range p.Approvals {
		out = append(out, ledger.FoldName(a.By))
	}
	m.Left, m.Open = leftOf(m.In.List, out)
	// Its team's approvers, when one team owned it as it was proposed and
	// owns it now (bounds.go): who may approve it without --approvers too.
	if t, ok := s.teamApproved(r, p); ok {
		if b, err := s.bounds(r.Context()); err == nil && len(b.Teams[t.Name].Approvers) > 0 {
			m.Team = t.Name
			m.TeamLeft, m.TeamOpen = leftOf(b.Teams[t.Name].Approvers, out)
			if m.In.No == "roles" {
				m.In = approversIn{From: "team"}
			}
		}
	}
	return m
}

// leftOf are those of a list who may approve, but those of out (the one it
// was made by or for, and who approved it), as the list names them; open
// says one of them may be more than a person, as listed takes them: a
// group, everyone of a domain, everyone, or a name with neither user: nor
// group:, which is a group too where there is one of that name.
func leftOf(list, out []string) (left []string, open bool) {
	for _, x := range list {
		name, person := strings.CutPrefix(x, "user:")
		switch {
		case x == "*" || strings.HasPrefix(x, "group:") || strings.HasPrefix(x, "*@"):
			// Everyone, a group, everyone of a domain: who is in them
			// isn't known here.
			open = true
		case slices.Contains(out, ledger.FoldName(name)):
			// The one it was made by or for, or one who approved it. A
			// name with neither prefix is a person or a group: taken for
			// theirs, a group of the same name is passed over.
			continue
		case !person:
			open = true // a person, or a group of the name (listed)
		}
		if !slices.Contains(left, x) {
			left = append(left, x)
		}
	}
	return left, open
}

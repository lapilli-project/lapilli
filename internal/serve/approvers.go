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
// Named is whether the page names them: to an admin alone, who reads them
// on the Roles page; anyone else is told who in words. Open says a group,
// everyone of a domain or everyone is among them, who may be anyone; Need
// is how many more approvals it needs; Approved, that someone approved it.
// When fewer are left than it needs, and none is open, it can't be
// approved until the flags change, and the page says how, to every reader.
type mayApprove struct {
	In       approversIn
	Named    bool
	Left     []string
	Open     bool
	Need     int
	Approved bool
}

// Stuck says fewer are left who may approve it than it needs: none, when
// no one approved it yet, is No one.
func (m mayApprove) Stuck() bool { return m.In.No == "" && !m.Open && len(m.Left) < m.Need }

// NoOne says no one at all may approve it but the one it was made by or
// for.
func (m mayApprove) NoOne() bool { return m.Stuck() && len(m.Left) == 0 && !m.Approved }

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
	for _, x := range m.In.List {
		if x == "*" || strings.HasPrefix(x, "group:") || strings.HasPrefix(x, "*@") {
			// Everyone, a group, everyone of a domain: who is in them
			// isn't known here.
			m.Open = true
		} else if name, _ := strings.CutPrefix(x, "user:"); slices.Contains(out, ledger.FoldName(name)) {
			// The one it was made by or for, or one who approved it. A
			// name with neither prefix is a person or a group: taken for
			// theirs, a group of the same name is passed over.
			continue
		}
		if !slices.Contains(m.Left, x) {
			m.Left = append(m.Left, x)
		}
	}
	return m
}

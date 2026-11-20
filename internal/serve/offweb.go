package serve

import (
	"html/template"
	"net/http"
	"slices"

	"lapilli.dev/internal/ledger"
)

// What a reader may not do, shown, not hidden (docs/design/ui.md): where a
// page's form is for those who may change something, everyone else sees
// one button, disabled, naming what it does, and why beside it, tied to it
// with aria-describedby, as a page's main action is. The server checks
// every write as before: a disabled button is never the only check.

// offFuncs are the templates' ways to say so, and crumbed.
var offFuncs = template.FuncMap{
	// crumbed is the page with its title, and a shorter name for the
	// breadcrumb's last item: a proposal's #id.
	"crumbed": func(p page, title, short string) page {
		p.Title, p.Short = title, short
		return p
	},
	// verdictOf is what a firing's verdict forms take (firings.html,
	// "verdict"): the page, the firing, and where its forms go back to.
	"verdictOf": func(p page, f any, back string) verdictArgs {
		v := verdictArgs{P: p, Back: p.Link(back), Loc: p.Loc}
		switch f := f.(type) {
		case firingLine:
			v.F = f
		case firingRow:
			v.F = f.firingLine
		}
		return v
	},
	// offOf is an action a reader may not take for their role: need is
	// "admin", or else an editor's.
	"offOf": func(p page, label, need, id string) pageAction {
		a := pageAction{Label: label, ID: id, Why: p.Status.Why}
		if a.Why == "" {
			a.Why = "Editors only"
			if need == "admin" {
				a.Why = "Admins only"
			}
		}
		return a
	},
	// again is an action whose why one before it said, by the same id:
	// the button alone, tied to that sentence (a row's, beside its list's
	// bar).
	"again": func(a pageAction) pageAction {
		a.Again = true
		return a
	},
	// offDeclared is an action on what --config declares: changed in the
	// file alone (docs/design/serveops.md).
	"offDeclared": func(label, id string) pageAction {
		return pageAction{Label: label, ID: id, Why: declaredWhy}
	},
	// offWhy is an action no one may take here, for why.
	"offWhy": func(label, id, why string) pageAction {
		return pageAction{Label: label, ID: id, Why: why}
	},
	// quiet is an action drawn less loudly, as a row's is.
	"quiet": func(a pageAction) pageAction {
		a.Quiet = true
		return a
	},
	// offTeam is an action on what is a team's, team (its name, "" for
	// everyone's): a team's editor of another team is told whose editors
	// may.
	"offTeam": func(p page, label, id, team string) pageAction {
		a := pageAction{Label: label, ID: id, Why: p.Status.Why}
		switch {
		case a.Why != "":
		case len(p.Status.Teams) > 0 && team != "":
			a.Why, a.WhyOf = "Only %s's editors", team
		default:
			a.Why = "Editors only"
		}
		return a
	},
}

// frozenWhy is why a change isn't approved, or undone, while a freeze holds
// it; teamApproversWhy, why a reader who approves none of every team's may
// not approve a team's change.
const (
	frozenWhy        = "Held by a change freeze, %s, until it ends"
	teamApproversWhy = "Approvers of every team, and %s's approvers, only"
)

// declaredWhy is why what --config declares isn't changed in the UI.
const declaredWhy = "Declared in config — change it in values"

// offs are a proposal's actions its reader may not take, each with why, as
// the handler that takes it allows: approving it and asking for it to be
// undone (an approver's, person), dropping it (an editor's or an
// approver's, droppable; one ready, an approver's or an admin's), closing
// it (an admin's), making it a task (an editor's, write). Each only where
// someone could, in its state.
func (s *Server) offs(r *http.Request, p ledger.Proposal, v *proposalView) {
	why := s.why(r)
	off := func(label, id, whose string) *pageAction {
		a := &pageAction{Label: label, ID: id, Why: whose}
		switch {
		case why != "":
			a.Why = why
		case !s.cfg.Changes && (id == "approve-why" || id == "undo-why"):
			a.Why = "Changes are off: lapilli serve was started without --changes"
		}
		return a
	}
	// What a team's boundaries stop (bounds.go): named, the freeze or the
	// team, unless the reader may change nothing here at all.
	named := func(a *pageAction, format, name string) *pageAction {
		if a.Why == format {
			a.WhyOf = name
		}
		return a
	}
	if p.State == ledger.StateProposed && !v.CanApprove {
		switch {
		case p.Path == ledger.PathNone:
			v.ApproveOff = &pageAction{Label: "Approve", ID: "approve-why", Why: "For a person to make where the rule is kept: nothing to approve here"}
		case v.Frozen != nil:
			v.ApproveOff = named(off("Approve", "approve-why", frozenWhy), frozenWhy, v.Frozen.Title)
		case v.Moved:
			v.ApproveOff = off("Approve", "approve-why", "Its rule is another team's now than when it was proposed: propose it again")
		case !v.approver && v.Team != "" && v.teamApprovers:
			v.ApproveOff = named(off("Approve", "approve-why", teamApproversWhy), teamApproversWhy, v.Team)
		case v.Bearer && v.approver:
			v.ApproveOff = off("Approve", "approve-why", "Signed in with a bearer token, as an API client is: approve from a browser signed in with a session")
		case v.Own && p.Agent:
			v.ApproveOff = off("Approve", "approve-why", "Your agent proposed this: someone else approves it.")
		case v.Own:
			v.ApproveOff = off("Approve", "approve-why", "You proposed this: someone else approves it.")
		case v.Done:
			v.ApproveOff = off("Approve", "approve-why", "You approved it: it waits for another approver.")
		case v.StaleWhy != nil && v.approver:
			v.ApproveOff = off("Approve", "approve-why", "Held until it is backtested again: it is stale")
		default:
			v.ApproveOff = off("Approve", "approve-why", "Approvers only")
		}
	}
	if undoable(p) && !v.CanUndo {
		why := "Approvers only"
		switch {
		case v.Frozen != nil:
			why = frozenWhy
		case v.Bearer && v.approver:
			why = "Signed in with a bearer token, as an API client is: approve from a browser signed in with a session"
		}
		v.UndoOff = off("Ask to undo it", "undo-why", why)
		if v.Frozen != nil {
			named(v.UndoOff, frozenWhy, v.Frozen.Title)
		}
	}
	if !v.CanDrop {
		switch {
		case p.State == ledger.StateProposed && len(s.editsTeams(r)) > 0 && s.role(r) < editor && !s.approver(r):
			v.DropOff = off("Drop it", "drop-why", "Editors, approvers and whoever proposed it, of its team's editors, only")
		case slices.Contains(closing, p.State):
			v.DropOff = off("Close it", "drop-why", "Admins only")
		case p.State == ledger.StateReady:
			v.DropOff = off("Drop it", "drop-why", "Approvers and admins only")
		case p.State == ledger.StateProposed || p.State == ledger.StateApproved:
			v.DropOff = off("Drop it", "drop-why", "Editors and approvers only")
		}
	}
	if p.TaskID == 0 && s.role(r) < editor {
		v.TaskOff = off("Make it a task", "task-why", "Editors only")
	}
	// Backtesting it again: an editor's, or its team's editors', of one
	// that waits for it.
	if v.StaleWhy != nil && !v.CanAgain {
		v.AgainOff = off("Backtest again", "again-why", "Editors only")
		if v.Team != "" && len(s.editsTeams(r)) > 0 {
			v.AgainOff = named(off("Backtest again", "again-why", "Only %s's editors"), "Only %s's editors", v.Team)
		}
	}
}

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
	if p.State == ledger.StateProposed && !v.CanApprove {
		switch {
		case p.Path == ledger.PathNone:
			v.ApproveOff = &pageAction{Label: "Approve", ID: "approve-why", Why: "For a person to make where the rule is kept: nothing to approve here"}
		case v.Own && p.Agent:
			v.ApproveOff = off("Approve", "approve-why", "Your agent proposed this: someone else approves it.")
		case v.Own:
			v.ApproveOff = off("Approve", "approve-why", "You proposed this: someone else approves it.")
		case v.Done:
			v.ApproveOff = off("Approve", "approve-why", "You approved it: it waits for another approver.")
		default:
			v.ApproveOff = off("Approve", "approve-why", "Approvers only")
		}
	}
	if undoable(p) && !v.CanUndo {
		v.UndoOff = off("Ask to undo it", "undo-why", "Approvers only")
	}
	if !v.CanDrop {
		switch {
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
}

package serve

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// The proposals' pages, as Argo CD shows a change (docs/design/ui.md): a
// proposal's diff, its backtest in a dry run's place, its approval, and
// what happened to it, in order; and the list of them, with facets.

// The proposals are shown for one team's rules, as the tasks are: the bar
// picks the team, and a team's editor sees their team's unless they pick
// all teams.
func init() { teamPages["proposals"] = true }

// stateGroups are where a proposal is, by its state, as its badge says:
// awaiting approval, approved, being applied (checked again and handed to
// a recipe, sent, its pull request open or merged, not yet read back),
// applied, undone, or closed (refused, failed, dropped, or changed in
// review).
var stateGroups = map[string]string{
	ledger.StateProposed: "proposed", ledger.StateApproved: "approved",
	ledger.StateReady: "doing", ledger.StateApplying: "doing", ledger.StateOpened: "doing", ledger.StateMerged: "doing",
	ledger.StateApplied: "applied", ledger.StateUndone: "undone",
	ledger.StateRefused: "closed", ledger.StateFailed: "closed", ledger.StateDropped: "closed", ledger.StateAmended: "closed",
}

// stateBadge is a proposal's state as its badge shows it: Class, its
// colour; Sign, for the eye alone; Word, where it is; Detail, the state
// itself where the word doesn't say it all. Each a text to translate.
type stateBadge struct {
	Class, Sign, Word, Detail string
}

// stateDetails are what each state says beside its group's word.
var stateDetails = map[string]string{
	ledger.StateReady:    "checked again, waiting for a recipe",
	ledger.StateApplying: "sent, not yet read back",
	ledger.StateOpened:   "its pull request is open",
	ledger.StateMerged:   "merged, not yet read back",
	ledger.StateRefused:  "refused",
	ledger.StateFailed:   "failed",
	ledger.StateDropped:  "dropped",
	ledger.StateAmended:  "changed in review",
}

// badgeOf is a state's badge.
func badgeOf(state string) stateBadge {
	b := stateBadge{Detail: stateDetails[state]}
	switch stateGroups[state] {
	case "proposed":
		b.Class, b.Sign, b.Word = "st-wait", "◷", "awaiting approval"
	case "approved":
		b.Class, b.Sign, b.Word = "st-ok", "✓", "approved"
	case "doing":
		b.Class, b.Sign, b.Word = "st-doing", "↻", "being applied"
	case "applied":
		b.Class, b.Sign, b.Word = "st-done", "●", "applied"
	case "undone":
		b.Class, b.Sign, b.Word = "st-undone", "↶", "undone"
	default:
		b.Class, b.Sign, b.Word = "st-closed", "⊘", "closed"
	}
	return b
}

// Badge is a proposal's badge, for the lists.
func (l proposalLine) Badge() stateBadge { return badgeOf(l.State) }

// proposalHead is what a proposal changes, in words, as its page's
// heading says it: a text to translate, and its values.
type proposalHead struct {
	Format string
	Args   []any
}

// Text is the heading in a language; HTML the same, each value set apart
// in <bdi>.
func (h proposalHead) Text(lang string) string        { return tr(lang, h.Format, h.Args...) }
func (h proposalHead) HTML(lang string) template.HTML { return trBDI(lang, h.Format, h.Args...) }

// headOf is what a proposal changes: a rule's for or threshold, from what
// to what; turning it off; deleting it; what it says; undoing another.
func headOf(p ledger.Proposal, ev evidence) proposalHead {
	switch p.Kind {
	case ledger.KindFor:
		now := lineOf(p.After, "for: ")
		if ev.Edit != nil && ev.Edit.For != nil {
			now = verdict.Duration(*ev.Edit.For)
		}
		return proposalHead{"%s's for, %s → %s", []any{p.Rule, cmpOr(lineOf(p.Before, "for: "), "–"), cmpOr(now, "–")}}
	case ledger.KindThreshold:
		if ev.Edit != nil && ev.Edit.From != "" {
			return proposalHead{"%s's threshold, %s → %s", []any{p.Rule, ev.Edit.From, ev.Edit.To}}
		}
		return proposalHead{"A new threshold for %s", []any{p.Rule}}
	case ledger.KindDisable:
		return proposalHead{"Turning %s off", []any{p.Rule}}
	case ledger.KindDelete:
		return proposalHead{"Deleting %s", []any{p.Rule}}
	case ledger.KindSays:
		if ev.Edit != nil && ev.Edit.Unset {
			return proposalHead{"What %s says: its %s, taken out", []any{p.Rule, ev.Edit.Annotation}}
		}
		if ev.Edit != nil {
			return proposalHead{"What %s says: its %s, written anew", []any{p.Rule, ev.Edit.Annotation}}
		}
	case ledger.KindUndo:
		return proposalHead{"Undoing #%d, on %s", []any{p.Follows, p.Rule}}
	}
	return proposalHead{"A change to %s", []any{p.Rule}}
}

// lineOf is what follows prefix on the first line of a rule's text that
// has it: its for, say.
func lineOf(text, prefix string) string {
	for _, l := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(l, prefix); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// btRow is a line of a backtest's table: what is counted, as the rule is
// and as proposed, and the change, signed, better or worse said in words
// when it is either.
type btRow struct {
	Label         string
	Now, Proposed string
	By            moved
}

// btTable is a proposal's backtest as its page shows it: now beside as
// proposed, over its span, from the evidence kept when it was made.
// Worse says, above it, that as proposed it misses incidents it catches
// now.
type btTable struct {
	From, To            time.Time
	Rows                []btRow
	Lost, Gained, Later []string
	Unfinished          bool
	Worse               bool
}

// btTableOf is a backtest's table: the firings, those that caught
// something, the incidents one misses that the other catches, and whether
// it catches them as soon. The evidence holds no time to catch, only which
// incidents are caught later: that is what the last row says.
func btTableOf(bt *backtest) *btTable {
	if bt == nil {
		return nil
	}
	n := strconv.Itoa
	x := &btTable{From: bt.From, To: bt.To, Lost: bt.Lost, Gained: bt.Gained, Later: bt.Later, Unfinished: bt.Unfinished, Worse: len(bt.Lost) > 0}
	x.Rows = []btRow{
		{"Fired", n(bt.Before.Firings), n(bt.After.Firings), moved{On: true, By: bt.After.Firings - bt.Before.Firings}},
		{"Caught something", n(bt.Before.White), n(bt.After.White), moved{On: true, By: bt.After.White - bt.Before.White, Up: true}},
		{"Incidents missed that the other catches", n(len(bt.Gained)), n(len(bt.Lost)), moved{On: true, By: len(bt.Lost) - len(bt.Gained)}},
		{"Incidents caught later", "–", n(len(bt.Later)), moved{On: true, By: len(bt.Later)}},
	}
	return x
}

// proposalStep is a step of what happened to a proposal, as its page's
// history lists it: made, approved (Of of Need), approved or waiting again
// as --approvals changed, moved to a state (with its pull request's Link,
// or why), an undo asked for (#Undo).
type proposalStep struct {
	At          time.Time
	By          string
	Kind, State string
	Of, Need    int
	Link        string
	Reason      string
	Undo        int64
}

// stepsMost is how many of a proposal's steps its page reads.
const stepsMost = 100

// stepsOf are what happened to a proposal, in order: its making and its
// approvals from what the ledger keeps of it, and every step it took
// since, from the changes the ledger keeps (those older than --keep are
// gone); cut says they are, its making's among them.
func (s *Server) stepsOf(r *http.Request, p ledger.Proposal) (steps []proposalStep, cut bool, err error) {
	cs, err := s.l.ProposalSteps(r.Context(), p.ID, stepsMost)
	if err != nil {
		return nil, false, err
	}
	steps = append(steps, proposalStep{At: p.Made, By: p.MadeBy, Kind: "made"})
	prefix := fmt.Sprintf("proposal #%d ", p.ID)
	approvals := 0
	made := false
	for _, c := range cs {
		what := strings.TrimPrefix(c.What, prefix)
		st := proposalStep{At: c.At, By: c.By}
		var after struct {
			Approvals int    `json:"approvals"`
			Need      int    `json:"need"`
			Reason    string `json:"reason"`
			Link      string `json:"link"`
		}
		json.Unmarshal([]byte(c.After), &after)
		switch {
		case what == "made":
			made = true
			continue // from the proposal itself
		case what == "approved":
			approvals++
			st.Kind, st.Of, st.Need = "approved", after.Approvals, after.Need
			if st.Of == 0 {
				st.Of = approvals
			}
		case strings.HasPrefix(what, "approved, as fewer"):
			st.Kind = "promoted"
		case strings.HasPrefix(what, "waits for approval again"):
			st.Kind = "demoted"
		case strings.HasPrefix(what, "asked to be undone, as #"):
			st.Kind = "undo"
			st.Undo, _ = strconv.ParseInt(strings.TrimPrefix(what, "asked to be undone, as #"), 10, 64)
		case stateGroups[what] != "":
			st.Kind, st.State, st.Reason = "state", what, after.Reason
			if what == ledger.StateOpened {
				st.Link = after.Link
			}
		default:
			continue
		}
		steps = append(steps, st)
	}
	// An undo is approved as it is asked for: by whoever asked, then.
	if p.Kind == ledger.KindUndo && approvals == 0 && len(p.Approvals) > 0 {
		steps = append(steps[:1], append([]proposalStep{{At: p.Approvals[0].At, By: p.Approvals[0].By, Kind: "approved", Of: 1, Need: 1}}, steps[1:]...)...)
	}
	slices.SortStableFunc(steps, func(a, b proposalStep) int { return a.At.Compare(b.At) })
	// An undo is made by its asking, kept as a step of the change it
	// undoes: its own steps start with its approval.
	return steps, !made && p.Kind != ledger.KindUndo, nil
}

// Who is who took a step, as a page names them: Lapilli itself and a
// change recipe by what they are.
func (st proposalStep) Who() string {
	switch st.By {
	case ledger.Proposer:
		return "Lapilli"
	case ledger.ChangesCopier:
		return "a change recipe"
	}
	return ""
}

// Name is who took a step, as a page names them in a language.
func (st proposalStep) Name(lang string) string {
	if w := st.Who(); w != "" {
		return tr(lang, w)
	}
	if st.By == "" || st.By == "anonymous" {
		return tr(lang, "anonymous")
	}
	return st.By
}

// Badge is the badge of the state a step moved a proposal to.
func (st proposalStep) Badge() stateBadge { return badgeOf(st.State) }

// fileRows are a file's diff, as a pull request carries it, a line a row:
// its heads (the file's names, where a run starts) as they are, the rest
// by their sign.
func fileRows(diff string) []diffRow {
	var out []diffRow
	for _, l := range textLines(diff) {
		switch {
		case strings.HasPrefix(l, "--- "), strings.HasPrefix(l, "+++ "), strings.HasPrefix(l, "@@"), l == "":
			out = append(out, diffRow{Text: l})
		default:
			out = append(out, diffRow{Op: l[:1], Text: l[1:]})
		}
	}
	return out
}

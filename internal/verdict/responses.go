package verdict

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/firing"
)

// What people did when a rule fired (docs/design/responses.md): each firing's
// response and whether it was useful, found by a Responder that reads the
// pages a paging tool sent and the changes made to the alert's target. The
// Responder lives apart (internal/source), as internal/response reads the
// ledger's types, which read this package.

// What a rule is found noisy by (Options.NoisyBy, --noisy-by).
const (
	// NoisyByCaught is by stones: a rule's firings that caught an incident.
	NoisyByCaught = "caught"
	// NoisyByUseful is by what was done about its firings.
	NoisyByUseful = "useful"
)

// The kinds of a firing's response (FiringResponse.Kind), the first that
// holds.
const (
	ResponseChanged      = "changed"
	ResponseAcknowledged = "acknowledged"
	ResponseUnanswered   = "unanswered"
	// ResponseRoutine is a firing with no page and only routine changes on
	// its target.
	ResponseRoutine = "routine"
	ResponseNone    = "none"
)

// ResponseChange is the change on a firing's target that its response
// rests on, or the earliest routine one, shown with it.
type ResponseChange struct {
	// Kind is rollout, rollback, scale or deploy.
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	// Labels name the target: a workload's namespace and its kind's
	// lower-case name (deployment, statefulset, daemonset), or a deploy's.
	Labels map[string]string `json:"labels,omitempty"`
	// Routine says its target changed so often in the period that one in
	// a span as long as the firing was likely anyway.
	Routine bool `json:"routine"`
}

// ResponsePage is the page a firing was sent as, as the paging tool says.
type ResponsePage struct {
	ID           string    `json:"id"`
	Sent         time.Time `json:"sent"`
	Acknowledged time.Time `json:"acknowledged,omitzero"`
	Resolved     time.Time `json:"resolved,omitzero"`
	// ResolvedBy is alert, when its alert manager resolved it, or person.
	ResolvedBy string `json:"resolved_by,omitempty"`
}

// FiringResponse is what was done about a firing, and whether it was
// useful. No person's name is in it.
type FiringResponse struct {
	Kind string `json:"kind"`
	// Useful is true, false, or nil when it isn't known.
	Useful *bool           `json:"useful"`
	Change *ResponseChange `json:"change,omitempty"`
	Page   *ResponsePage   `json:"page,omitempty"`
}

// RuleResponses is a rule's firings counted by their responses. The five
// kinds are exclusive: they add up to the rule's firings.
type RuleResponses struct {
	Changed      int `json:"changed"`
	Routine      int `json:"routine"`
	Acknowledged int `json:"acknowledged"`
	Unanswered   int `json:"unanswered"`
	None         int `json:"none"`
	// Useful and NotUseful are its firings known to be, and not to be, the
	// rest unknown. Actionable is Useful of both, null when none is known.
	Useful     int      `json:"useful"`
	NotUseful  int      `json:"not_useful"`
	Actionable *float64 `json:"actionable"`
}

// ResponseTotals are what the report read of responses.
type ResponseTotals struct {
	// Pages is how many pages were read, and PagesUnmatched those that
	// were no firing's.
	Pages          int `json:"pages"`
	PagesUnmatched int `json:"pages_unmatched"`
	// Changes are the changes read: rollouts, rollbacks, scales and deploys.
	Changes int `json:"changes"`
	// AutoscalersKnown says the history had the autoscalers' targets; when
	// it hadn't, every scale is counted.
	AutoscalersKnown bool `json:"autoscalers_known"`
	// Rolled says kube-state-metrics was read (--rollouts).
	Rolled bool `json:"-"`
}

// A Responder finds a firing's response. Judge hands it each firing it
// judges: white says it caught an incident, judged that its stone was
// judged by a record, and answer is a person's word on whether it needed
// action (ActionYes, ActionNo or none). within is how far from the firing a
// page may have been sent: two evaluations of its rule. now is the end of
// the period, which a firing still on ends at.
type Responder interface {
	Respond(f firing.Firing, within time.Duration, now time.Time, white, judged bool, answer string) FiringResponse
	// Totals are what was read, and how many pages the firings given so
	// far were no page of.
	Totals() ResponseTotals
}

// Actionable is a rule's useful firings over those useful or not, and false
// when none is.
func (r RuleResult) Actionable() (float64, bool) {
	if r.Responses == nil {
		return 0, false
	}
	n := r.Responses.Useful + r.Responses.NotUseful
	if n == 0 {
		return 0, false
	}
	return float64(r.Responses.Useful) / float64(n), true
}

func (r *RuleResult) countResponse(fr FiringResponse) {
	if r.Responses == nil {
		r.Responses = &RuleResponses{}
	}
	c := r.Responses
	switch fr.Kind {
	case ResponseChanged:
		c.Changed++
	case ResponseRoutine:
		c.Routine++
	case ResponseAcknowledged:
		c.Acknowledged++
	case ResponseUnanswered:
		c.Unanswered++
	default:
		c.None++
	}
	switch {
	case fr.Useful == nil:
	case *fr.Useful:
		c.Useful++
	default:
		c.NotUseful++
	}
}

// finishResponses sets Actionable once the counts are in.
func (r *RuleResult) finishResponses() {
	if r.Responses == nil {
		return
	}
	r.Responses.Actionable = nil
	if p, ok := r.Actionable(); ok {
		r.Responses.Actionable = &p
	}
}

// ByResponsesAlone is how many of the report's firings no record judged
// that what was done about them did: counted, not judged by stones, and
// useful or not useful all the same.
func (r *Report) ByResponsesAlone() int {
	n := 0
	for _, j := range r.Firings {
		if j.NoRecord && j.Response != nil && j.Response.Useful != nil {
			n++
		}
	}
	return n
}

// responsesAlone is the line the no-record lines are followed by, "" when
// no firing was judged by responses alone.
func responsesAlone(r *Report) string {
	if r.Responses == nil {
		return ""
	}
	n := r.ByResponsesAlone()
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("Of them, %s judged by what was done about them alone: they are in the ACTIONABLE of their rules, which --noisy-by useful holds a rule to.", plural(n, "firing was", "firings were"))
}

// responsesText writes the RESPONSES table: what was done about each
// rule's firings.
func responsesText(w io.Writer, r *Report) error {
	if r.Responses == nil {
		return nil
	}
	var rows [][]string
	routine := false
	for _, rr := range r.Rules {
		if rr.Firings == 0 || rr.Responses == nil || rr.Unsupported != "" && !rr.ByHistory() {
			continue
		}
		c := rr.Responses
		changed := fmt.Sprint(c.Changed)
		if c.Routine > 0 {
			changed += fmt.Sprintf(" (%d)", c.Routine)
			routine = true
		}
		share := "–"
		if p, ok := rr.Actionable(); ok {
			share = fmt.Sprintf("%.0f%%", 100*p)
		}
		rows = append(rows, []string{Clean(rr.Rule), fmt.Sprint(rr.Firings), share, changed, fmt.Sprint(c.Acknowledged), fmt.Sprint(c.Unanswered), fmt.Sprint(c.None)})
	}
	t := r.Responses
	fmt.Fprintln(w)
	if len(rows) > 0 {
		rows = slices.Insert(rows, 0, []string{"RULE", "FIRED", "ACTIONABLE", "CHANGED", "ACKNOWLEDGED", "UNANSWERED", "NONE"})
		if err := table(w, rows); err != nil {
			return err
		}
		if routine {
			fmt.Fprintln(w, "  CHANGED counts firings a change on the alert's target followed; in brackets, those whose change was routine, which counts for nothing.")
		}
	} else {
		fmt.Fprintln(w, "No firing has a response to count.")
	}
	if t.Pages > 0 {
		fmt.Fprintf(w, "%s read, %d matched no firing (a rule renamed, labels the paging tool cut).\n", plural(t.Pages, "page", "pages"), t.PagesUnmatched)
	}
	if t.Rolled {
		if t.AutoscalersKnown {
			fmt.Fprintf(w, "%s read; a Deployment or StatefulSet an autoscaler targets isn't counted as scaled.\n", plural(t.Changes, "change", "changes"))
		} else {
			fmt.Fprintf(w, "%s read. No autoscaler was found in the history (kube_horizontalpodautoscaler_info), so every scale is counted, an autoscaler's too.\n", plural(t.Changes, "change", "changes"))
		}
	} else if t.Changes > 0 {
		fmt.Fprintf(w, "%s read.\n", plural(t.Changes, "change", "changes"))
	}
	return nil
}

// ResponseCSVColumns are the columns appended to CSVColumns when the report
// has responses: the firing's response kind and whether it was useful, then
// its rule's counts and actionable share.
var ResponseCSVColumns = []string{"response", "useful", "rule_changed", "rule_routine", "rule_acknowledged", "rule_unanswered", "rule_none", "rule_useful", "rule_not_useful", "rule_actionable"}

// responseCells are a firing's cells of ResponseCSVColumns.
func responseCells(j Judged, rules map[string]RuleResult) []string {
	kind, useful := "", ""
	if j.Response != nil {
		kind = j.Response.Kind
		if j.Response.Useful != nil {
			useful = csvBool(*j.Response.Useful)
		}
	}
	rr := rules[j.Rule]
	c := rr.Responses
	if c == nil {
		c = &RuleResponses{}
	}
	share := ""
	if p, ok := rr.Actionable(); ok {
		share = fmt.Sprintf("%.4g", p)
	}
	return []string{kind, useful, fmt.Sprint(c.Changed), fmt.Sprint(c.Routine), fmt.Sprint(c.Acknowledged), fmt.Sprint(c.Unanswered), fmt.Sprint(c.None),
		fmt.Sprint(c.Useful), fmt.Sprint(c.NotUseful), share}
}

// responseMetrics are the lines of lapilli_rule_responses and
// lapilli_rule_actionable for a rule whose labels are l, `{rule="..."}`.
func responseMetrics(l string, rr RuleResult) (responses []string, actionable string) {
	c := rr.Responses
	if c == nil {
		c = &RuleResponses{}
	}
	for _, x := range []struct {
		kind string
		n    int
	}{{ResponseChanged, c.Changed}, {ResponseRoutine, c.Routine}, {ResponseAcknowledged, c.Acknowledged}, {ResponseUnanswered, c.Unanswered}, {ResponseNone, c.None}} {
		responses = append(responses, fmt.Sprintf(`lapilli_rule_responses%s,response="%s"} %d`, strings.TrimSuffix(l, "}"), x.kind, x.n))
	}
	if p, ok := rr.Actionable(); ok {
		actionable = fmt.Sprintf("lapilli_rule_actionable%s %g", l, p)
	}
	return responses, actionable
}

// hasResponses reports whether the report counted responses.
func (r *Report) hasResponses() bool { return r != nil && r.Responses != nil }

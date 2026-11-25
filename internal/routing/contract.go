package routing

import "fmt"

// DefaultBudget is how many flushes a run looks at when Options.Budget
// doesn't say.
const DefaultBudget = 20_000_000

// spent counts one flush and reports whether the run has to stop: its
// budget is used or its context is done. It says why in Result.Stopped.
func (r *runner) spent() bool {
	if r.halt {
		return true
	}
	r.steps++
	budget := r.o.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	switch {
	case r.steps > budget:
		r.res.Stopped = fmt.Sprintf("more than %d flushes: the notifications after them aren't worked out", budget)
	case r.steps&0x3ff == 0 && r.ctx.Err() != nil:
		r.res.Stopped = "stopped before it was done (" + r.ctx.Err().Error() + "): the notifications after that aren't worked out"
	default:
		return false
	}
	r.halt = true
	return true
}

// dedupKinds are the kinds whose tool dedups on the group key.
var dedupKinds = []string{"pagerduty", "opsgenie", "victorops", "incidentio"}

// DedupsByGroup reports whether an integration kind's tool dedups on the
// group's key: a repeat or a "new alerts" notification isn't a new page
// there, only a firing notification after none, or after all resolved, is.
func DedupsByGroup(kind string) bool {
	for _, k := range dedupKinds {
		if k == kind {
			return true
		}
	}
	return false
}

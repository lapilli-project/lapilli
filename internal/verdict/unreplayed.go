package verdict

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"lapilli.dev/internal/rules"
)

// Unreplayed is a rule that couldn't be replayed, and why: in the JSON of
// a report, and of a comparison, so that a rule left out is seen without
// reading every rule.
type Unreplayed struct {
	Rule string `json:"rule"`
	// Side is, in a comparison, the set of rules it is in: before (those in
	// place) or after (those proposed); none for a rule alike in both,
	// replayed once for both.
	Side   string `json:"side,omitempty"`
	Reason string `json:"reason"`
	// Failed says its replay failed, for a reason of the run: a query
	// failed, as when the history turned it away, or the time ran out.
	// Another run may replay it; the report is of the other rules. A rule
	// not Failed can't be replayed at all (a CloudWatch metric alarm, a rule
	// paused), whatever the run.
	Failed bool `json:"failed"`
}

// UnreplayedRules are the rules of the report that couldn't be replayed,
// those whose replay failed first.
func (r *Report) UnreplayedRules() []Unreplayed {
	return failedFirst(unreplayed(r, ""))
}

// UnreplayedRules are the rules of the comparison that couldn't be
// replayed, those whose replay failed first: a rule changed, before and
// after; one alike in both, replayed once for both, once.
func (c *Comparison) UnreplayedRules() []Unreplayed {
	changed := map[string]bool{}
	for _, rc := range c.Rules {
		changed[rc.Rule] = true
	}
	var out []Unreplayed
	for _, u := range unreplayed(c.Before, "before") {
		if !changed[u.Rule] {
			u.Side = ""
		}
		out = append(out, u)
	}
	for _, u := range unreplayed(c.After, "after") {
		if changed[u.Rule] {
			out = append(out, u)
		}
	}
	return failedFirst(out)
}

func unreplayed(r *Report, side string) []Unreplayed {
	if r == nil {
		return nil
	}
	var out []Unreplayed
	for _, rr := range r.Rules {
		if rr.Unsupported != "" {
			out = append(out, Unreplayed{Rule: rr.Rule, Side: side, Reason: rr.Unsupported, Failed: rules.ReplayFailed(rr.Unsupported)})
		}
	}
	return out
}

// failedFirst puts those whose replay failed first, each by name, a rule
// before as it was before as it is after.
func failedFirst(us []Unreplayed) []Unreplayed {
	out := slices.Clone(us)
	side := map[string]int{"": 0, "before": 1, "after": 2}
	slices.SortStableFunc(out, func(a, b Unreplayed) int {
		switch {
		case a.Failed != b.Failed && a.Failed:
			return -1
		case a.Failed != b.Failed:
			return 1
		case a.Rule != b.Rule:
			return strings.Compare(a.Rule, b.Rule)
		}
		return side[a.Side] - side[b.Side]
	})
	return out
}

// shownFirst is how many rules whose replay failed are named at the top;
// all of them are listed with the rest below.
const shownFirst = 10

// partial writes, at the top, the rules whose replay failed: what follows
// leaves them out, and a long report shouldn't hide that at its foot. of
// is how many rules there are, 0 to leave it unsaid.
func partial(w io.Writer, us []Unreplayed, of int) {
	var failed []Unreplayed
	for _, u := range us {
		if u.Failed {
			failed = append(failed, u)
		}
	}
	if len(failed) == 0 {
		return
	}
	if of > 0 {
		fmt.Fprintf(w, "! %d of %d rules couldn't be replayed, and are left out of what follows:\n", len(failed), of)
	} else {
		fmt.Fprintf(w, "! %d rules couldn't be replayed, and are left out of what follows:\n", len(failed))
	}
	for i, u := range failed {
		if i == shownFirst {
			fmt.Fprintf(w, "  and %d more, listed below\n", len(failed)-shownFirst)
			break
		}
		name := Clean(u.Rule)
		if u.Side != "" {
			name += " (" + u.Side + ")"
		}
		fmt.Fprintf(w, "  %s: %s\n", name, Clean(u.Reason))
	}
}

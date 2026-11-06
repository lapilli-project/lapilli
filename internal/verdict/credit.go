package verdict

import "strings"

// SpareAfter is how many incidents a rule must have caught before it can be
// called spare: another rule beating it once proves little.
const SpareAfter = 2

// severityRank orders the severities most rules use. Others are compared
// only for being the same.
var severityRank = map[string]int{"critical": 3, "warning": 2, "info": 1}

// atLeast reports whether an alert of severity a pages at least as loudly as
// one of severity b, as far as anyone can tell from the labels.
func atLeast(a, b string) bool {
	if a == b {
		return true
	}
	x, y := severityRank[strings.ToLower(a)], severityRank[strings.ToLower(b)]
	return x > 0 && y > 0 && x >= y
}

// Credit works out what each rule adds to the others, which a rule's
// precision can't say: a rule that is right every time, but always after
// another, adds nothing.
//
// Incidents is how many incidents a rule caught, and First and Alone how many
// of them it caught before every other rule, or when no other did. A noisy
// rule's catch is likely chance, so it doesn't count beside one by a rule that
// isn't noisy: it takes nothing from that rule's First or Alone, and earns
// none of its own there.
//
// A rule is Spare when it caught SpareAfter incidents or more and, for each,
// another rule covers it: one that isn't noisy, can still fire, caught it as
// soon or sooner, and is of the same severity or above. Of rules that caught it
// at the same moment, the first by name covers the rest, and isn't covered by
// them; a noisy rule never covers, and any rule beside it covers it. So covering
// has no cycles, and every spare rule could be taken away at once: on each
// incident a rule that isn't spare remains, as soon and as loud.
//
// inEffect, when not nil, says which rules can still fire. A rule taken away
// since has what it caught counted, but covers nothing and isn't spare.
func Credit(rs []RuleResult, incs []IncidentResult, inEffect func(rule string) bool) {
	index := make(map[string]int, len(rs))
	covered := make([]bool, len(rs))
	for i := range rs {
		rs[i].Incidents, rs[i].First, rs[i].Alone, rs[i].Spare = 0, 0, 0, false
		index[rs[i].Rule] = i
		covered[i] = true
	}
	live := func(rule string) bool {
		i, ok := index[rule]
		return ok && rs[i].Unsupported == "" && (inEffect == nil || inEffect(rule))
	}
	for _, in := range incs {
		var counted []Catch
		for _, c := range in.CaughtBy {
			if !c.Noisy {
				counted = append(counted, c)
			}
		}
		if len(counted) == 0 {
			counted = in.CaughtBy
		}
		for _, c := range in.CaughtBy {
			i, ok := index[c.Rule]
			if !ok {
				continue
			}
			rs[i].Incidents++
			cover := false
			for _, o := range in.CaughtBy {
				if o.Rule == c.Rule || o.Noisy || !live(o.Rule) || !atLeast(rs[index[o.Rule]].Severity, rs[i].Severity) {
					continue
				}
				cover = cover || o.Delay < c.Delay || (o.Delay == c.Delay && (c.Noisy || o.Rule < c.Rule))
			}
			covered[i] = covered[i] && cover
		}
		for _, c := range counted {
			i, ok := index[c.Rule]
			if !ok {
				continue
			}
			first := true
			for _, o := range counted {
				first = first && (o.Rule == c.Rule || c.Delay < o.Delay)
			}
			if first {
				rs[i].First++
			}
			if len(counted) == 1 {
				rs[i].Alone++
			}
		}
	}
	for i := range rs {
		rs[i].Spare = covered[i] && rs[i].Incidents >= SpareAfter && live(rs[i].Rule)
	}
}

// Credit works out what each rule in the report adds to the others. When the
// report is of some of the rules, only how many incidents each caught can be
// said.
func (r *Report) Credit(inEffect func(rule string) bool) {
	Credit(r.Rules, r.Incidents, inEffect)
	if r.SomeRules {
		for i := range r.Rules {
			r.Rules[i].First, r.Rules[i].Alone, r.Rules[i].Spare = 0, 0, false
		}
	}
}

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
// a rule that isn't noisy caught it as soon or sooner, at the same severity
// or above: taken away, nothing would have been missed, or caught later, or
// by something quieter.
func Credit(rs []RuleResult, incs []IncidentResult) {
	index := make(map[string]int, len(rs))
	covered := make([]bool, len(rs))
	for i := range rs {
		rs[i].Incidents, rs[i].First, rs[i].Alone, rs[i].Spare = 0, 0, 0, false
		index[rs[i].Rule] = i
		covered[i] = true
	}
	severity := func(rule string) string {
		if i, ok := index[rule]; ok {
			return rs[i].Severity
		}
		return ""
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
				cover = cover || (o.Rule != c.Rule && !o.Noisy && o.Delay <= c.Delay && atLeast(severity(o.Rule), rs[i].Severity))
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
		rs[i].Spare = covered[i] && rs[i].Incidents >= SpareAfter
	}
}

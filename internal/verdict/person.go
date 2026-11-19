package verdict

import (
	"cmp"
	"slices"
	"time"

	"lapilli.dev/internal/firing"
)

// A person's verdict on one firing (docs/design/judging.md, item 3): that
// it is unrelated to the incidents it caught, which turns that firing
// alone black, and whether it needed someone to act, which changes no
// stone. A verdict is kept by its firing's alert and start, not by an id:
// a run replaces the firings of its period, and a replay may start one a
// step later, merge two, or part one. So each verdict finds its firing
// again by one rule (Place), in lapilli serve's ledger and in every
// replay judged in memory alike, on both sides of a change.

// MaxUnrelated is how many incidents a verdict of "unrelated" keeps: a
// firing that caught more is refused it.
const MaxUnrelated = 16

// The answers to "did it need action?": none is "".
const (
	ActionYes = "yes"
	ActionNo  = "no"
)

// Verdict is a person's word on one firing, as judging takes it.
type Verdict struct {
	// Rule, Labels and Start are its firing's alert and start: what keeps
	// it.
	Rule   string
	Labels map[string]string
	Start  time.Time
	// End is its firing's end when it was said: zero for one still on.
	End time.Time
	// Unrelated are the incidents it was said to be unrelated to, by id:
	// none for none.
	Unrelated []string
	// Action is ActionYes, ActionNo or "".
	Action string
	// At is when it was last said: between two verdicts of other alerts
	// that come to one firing, the latest holds it.
	At time.Time
}

// Placed is what a firing has of the verdicts that came to it: the
// incidents it is unrelated to, of every one of them (so that a replay
// that merges two firings brings back neither's word), and the answer of
// the one that holds it.
type Placed struct {
	Unrelated []string
	Action    string
	// Holds is the verdict that holds it, as an index in the verdicts
	// placed; Others, the others that came to it, which judge nothing
	// but their "unrelated".
	Holds  int
	Others []int
}

// Place gives each verdict its firing among fs (in firing.Sort's order),
// by the rule of a move (docs/design/judging.md, "Through judging
// again"): the firing of its alert that starts at its start; else, of the
// firings of its rule with the same values of match, the one of its own
// alert on at its start, the first of its own alert that starts within
// the span it was said of, then the same of the other alerts, in that
// order. The span is its start to its end as kept, or, said while its
// firing was on, to its start plus reach of its rule (its "for" and
// --refire), so that it never reaches a firing hours later. at is, for
// each verdict, the index in fs of its firing, -1 for none; on is, for
// each firing given one, what it has of them (Placed).
//
// Two verdicts may come to one firing (a replay merged two, or the rule
// came to another alert's): the one said of that very alert holds it, of
// its very start first; between two of other alerts, or two of the same
// alert and neither of its start, the one said latest.
func Place(vs []Verdict, fs []firing.Firing, match []string, reach func(rule string) time.Duration) (at []int, on map[int]*Placed) {
	at = make([]int, len(vs))
	on = map[int]*Placed{}
	if len(vs) == 0 {
		return at, on
	}
	type alert struct{ rule, labels string }
	byAlert := map[alert][]int{}
	byKey := map[string][]int{}
	for i, f := range fs {
		a := alert{f.Rule, labelKey(f.Labels)}
		byAlert[a] = append(byAlert[a], i)
		k := EpisodeKey(f.Rule, f.Labels, match)
		byKey[k] = append(byKey[k], i)
	}
	for vi, v := range vs {
		own := byAlert[alert{v.Rule, labelKey(v.Labels)}]
		at[vi] = place(v, fs, own, byKey[EpisodeKey(v.Rule, v.Labels, match)], reach)
	}
	// Who holds each firing, and what it has of every verdict that came.
	holders := map[int][]int{}
	for vi, i := range at {
		if i >= 0 {
			holders[i] = append(holders[i], vi)
		}
	}
	for i, vis := range holders {
		key := labelKey(fs[i].Labels)
		// Said of its very alert and start, then of its alert, then the
		// latest said.
		rank := func(v Verdict) int {
			switch {
			case labelKey(v.Labels) != key:
				return 0
			case v.Start.Equal(fs[i].Start):
				return 2
			}
			return 1
		}
		best := slices.MaxFunc(vis, func(a, b int) int {
			return cmp.Or(cmp.Compare(rank(vs[a]), rank(vs[b])), vs[a].At.Compare(vs[b].At), cmp.Compare(b, a))
		})
		p := &Placed{Holds: best, Action: vs[best].Action}
		for _, vi := range vis {
			if vi != best {
				p.Others = append(p.Others, vi)
			}
			for _, id := range vs[vi].Unrelated {
				if !slices.Contains(p.Unrelated, id) {
					p.Unrelated = append(p.Unrelated, id)
				}
			}
		}
		slices.Sort(p.Others)
		slices.Sort(p.Unrelated)
		on[i] = p
	}
	return at, on
}

// place is the firing a verdict finds, of fs: own are its own alert's
// firings, same those of its rule with the same values of the match, both
// in fs's order.
func place(v Verdict, fs []firing.Firing, own, same []int, reach func(string) time.Duration) int {
	for _, i := range own {
		if fs[i].Start.Equal(v.Start) {
			return i
		}
	}
	until := v.End
	if until.IsZero() {
		var d time.Duration
		if reach != nil {
			d = reach(v.Rule)
		}
		until = v.Start.Add(d)
	}
	onAt := func(f firing.Firing) bool {
		return !f.Start.After(v.Start) && (f.End.IsZero() || f.End.After(v.Start))
	}
	within := func(f firing.Firing) bool { return !f.Start.Before(v.Start) && !f.Start.After(until) }
	var others []int
	for _, i := range same {
		if !slices.Contains(own, i) {
			others = append(others, i)
		}
	}
	for _, of := range [][]int{own, others} {
		for _, i := range of {
			if onAt(fs[i]) {
				return i
			}
		}
		for _, i := range of {
			if within(fs[i]) {
				return i
			}
		}
	}
	return -1
}

// VerdictReach is how far after its start a verdict said while its firing was on
// may find it again: its rule's "for" and the --refire o gives.
func (o Options) VerdictReach(forOf func(rule string) time.Duration) func(rule string) time.Duration {
	return func(rule string) time.Duration {
		var d time.Duration
		if forOf != nil {
			d = max(forOf(rule), 0)
		}
		return d + o.RefireWithin
	}
}

// NotJudged reports whether a rule entry says rule is judged by nothing
// (judged: false, docs/design/judging.md, "Rules judged by nothing"):
// its firings are counted, never white nor black, and catch nothing.
// Entries are read for it with named signals in effect or none.
func (o Options) NotJudged(rule string) bool { return o.Signals.notJudged(rule) }

func (s *Signals) notJudged(rule string) bool {
	if s == nil || !s.unjudged {
		return false
	}
	i := s.entryAt(rule)
	return i >= 0 && s.Entries[i].NotJudged()
}

// NotJudged reports whether the entry says its rules are judged by nothing.
func (e RuleEntry) NotJudged() bool { return e.Judged != nil && !*e.Judged }

// NotJudgedRules reports whether any entry says its rules are judged by
// nothing.
func (s *Signals) NotJudgedRules() bool { return s != nil && s.unjudged }

package verdict

import (
	"maps"
	"math"
	"slices"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// NearBy is how close a rule's value has to come to the number its
// condition compares it with, as a share of that number, to have nearly
// caught an incident.
const NearBy = rules.NearBy

// NearMiss is a rule that nearly caught an incident: its condition held from
// From, counted from the incident's start, but either not for its "for", so
// it never fired, or only so that it fired after the incident was over
// (Late); or, by Value, it didn't hold, but its value came close.
type NearMiss struct {
	Incident string            `json:"incident"`
	Rule     string            `json:"rule"`
	Labels   map[string]string `json:"labels,omitempty"` // the alert's, while it held
	From     time.Duration     `json:"from_ns"`
	// Held is how long the condition held, when that was under its "for";
	// when Late, Fired is when the rule did fire, from the incident's start.
	Held  time.Duration `json:"held_ns,omitempty"`
	Late  bool          `json:"late,omitempty"`
	Fired time.Duration `json:"fired_ns,omitempty"`
	For   time.Duration `json:"for_ns"`
	// NewFor is the longest "for" that would have caught the incident; the
	// rule would then have fired Delay from its start.
	NewFor time.Duration `json:"new_for_ns"`
	Delay  time.Duration `json:"delay_ns"`
	// Before and After are the rule's stones over the period with its "for"
	// and with NewFor; After is nil when that wasn't worked out.
	Before RuleResult  `json:"before"`
	After  *RuleResult `json:"after,omitempty"`
	// Value, when set, is how the rule nearly caught it by its value; NewFor
	// is then its own "for", and From when the value passed Value.New.
	Value *ByValue `json:"value,omitempty"`
}

// ByValue is how a rule nearly caught an incident by its value: its
// condition compares a value with Bound, and the value came within NearBy
// of it for the rule's "for", but didn't pass it; or, on the right of
// unless, it did, and so kept the rule quiet.
type ByValue struct {
	Op    string  `json:"op"` // >, >=, < or <=
	Bound float64 `json:"bound"`
	// Of is the expression compared, cut short, when the rule's condition
	// compares more than one with a number; Unless, that it is on the right
	// of unless.
	Of     string `json:"of,omitempty"`
	Unless bool   `json:"unless,omitempty"`
	// Reached is the closest to Bound the value stayed for the rule's
	// "for", when that could be read, and New the round number that would
	// have caught the incident.
	Reached *float64 `json:"reached,omitempty"`
	New     float64  `json:"new"`
}

// Compared is the comparison as the rule makes it, as a person reads it:
// "> 0.9", "errors > 0.9", "unless requests < 10".
func (v ByValue) Compared() string { return v.with(v.Bound) }

// Changed is the comparison with the new number.
func (v ByValue) Changed() string { return v.with(v.New) }

func (v ByValue) with(n float64) string {
	s := v.Op + " " + Number(n)
	if v.Of != "" {
		s = v.Of + " " + s
	}
	if v.Unless {
		s = "unless " + s
	}
	return s
}

// Kept is how much of the rule a near miss keeps: of its "for", or of its
// number, as a share.
func (nm NearMiss) Kept() float64 {
	if nm.Value != nil {
		return 1 - math.Abs(nm.Value.Bound-nm.Value.New)/math.Abs(nm.Value.Bound)
	}
	if nm.For <= 0 {
		return 0
	}
	return float64(nm.NewFor) / float64(nm.For)
}

// Nearly finds, of the stretches a rule's condition held (firings with no
// "for", from the first evaluation that held to after the last), the one
// that would have caught the incident with the longest "for" short of the
// rule's own. every is how often the rule was evaluated. ok is false when no
// shorter "for" would have caught it.
func Nearly(in incident.Incident, held []firing.Firing, ruleFor, every time.Duration, o Options) (NearMiss, bool) {
	var best NearMiss
	found := false
	from, to := in.Start.Add(-o.Lead), in.EndOr(o.UnknownLen)
	// The longest "for" short of the rule's own, in whole evaluations.
	longest := (ruleFor - 1) / every * every
	for _, f := range held {
		if f.End.IsZero() || !in.Labels.Agree(f.Labels, o.Match) {
			continue
		}
		// With a "for" of d it fires at Start+d, if it still held then, and
		// catches the incident if that is in [from, to].
		length := f.End.Sub(f.Start)
		d := min(length-every, to.Sub(f.Start), longest)
		if d < 0 {
			continue
		}
		d = d / every * every
		at := f.Start.Add(d)
		if at.Before(from) {
			continue
		}
		delay := at.Sub(in.Start)
		if found && (d < best.NewFor || d == best.NewFor && delay >= best.Delay) {
			continue
		}
		best = NearMiss{Incident: in.ID, Labels: f.Labels, From: f.Start.Sub(in.Start), For: ruleFor, NewFor: d, Delay: delay}
		if fires := (ruleFor + every - 1) / every * every; length-every >= fires {
			// It held long enough: it fired, once its "for" was up, too late.
			best.Late = true
			best.Fired = f.Start.Add(fires).Sub(in.Start)
		} else {
			best.Held = length
		}
		found = true
	}
	return best, found
}

// NearlyBy finds, of a rule whose condition compares a value with a bound,
// whether a round number past the value, within NearBy of the bound, would
// have caught the incident: the value at each evaluation, every apart, is in
// series, from start: what held then may have held, and fired, before, and
// catches nothing (firing.After). For a range, the value meets its other
// end as it is. It fires as the replay would fire it with that number, its
// "for" and keep_firing_for, and catches as Catches says. ok is false when
// no such number would have. Of several label sets it keeps the one whose
// number changes least.
func NearlyBy(in incident.Incident, series []prom.Series, r rules.Rule, b rules.Bound, start time.Time, every time.Duration, o Options) (NearMiss, bool) {
	if b.Value == 0 || math.IsNaN(b.Value) || math.IsInf(b.Value, 0) || every <= 0 || r.For < 0 {
		return NearMiss{}, false // nothing is within a share of nothing
	}
	from, to := in.Start.Add(-o.Lead), in.EndOr(o.UnknownLen)
	n := int((r.For + every - 1) / every) // evaluations after the first, to fire
	span := time.Duration(n)*every + every/2
	var best NearMiss
	found := false
	for _, s := range series {
		l := maps.Clone(s.Labels)
		delete(l, "__name__")
		maps.Copy(l, r.Labels)
		// A value that isn't a number doesn't hold: it breaks a stretch.
		smp := slices.DeleteFunc(slices.Clone(s.Samples), func(x prom.Sample) bool { return math.IsNaN(x.V) })
		if len(smp) == 0 || !in.Labels.Agree(l, o.Match) {
			continue
		}
		// For a range, a value its other end alone decides is as near holding,
		// or as far from it, as can be.
		if b.Rest != nil {
			best, worst := math.Inf(1), math.Inf(-1)
			if !b.Above() {
				best, worst = worst, best
			}
			for i := range smp {
				switch rest := b.Rest.Holds(smp[i].V); {
				case b.Either && rest:
					smp[i].V = best
				case !b.Either && !rest:
					smp[i].V = worst
				}
			}
		}
		// The closest to the bound the value stayed, n+1 evaluations in a row,
		// ending in [from, to]: the least of them above, the most below.
		reached, any := 0.0, false
		// q is a sliding window's indexes, their values in order; worse is
		// whether x is no closer to holding than y.
		var q []int
		worse := func(x, y float64) bool {
			if b.Above() {
				return x <= y
			}
			return x >= y
		}
		for k := range smp {
			for len(q) > 0 && worse(smp[k].V, smp[q[len(q)-1]].V) {
				q = q[:len(q)-1]
			}
			q = append(q, k)
			for q[0] < k-n {
				q = q[1:]
			}
			if k < n || smp[k].T.Before(from) || smp[k].T.After(to) || smp[k].T.Sub(smp[k-n].T) > span {
				continue
			}
			if v := smp[q[0]].V; !any || worse(reached, v) && reached != v {
				reached, any = v, true
			}
		}
		if !any || b.Holds(reached) {
			continue // it held, and fired before the incident if at all
		}
		if math.IsInf(reached, 0) {
			continue // its other end never held
		}
		nb := rules.Bound{Op: b.Op, Value: rules.Round(reached, b)}
		if far := b.Furthest(); b.Above() && !(nb.Value >= far) || !b.Above() && !(nb.Value <= far) {
			continue // more than a fifth short
		}
		// When it would have fired with that, as the replay fires: from the
		// evaluations that held, with its "for" and keep_firing_for.
		held := prom.Series{Labels: s.Labels, Samples: slices.DeleteFunc(slices.Clone(smp), func(x prom.Sample) bool { return !nb.Holds(x.V) })}
		wait := time.Duration(n) * every
		fs := firing.FromSeries(r.Name, r.Labels, firing.After([]prom.Series{held}, start, every, r.KeepFiringFor), every,
			func(p time.Time) time.Time { return p.Add(wait) }, r.KeepFiringFor, smp[len(smp)-1].T.Add(every))
		for _, f := range fs {
			if !Catches(f, in, o) {
				continue
			}
			nm := NearMiss{Incident: in.ID, Labels: f.Labels, From: f.Start.Add(-wait).Sub(in.Start), For: r.For, NewFor: r.For,
				Delay: f.Start.Sub(in.Start), Value: &ByValue{Op: b.Op, Bound: b.Value, Of: b.Of, Reached: &reached, New: nb.Value}}
			if !found || nm.Kept() > best.Kept() || nm.Kept() == best.Kept() && nm.Delay < best.Delay {
				best, found = nm, true
			}
			break
		}
	}
	return best, found
}

// NearlyAt is whether a rule with b's number at n in place of its own,
// firing as fired says, would have caught the incident: by the first of
// those firings that catches it, as Catches says. every is how often the
// rule was evaluated.
func NearlyAt(in incident.Incident, fired []firing.Firing, r rules.Rule, b rules.Bound, n float64, every time.Duration, o Options) (NearMiss, bool) {
	f, ok := FirstCatch(in, fired, o)
	if !ok || every <= 0 {
		return NearMiss{}, false
	}
	wait := (r.For + every - 1) / every * every
	return NearMiss{Incident: in.ID, Labels: f.Labels, From: f.Start.Add(-wait).Sub(in.Start), For: r.For, NewFor: r.For,
		Delay: f.Start.Sub(in.Start), Value: &ByValue{Op: b.Op, Bound: b.Value, Of: b.Of, Unless: b.Unless, New: n}}, true
}

// FiringDuring reports whether, of fired, one with labels that agree with
// the incident's is firing at some time it could catch it: from its lead to
// its end. A rule holding more fires at more times, so this holds for any
// number further than one it holds for, as catching it needn't: a number
// further may fire before the incident instead, and one further still fire
// again within it.
func FiringDuring(in incident.Incident, fired []firing.Firing, o Options) bool {
	from, to := in.Start.Add(-o.Lead), in.EndOr(o.UnknownLen)
	for _, f := range fired {
		if !f.Start.After(to) && (f.End.IsZero() || f.End.After(from)) && in.Labels.Agree(f.Labels, o.Match) {
			return true
		}
	}
	return false
}

// FirstCatch is the first of fired that catches the incident, as Catches
// says.
func FirstCatch(in incident.Incident, fired []firing.Firing, o Options) (firing.Firing, bool) {
	first := -1
	for i, f := range fired {
		if Catches(f, in, o) && (first < 0 || f.Start.Before(fired[first].Start)) {
			first = i
		}
	}
	if first < 0 {
		return firing.Firing{}, false
	}
	return fired[first], true
}

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
const NearBy = 0.2

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
// of it for the rule's "for", but didn't pass it.
type ByValue struct {
	Op    string  `json:"op"` // >, >=, < or <=
	Bound float64 `json:"bound"`
	// Reached is the closest to Bound the value stayed for the rule's
	// "for", and New the round number short of that which would have caught
	// the incident.
	Reached float64 `json:"reached"`
	New     float64 `json:"new"`
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
// series. ok is false when no such number would have. Of several label sets
// it keeps the one whose number changes least.
func NearlyBy(in incident.Incident, series []prom.Series, ruleLabels map[string]string, b rules.Bound, ruleFor, every time.Duration, o Options) (NearMiss, bool) {
	if b.Value == 0 || every <= 0 {
		return NearMiss{}, false // nothing is within a share of nothing
	}
	from, to := in.Start.Add(-o.Lead), in.EndOr(o.UnknownLen)
	n := int((ruleFor + every - 1) / every) // evaluations after the first, to fire
	span := time.Duration(n)*every + every/2
	var best NearMiss
	found := false
	for _, s := range series {
		l := maps.Clone(s.Labels)
		delete(l, "__name__")
		maps.Copy(l, ruleLabels)
		// A value that isn't a number doesn't hold: it breaks a stretch.
		smp := slices.DeleteFunc(slices.Clone(s.Samples), func(x prom.Sample) bool { return math.IsNaN(x.V) })
		if len(smp) == 0 || !in.Labels.Agree(l, o.Match) {
			continue
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
		nb := rules.Bound{Op: b.Op, Value: round(reached, b)}
		if math.Abs(nb.Value-b.Value) > NearBy*math.Abs(b.Value) {
			continue
		}
		// When it would have fired with that: the first evaluation n after
		// one that began a stretch of holding, between from and to.
		start := -1
		for i := range smp {
			if !nb.Holds(smp[i].V) {
				start = -1
				continue
			}
			if start < 0 || smp[i].T.Sub(smp[i-1].T) > every+every/2 {
				start = i
			}
			if i-start != n || smp[i].T.Before(from) {
				continue
			}
			if smp[i].T.After(to) {
				break
			}
			nm := NearMiss{Incident: in.ID, Labels: l, From: smp[start].T.Sub(in.Start), For: ruleFor, NewFor: ruleFor,
				Delay: smp[i].T.Sub(in.Start), Value: &ByValue{Op: b.Op, Bound: b.Value, Reached: reached, New: nb.Value}}
			if !found || nm.Kept() > best.Kept() || nm.Kept() == best.Kept() && nm.Delay < best.Delay {
				best, found = nm, true
			}
			break
		}
	}
	return best, found
}

// round is the round number nearest v that v meets, on the bound's side: in
// steps of a tenth of the bound's leading figure, 0.87 short of a bound of
// 0.9, 480 short of 500.
func round(v float64, b rules.Bound) float64 {
	exp := int(math.Floor(math.Log10(math.Abs(b.Value)))) - 1
	at := func(k float64) float64 {
		if exp < 0 {
			return k / math.Pow10(-exp)
		}
		return k * math.Pow10(exp)
	}
	x := v / at(1)
	k := math.Floor(x)
	if !b.Above() {
		k = math.Ceil(x)
	}
	step := 1.0
	if b.Above() {
		step = -1
	}
	for i := 0; i < 3 && !(rules.Bound{Op: b.Op, Value: at(k)}).Holds(v); i++ {
		k += step
	}
	return at(k)
}

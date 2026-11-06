// Package verdict judges alerts against what actually went wrong.
//
// Each firing gets a stone, as Roman juries voted: white when it caught an
// incident, black when nothing was wrong. A rule's stones, counted over a
// week, say whether it earns its place; an incident no rule caught is one the
// alerts missed.
package verdict

import (
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
)

// Options say how to judge.
type Options struct {
	From, To time.Time
	// Lead is how long before an incident starts a firing still counts as
	// catching it.
	Lead time.Duration
	// Match are labels a firing and an incident must agree on, when both
	// have them, for the firing to catch the incident: service, team. An
	// incident's label can have several values, any of which agrees.
	Match []string
	// RefireWithin is how soon after resolving a firing again counts as the
	// alert flapping.
	RefireWithin time.Duration
	// UnknownLen is how long an incident with no end is taken to last.
	UnknownLen time.Duration
	// A rule is noisy when it fired at least NoisyFirings times and under
	// NoisyPrecision of those firings caught anything: it fires whatever
	// happens. Zero means the defaults.
	NoisyFirings   int
	NoisyPrecision float64
	// SomeRules says only some of the rules are judged (--rule, or only those
	// changed), so what a rule adds to the others can't be said.
	SomeRules bool
	// Hours, when not nil, say when people are at work and asleep, to count
	// each rule's firings out of hours by.
	Hours *Hours
}

// The defaults for Options.NoisyFirings and Options.NoisyPrecision.
const (
	NoisyFirings   = 10
	NoisyPrecision = 0.2
)

func (o Options) noisyAt() NoisyAt {
	n := NoisyAt{Firings: o.NoisyFirings, Precision: o.NoisyPrecision}
	if n.Firings == 0 {
		n.Firings = NoisyFirings
	}
	if n.Precision == 0 {
		n.Precision = NoisyPrecision
	}
	return n
}

// NoisyAt is when a rule counts as noisy.
type NoisyAt struct {
	Firings   int     `json:"firings"`
	Precision float64 `json:"precision"`
}

// Judged is a firing and its stone.
type Judged struct {
	firing.Firing
	White     bool     `json:"white"`
	Incidents []string `json:"incidents,omitempty"`
}

// RuleResult is one rule's stones.
type RuleResult struct {
	Rule  string `json:"rule"`
	Group string `json:"group,omitempty"`
	// Severity is the rule's severity label; several, when versions or
	// rules of the same name differ, are joined by /.
	Severity string `json:"severity,omitempty"`
	Firings  int    `json:"firings"`
	White    int    `json:"white"`
	Black    int    `json:"black"`
	Refired  int    `json:"refired"` // firings that came within RefireWithin of the last one resolving
	Replayed bool   `json:"replayed"`
	// Noisy says the rule fires whatever happens: see Options.
	Noisy bool `json:"noisy"`
	// Unsupported is why the rule couldn't be replayed.
	Unsupported string `json:"unsupported,omitempty"`
	// Excused are firings a quiet window excused (lapilli serve): not
	// judged, and not in Firings.
	Excused int `json:"excused,omitempty"`
	// Incidents is how many incidents the rule caught, First those it caught
	// before any other rule, and Alone those no other rule caught. Spare says
	// other rules caught all of them as soon. See Credit.
	Incidents int  `json:"incidents"`
	First     int  `json:"first"`
	Alone     int  `json:"alone"`
	Spare     bool `json:"spare"`
	// OffHours and Asleep are the firings that came out of working hours, and
	// while people slept, by Options.Hours: a firing asleep isn't off hours
	// too. The Black ones caught nothing.
	OffHours      int `json:"off_hours"`
	OffHoursBlack int `json:"off_hours_black"`
	Asleep        int `json:"asleep"`
	AsleepBlack   int `json:"asleep_black"`
}

// Precision is the share of firings that caught something; ok is false
// when the rule never fired.
func (r RuleResult) Precision() (float64, bool) {
	if r.Firings == 0 {
		return 0, false
	}
	return float64(r.White) / float64(r.Firings), true
}

// Catch is one rule catching an incident.
type Catch struct {
	Rule string `json:"rule"`
	// Delay is from the incident's start to the rule's first firing;
	// negative when the alert came first.
	Delay time.Duration `json:"delay_ns"`
	// Noisy is a catch by a rule that fires whatever happens, which would
	// likely have fired at that time anyway.
	Noisy bool `json:"noisy"`
}

func (r RuleResult) noisy(at NoisyAt) bool {
	p, ok := r.Precision()
	return ok && r.Firings >= at.Firings && p < at.Precision
}

// IncidentResult is one incident and the rules that caught it.
type IncidentResult struct {
	incident.Incident
	CaughtBy []Catch `json:"caught_by"`
}

// Missed reports whether no rule caught the incident.
func (i IncidentResult) Missed() bool { return len(i.CaughtBy) == 0 }

// OnlyNoisy reports whether only noisy rules caught the incident, which is
// little better than missing it.
func (i IncidentResult) OnlyNoisy() bool {
	for _, c := range i.CaughtBy {
		if !c.Noisy {
			return false
		}
	}
	return len(i.CaughtBy) > 0
}

// First is the earliest catch by a rule that isn't noisy.
func (i IncidentResult) First() (Catch, bool) {
	for _, c := range i.CaughtBy {
		if !c.Noisy {
			return c, true
		}
	}
	return Catch{}, false
}

// Report is the verdict for a period.
type Report struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// RulesChanged is when, in the period, the rules replayed were
	// replaced by the next version of them.
	RulesChanged []time.Time      `json:"rules_changed,omitempty"`
	NoisyAt      NoisyAt          `json:"noisy_at"`
	Rules        []RuleResult     `json:"rules"`
	Incidents    []IncidentResult `json:"incidents"`
	Firings      []Judged         `json:"firings"`
	// Suggestions are noisy rules that would have kept their catches with
	// a longer "for".
	Suggestions []Suggestion `json:"suggestions,omitempty"`
	// SomeRules says the report is of some of the rules: see Options.
	SomeRules bool `json:"some_rules,omitempty"`
	// Hours are the hours firings were counted out of hours by.
	Hours *HoursUsed `json:"hours,omitempty"`
	// NearMisses are rules that nearly caught an incident nothing caught, or
	// only noisy rules did: their condition held, but not for their "for".
	NearMisses []NearMiss `json:"near_misses,omitempty"`
}

// Suggestion is a noisy rule replayed with a longer "for": it would have
// fired less and still caught every incident it caught, if later.
type Suggestion struct {
	Rule   string        `json:"rule"`
	For    time.Duration `json:"for_ns"`
	NewFor time.Duration `json:"new_for_ns"`
	Before RuleResult    `json:"before"`
	After  RuleResult    `json:"after"`
	// Incidents is how many incidents the rule caught, and still would.
	Incidents int `json:"incidents"`
	// Later is the most any of them would have been caught later.
	Later time.Duration `json:"later_ns"`
}

// Judge gives every firing in the period its stone. known lists the rules
// that were replayed, so rules that never fired show too; it may be nil.
func Judge(known []rules.Rule, fs []firing.Firing, incs []incident.Incident, o Options) *Report {
	at := o.noisyAt()
	rep := &Report{From: o.From, To: o.To, NoisyAt: at, Rules: []RuleResult{}, Incidents: []IncidentResult{}, Firings: []Judged{}, SomeRules: o.SomeRules,
		Hours: o.Hours.Used()}
	severities := map[string][]string{}
	addSeverity := func(rule, sev string) {
		if sev != "" && !slices.Contains(severities[rule], sev) {
			severities[rule] = append(severities[rule], sev)
		}
	}
	byRule := map[string]*RuleResult{}
	result := func(name, group string) *RuleResult {
		if r, ok := byRule[name]; ok {
			return r
		}
		r := &RuleResult{Rule: name, Group: group}
		byRule[name] = r
		return r
	}
	for _, k := range known {
		// A name can stand for several rules, or a rule as it was at
		// several times; if any of them couldn't be replayed, say so.
		r := result(k.Name, k.Group)
		if r.Unsupported == "" {
			r.Unsupported = k.Unsupported
		}
		r.Replayed = r.Unsupported == ""
		addSeverity(k.Name, k.Labels["severity"])
	}

	var inPeriod []incident.Incident
	for _, in := range incs {
		if !in.Start.Before(o.From) && in.Start.Before(o.To) {
			inPeriod = append(inPeriod, in)
		}
	}
	catches := make([]map[string]time.Duration, len(inPeriod))
	for i := range catches {
		catches[i] = map[string]time.Duration{}
	}

	last := map[string]time.Time{} // rule and labels to when it last resolved
	firing.Sort(fs)
	for _, f := range fs {
		if !f.Active(o.From, o.To) {
			continue
		}
		j := Judged{Firing: f}
		for i, in := range inPeriod {
			if Catches(f, in, o) {
				j.White = true
				j.Incidents = append(j.Incidents, in.ID)
				d := f.Start.Sub(in.Start)
				if have, ok := catches[i][f.Rule]; !ok || d < have {
					catches[i][f.Rule] = d
				}
			}
		}
		r := result(f.Rule, "")
		if len(known) == 0 {
			addSeverity(f.Rule, f.Labels["severity"])
		}
		r.Firings++
		if j.White {
			r.White++
		} else {
			r.Black++
		}
		o.Hours.Count(r, f.Start, j.White)
		k := f.Rule + "\x00" + labelKey(f.Labels)
		if Refired(last[k], f.Start, o) {
			r.Refired++
		}
		last[k] = f.End
		rep.Firings = append(rep.Firings, j)
	}

	for name, r := range byRule {
		r.Noisy = r.noisy(at)
		slices.Sort(severities[name])
		r.Severity = strings.Join(severities[name], "/")
	}
	for i, in := range inPeriod {
		ir := IncidentResult{Incident: in, CaughtBy: []Catch{}}
		for rule, d := range catches[i] {
			ir.CaughtBy = append(ir.CaughtBy, Catch{Rule: rule, Delay: d, Noisy: byRule[rule].Noisy})
		}
		sort.Slice(ir.CaughtBy, func(a, b int) bool {
			if ir.CaughtBy[a].Delay != ir.CaughtBy[b].Delay {
				return ir.CaughtBy[a].Delay < ir.CaughtBy[b].Delay
			}
			return ir.CaughtBy[a].Rule < ir.CaughtBy[b].Rule
		})
		rep.Incidents = append(rep.Incidents, ir)
	}

	for _, r := range byRule {
		rep.Rules = append(rep.Rules, *r)
	}
	// Noisiest first: the rules a reader should look at.
	sort.Slice(rep.Rules, func(a, b int) bool {
		x, y := rep.Rules[a], rep.Rules[b]
		if x.Black != y.Black {
			return x.Black > y.Black
		}
		if x.Firings != y.Firings {
			return x.Firings > y.Firings
		}
		return x.Rule < y.Rule
	})
	rep.Credit(nil)
	return rep
}

// Catches reports whether a firing catches an incident: it started between
// Lead before the incident and its end, and their labels agree on Match.
// One already firing before that caught nothing: it was on anyway.
func Catches(f firing.Firing, in incident.Incident, o Options) bool {
	from, to := in.Start.Add(-o.Lead), in.EndOr(o.UnknownLen)
	if f.Start.Before(from) || f.Start.After(to) {
		return false
	}
	return in.Labels.Agree(f.Labels, o.Match)
}

// Refired reports whether a firing that started at start came within
// RefireWithin of the same alert last resolving, at end: the alert
// flapping. A zero end is an alert that never resolved, or never fired.
func Refired(end, start time.Time, o Options) bool {
	return !end.IsZero() && start.Sub(end) <= o.RefireWithin
}

func labelKey(l map[string]string) string {
	var s string
	for _, k := range slices.Sorted(maps.Keys(l)) {
		s += k + "=" + l[k] + "\x00"
	}
	return s
}

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
	// DeployBefore and DeployAfter are how long before a deploy's start,
	// and after its end, a firing still counts as during it (lapilli
	// serve's --deploy-before and --deploy-after): marked, not judged
	// apart.
	DeployBefore, DeployAfter time.Duration
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
	// ImpactFrom, when not zero, is from when an impact signal (--impact)
	// was read, all of it: a measure of whether users were hurt, found or
	// not, that keeps the record of every firing from then (Record).
	ImpactFrom time.Time
	// RecordWeek, when not nil, is the week a time is in, [from, to), as
	// lapilli serve aligns its weeks: a firing is judged when the record of
	// its week is kept, as serve's stones are kept by week. Nil judges each
	// firing by the record at its own time, as lapilli verdict does.
	RecordWeek func(t time.Time) (from, to time.Time) `json:"-"`
	// Signals, when named, are the impact signals and the rule entries
	// firings are judged by (docs/design/signals.md): each window matched
	// on its signal's labels, each firing judged by the signals that could
	// have seen it, from each one's start. ImpactFrom is then zero: it is
	// the start of an impact signal with no name.
	Signals *Signals `json:"-"`
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

// Judged is a firing and its stone. NoRecord says the record wasn't kept
// at its time (Record.At): it caught nothing written down, and is counted,
// not judged.
type Judged struct {
	firing.Firing
	White     bool     `json:"white"`
	Incidents []string `json:"incidents,omitempty"`
	NoRecord  bool     `json:"no_record,omitempty"`
	// Refired says it came within Options.RefireWithin of the same alert
	// last resolving: one of its rule's Refired.
	Refired bool `json:"refired,omitempty"`
	// DuringDeploy says, in lapilli serve, it came during a deploy of what
	// it is about: one of its rule's DuringDeploys.
	DuringDeploy bool `json:"during_deploy,omitempty"`
	// Signals are, with named impact signals, those tied to it: those that
	// could have seen the incident it would catch (Signals.Ties). Their
	// windows keep its record, and so does reading those of them that keep
	// the record, from when all of them started.
	Signals []string `json:"signals,omitempty"`
	// Excused is, in lapilli serve's CSV, a firing a quiet window excused:
	// neither white nor black, and in none of the numbers. A report's
	// Firings leave such firings out.
	Excused bool `json:"-"`
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
	// Unjudged are its firings with no record at their time (Record): in
	// Firings and Black, as they caught nothing written down, and in no
	// precision, nor in whether it is noisy. NoRecord says none of its
	// firings was judged: it has no precision, and isn't noisy.
	Unjudged int  `json:"unjudged,omitempty"`
	NoRecord bool `json:"no_record,omitempty"`
	// Unsupported is why the rule couldn't be replayed.
	Unsupported string `json:"unsupported,omitempty"`
	// Unseen is what its replay can't see, as rules.Rule.Unseen says.
	Unseen []string `json:"unseen,omitempty"`
	// Outlasts, when not nil, says its "for" is as long as the range its
	// condition counts events over, or longer: one event alone can never
	// fire it (rules.Rule.ForOutlasts). Of a rule replayed.
	Outlasts *Outlast `json:"for_outlasts,omitempty"`
	// Source is, in lapilli serve, where a rule's firings were recorded
	// before they were copied in: a rule not replayed, whose condition
	// Lapilli doesn't have.
	Source string `json:"source,omitempty"`
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
	// Team is, in lapilli serve, the team that owns the rule.
	Team string `json:"team,omitempty"`
	// DuringDeploys are, in lapilli serve, the firings that came during a
	// deploy of what they are about, and DeploysCaught those of them that
	// caught something: in Firings and White, and shown apart.
	DuringDeploys int `json:"during_deploys,omitempty"`
	DeploysCaught int `json:"during_deploys_caught,omitempty"`
	// DeploysUnjudged are those of DuringDeploys among Unjudged.
	DeploysUnjudged int `json:"during_deploys_unjudged,omitempty"`
	// ImpactRule is, with named impact signals, the rule entry that names
	// it (Settings.ImpactRules), from 1; 0 for none.
	ImpactRule int `json:"impact_rule,omitempty"`
}

// Judged is how many of its firings were judged: those with a record at
// their time.
func (r RuleResult) Judged() int {
	if r.NoRecord {
		return 0
	}
	return r.Firings - r.Unjudged
}

// Precision is the share of the firings judged that caught something; ok
// is false when none was: it never fired, or had no record to judge by.
func (r RuleResult) Precision() (float64, bool) {
	n := r.Judged()
	if n <= 0 {
		return 0, false
	}
	return float64(r.White) / float64(n), true
}

// Unrecorded reports whether it fired as often as a noisy rule, with no
// record to judge those firings by, and isn't noisy by those it has.
func (r RuleResult) Unrecorded(at NoisyAt) bool {
	return !r.Noisy && r.Firings-r.Judged() >= at.Firings
}

// IsNoisy reports whether a rule fires whatever happens, by the firings
// judged alone: at least at.Firings of them, and under at.Precision of them
// catching anything.
func IsNoisy(judged, white int, at NoisyAt) bool {
	return judged > 0 && judged >= at.Firings && float64(white)/float64(judged) < at.Precision
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

func (r RuleResult) noisy(at NoisyAt) bool { return IsNoisy(r.Judged(), r.White, at) }

// IncidentResult is one incident and the rules that caught it.
type IncidentResult struct {
	incident.Incident
	CaughtBy []Catch `json:"caught_by"`
}

// ImpactShort is of the rules judged, those that fire only once their
// condition held longer than --impact-for (For): Rules of Judged, Need the
// most common of how long they need, their "for" and one evaluation (with
// no "for", an evaluation), and Most how many need that. An incident found by --impact as short as For
// can't be caught by them: shown, it is still judged as any other.
type ImpactShort struct {
	For    time.Duration `json:"impact_for"`
	Rules  int           `json:"rules"`
	Judged int           `json:"judged"`
	Need   time.Duration `json:"need"`
	Most   int           `json:"need_rules"`
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
	RulesChanged []time.Time `json:"rules_changed,omitempty"`
	NoisyAt      NoisyAt     `json:"noisy_at"`
	// NoRecord says the record of the period isn't kept for any time in it
	// (Record.Kept): none of its firings is judged, no rule is noisy, and
	// none is suggested a longer "for". Unjudged are its firings with no
	// record at their time, of it or of a period whose record is kept only
	// at times.
	NoRecord  bool             `json:"no_record,omitempty"`
	Unjudged  int              `json:"unjudged,omitempty"`
	Rules     []RuleResult     `json:"rules"`
	Incidents []IncidentResult `json:"incidents"`
	Firings   []Judged         `json:"firings"`
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
	// NearMissesCut says the time ran out before every incident was looked
	// at for near misses.
	NearMissesCut bool `json:"near_misses_cut,omitempty"`
	// ImpactShort, with --impact, says how many of the rules judged can't
	// fire on impact as short as --impact-for counts as an incident: their
	// condition must hold for their "for" and one evaluation more.
	ImpactShort *ImpactShort `json:"impact_short,omitempty"`
	// Extras says the steps after the replays (a longer "for" for the noisy
	// rules, near misses, what fired with no record) were "skipped", by
	// --extras 0, or "cut" short, by --extras or --timeout: what they find
	// is then none, or in part. Empty, they ran to their end.
	Extras string `json:"extras,omitempty"`
	// Unrecorded are, of a period with no record (NoRecord), the rules
	// replayed that fired as often as a noisy rule, and what their firings
	// were: the most firings first.
	Unrecorded []Unrecorded `json:"unrecorded,omitempty"`
	// Settings are what judged it, for anyone counting its numbers again
	// (docs/numbers.md).
	Settings *Settings `json:"settings,omitempty"`
	// Unreplayed are the rules that couldn't be replayed, as
	// UnreplayedRules gives them, set by the run that replayed them: in
	// lapilli verdict's --json, and named at the top of its text. A report
	// read back from lapilli serve's ledger has none: its numbers keep
	// what a rule fired before its last replay failed.
	Unreplayed []Unreplayed `json:"unreplayed,omitempty"`
}

// Settings are what a report's firings were judged by, as Options has
// them: enough to judge them again from the report alone
// (docs/numbers.md).
type Settings struct {
	// Lead is how long before an incident a firing still catches it.
	Lead time.Duration `json:"lead_ns"`
	// Match are the labels a firing and an incident must agree on.
	Match []string `json:"match"`
	// Refire is how soon after the same alert resolved a firing re-fired.
	Refire time.Duration `json:"refire_ns"`
	// UnknownLength is how long an incident with no end is taken to last.
	UnknownLength time.Duration `json:"unknown_length_ns"`
	// RecordLookback is how long before a firing an incident keeps its
	// record (RecordLookback).
	RecordLookback time.Duration `json:"record_lookback_ns"`
	// ImpactFrom is from when an impact signal was read, all of it: a
	// firing from then has a record. Zero for none.
	ImpactFrom time.Time `json:"impact_from,omitzero"`
	// ByWeek says a firing's record is its week's (lapilli serve), not its
	// own time's.
	ByWeek bool `json:"record_by_week,omitempty"`
	// ImpactSignals are the named impact signals, each with its "for", its
	// match (null for the match in effect), whether it keeps the record,
	// and from when it does; ImpactRules the rule entries, in order. None
	// with an impact signal that has no name.
	ImpactSignals []Signal    `json:"impact_signals,omitempty"`
	ImpactRules   []RuleEntry `json:"impact_rules,omitempty"`
}

// Settings are what o judges by, as a report says them.
func (o Options) Settings() *Settings {
	match := o.Match
	if match == nil {
		match = []string{}
	}
	s := &Settings{Lead: o.Lead, Match: match, Refire: o.RefireWithin, UnknownLength: o.UnknownLen,
		RecordLookback: RecordLookback, ImpactFrom: o.ImpactFrom, ByWeek: o.RecordWeek != nil}
	if o.Signals.Named() {
		s.ImpactSignals, s.ImpactRules = slices.Clone(o.Signals.List), slices.Clone(o.Signals.Entries)
	}
	return s
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
		Hours: o.Hours.Used(), Settings: o.Settings()}
	rec := RecordOf(incs, o)
	rep.NoRecord = !rec.JudgesAny(o.From, o.To)
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
		r := &RuleResult{Rule: name, Group: group, ImpactRule: o.Signals.EntryNumber(name)}
		byRule[name] = r
		return r
	}
	named := o.Signals.Named()
	for _, k := range known {
		// A name can stand for several rules, or a rule as it was at
		// several times; if any of them couldn't be replayed, say so.
		r := result(k.Name, k.Group)
		if r.Unsupported == "" {
			r.Unsupported = k.Unsupported
		}
		r.Replayed = r.Unsupported == ""
		for _, u := range k.Unseen() {
			if !slices.Contains(r.Unseen, u) {
				r.Unseen = append(r.Unseen, u)
			}
		}
		// Of a rule as it was at several times, as the last given is.
		r.Outlasts = nil
		if w, ok := k.ForOutlasts(); ok {
			r.Outlasts = &Outlast{For: k.For, Window: w}
		}
		addSeverity(k.Name, k.AlertLabels()["severity"])
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
		var ties []int
		if named {
			ties = o.Signals.Ties(f.Rule, f.Labels, o.Match)
			j.Signals = o.Signals.Names(ties)
		}
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
			if named && !rec.JudgesTied(f.Start, ties) || !named && !rec.Judges(f.Start) {
				j.NoRecord = true
				r.Unjudged++
				rep.Unjudged++
			}
		}
		o.Hours.Count(r, f.Start, j.White)
		k := f.Rule + "\x00" + labelKey(f.Labels)
		if Refired(last[k], f.Start, o) {
			r.Refired++
			j.Refired = true
		}
		last[k] = f.End
		rep.Firings = append(rep.Firings, j)
	}

	for name, r := range byRule {
		r.NoRecord = r.Firings > 0 && r.Unjudged == r.Firings
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
// Lead before the incident and its end, and their labels agree on the
// labels it is matched on (Options.Agrees: Match, or a signal's or a rule
// entry's). One already firing before that caught nothing: it was on
// anyway.
func Catches(f firing.Firing, in incident.Incident, o Options) bool {
	from, to := in.Start.Add(-o.Lead), in.EndOr(o.UnknownLen)
	if f.Start.Before(from) || f.Start.After(to) {
		return false
	}
	return o.Agrees(f.Rule, f.Labels, in)
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

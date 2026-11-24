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
	// catching it: of every incident but a window of a named signal whose
	// signal, or the rule's entry, gives a lead of its own (LeadFor).
	Lead time.Duration
	// Match are labels a firing and an incident must agree on, when both
	// have them, for the firing to catch the incident: service, team. An
	// incident's label can have several values, any of which agrees.
	Match []string
	// MatchStrict says a firing that lacks a label of Match that the
	// incident has doesn't catch it (--match-strict), where it would by
	// default: the label isn't there to disagree.
	MatchStrict bool
	// LinkedOnly says an incident that lists the alerts its tool linked to
	// it (incident.Incident.Alerts) is caught by those alone
	// (--linked-only, docs/design/links.md); one that lists none is
	// caught as any other.
	LinkedOnly bool
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
	// not, that keeps the record of every firing from then (Record). In
	// lapilli verdict, where the data of every selector it reads begins,
	// when that is after it was first read.
	ImpactFrom time.Time
	// ImpactNoData says an impact signal with no name was read, and a
	// selector it reads had no data: it keeps no record (ImpactFrom zero).
	ImpactNoData bool
	// RecordWeek, when not nil, is the week a time is in, [from, to), as
	// lapilli serve aligns its weeks: a firing is judged when the record of
	// its week is kept, as serve's stones are kept by week. Nil judges each
	// firing by the record at its own time, as lapilli verdict does.
	RecordWeek func(t time.Time) (from, to time.Time) `json:"-"`
	// Versions are, when the rules replayed stood as several versions over
	// the period (a rules history), every one of them: from when a
	// firing's condition held (Judged.After) is its start less the least
	// "for" and lag of its rule among known and these, so it is said to
	// have held in an incident only when every version would have.
	Versions []rules.Rule `json:"-"`
	// Held is, in lapilli serve, by rule, how long before a firing its
	// condition held, of the versions in effect over the period (HeldFor):
	// what the ledger says a firing came after an incident by, as Judge
	// says it from known and Versions. Nil says nothing came after any.
	Held map[string]time.Duration `json:"-"`
	// Signals, when named, are the impact signals and the rule entries
	// firings are judged by (docs/design/signals.md): each window matched
	// on its signal's labels, each firing judged by the signals that could
	// have seen it, from each one's start. ImpactFrom is then zero: it is
	// the start of an impact signal with no name.
	Signals *Signals `json:"-"`
	// SignalsGen is, in lapilli serve, the ledger's generation of the named
	// signals these options were read at (ledger.SignalsGen): the ledger
	// judges what it pended with options read before the signals last
	// changed no more (docs/design/signals.md, "Step 3, as built"). Zero,
	// not said: then Signals are held to what the ledger keeps.
	SignalsGen int64 `json:"-"`
	// Zone is the zone days are counted in, which parts episodes at
	// midnight (countEpisodes): --tz, or lapilli serve's ledger's; the
	// machine's when nil.
	Zone *time.Location `json:"-"`
	// Verdicts are people's verdicts on firings of the period
	// (docs/design/judging.md, item 3), each given to the firing it finds
	// again by the rule of a move (Place): a firing said to be unrelated
	// to what it caught doesn't catch it. A replay of a change judges both
	// of its sides with them, so that neither looks better on the firings
	// people called coincidences.
	Verdicts []Verdict `json:"-"`
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
	// After are, of a black firing, the incidents it came after the end
	// of, its condition having held in them (Firing.Held) and its labels
	// agreeing: their alert, too late to catch them. It stays black. Its
	// rule's Late counts it.
	After []string `json:"after,omitempty"`
	// DuringDeploy says, in lapilli serve, it came during a deploy of what
	// it is about: one of its rule's DuringDeploys.
	DuringDeploy bool `json:"during_deploy,omitempty"`
	// Signals are, with named impact signals, those tied to it: those that
	// could have seen the incident it would catch (Signals.Ties). Their
	// windows keep its record, and so does reading those of them that keep
	// the record, from when all of them started.
	Signals []string `json:"signals,omitempty"`
	// Marked are, in lapilli serve, the kinds of marked window it came
	// during (docs/design/marked.md), in order: of its rule's Marked.
	Marked []string `json:"marked,omitempty"`
	// Excused is, in lapilli serve's CSV, a firing a quiet window excused:
	// neither white nor black, and in none of the numbers. A report's
	// Firings leave such firings out.
	Excused bool `json:"-"`
	// Episode is the episode it is of (docs/design/judging.md, item 2),
	// numbered from 1 in the report in order of their first firing: 0
	// when the report's episodes aren't counted (lapilli serve, before
	// its ledger has counted the weeks). Day is the day its start falls
	// on in the zone days are counted in (Settings.Zone), 2026-11-19:
	// what parts an episode at midnight.
	Episode int    `json:"episode,omitempty"`
	Day     string `json:"day,omitempty"`
	// Unrelated are, of the incidents it catches as Lapilli judges it,
	// those a person said it is unrelated to (docs/design/judging.md,
	// item 3), which it doesn't catch: white as Lapilli judged it is White,
	// or Unrelated not empty. Action is a person's answer to whether it
	// needed someone to act: ActionYes, ActionNo or none.
	Unrelated []string `json:"unrelated,omitempty"`
	Action    string   `json:"action,omitempty"`
	// NotJudged says its rule's entry says it is judged by nothing
	// (judged: false): it catches nothing, and is counted, not judged, as
	// NoRecord says too.
	NotJudged bool `json:"not_judged,omitempty"`
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
	// Late are of its black firings (its unjudged among them, as Black
	// counts them) those that came after an incident was over, its
	// condition having held in it (Judged.After): black all the same, as
	// nothing came in time.
	Late     int  `json:"late,omitempty"`
	Replayed bool `json:"replayed"`
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
	// NoData is, of a rule that never fired, the values its condition
	// compares that had no series at all in the period, so it couldn't
	// have fired. It is set only when looked for, a step after the replays
	// (--extras).
	NoData []string `json:"no_data,omitempty"`
	// Outlasts, when not nil, says its "for" is as long as the range its
	// condition counts events over, or longer: one event alone can never
	// fire it (rules.Rule.ForOutlasts). Of a rule replayed.
	Outlasts *Outlast `json:"for_outlasts,omitempty"`
	// Source is, in lapilli serve, where a rule's firings were recorded
	// before they were copied in: a rule not replayed, whose condition
	// Lapilli doesn't have. In lapilli verdict, GrafanaSource for a
	// Grafana rule Lapilli can't replay, judged from Grafana's own alert
	// state history (docs/design/grafanajudged.md).
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
	// Marked are, in lapilli serve, by kind of marked window, the firings
	// that came during one of the kind (docs/design/marked.md): in
	// Firings and White as any other, and counted apart.
	Marked map[string]MarkedCount `json:"marked,omitempty"`
	// Episodes are its firings counted as episodes (EpisodeCount), beside
	// them: left out with none, or not counted.
	Episodes EpisodeCount `json:"episodes,omitzero"`
	// Unrelated are its firings white as Lapilli judged them that a person
	// said were unrelated to all they caught, black by a person's word;
	// ActionYes and ActionNo its firings people said needed someone to
	// act, and didn't (docs/design/judging.md, item 3).
	Unrelated int `json:"unrelated,omitempty"`
	ActionYes int `json:"action_yes,omitempty"`
	ActionNo  int `json:"action_no,omitempty"`
	// NotJudged are its firings its rule entry says are judged by nothing
	// (judged: false): of Unjudged, which counts them too.
	NotJudged int `json:"not_judged,omitempty"`
}

// MarkedCount is a rule's firings during the windows of one kind: those
// that caught something, re-fired, and weren't judged (of Unjudged).
type MarkedCount struct {
	Firings  int `json:"firings"`
	Caught   int `json:"caught"`
	Refired  int `json:"refired"`
	Unjudged int `json:"unjudged,omitempty"`
}

// GrafanaSource is the Source of a Grafana rule Lapilli can't replay,
// judged from what Grafana's own alert state history says it fired: in
// lapilli serve, the source its history is copied in under, which no job
// may write to (docs/design/grafanajudged.md).
const GrafanaSource = "grafana"

// FiringsSource is the Source of a rule whose firings were recorded
// elsewhere and read with --firings beside the rules replayed: a cloud's own
// alarm, which Lapilli doesn't replay.
const FiringsSource = "firings"

// RecordedReason is the Unsupported of such a rule: why it wasn't replayed.
const RecordedReason = "recorded elsewhere, not replayed"

// ByHistory reports whether the rule isn't replayed and its firings come
// from its history (Source): a Grafana rule judged from Grafana's own, a
// cloud's alarm tied to the history copied in for it. It is judged as a
// replayed rule is, but for what needs its condition: a longer "for", near
// misses, a backtest.
func (r RuleResult) ByHistory() bool { return r.Unsupported != "" && r.Source != "" }

// Recorded reports whether the rule is one of --firings beside the rules
// replayed: ByHistory, its firings read from a file, not asked of a history.
// It is no rule that couldn't be replayed, as it was never to be.
func (r RuleResult) Recorded() bool { return r.ByHistory() && r.Unsupported == RecordedReason }

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
	// Linked says a firing of the rule that caught it is one of the alerts
	// the incident tool linked to it (incident.Incident.Alerts).
	Linked bool `json:"linked,omitempty"`
	// Loose says the rule caught it while its firings lacked a label of
	// the match in effect that the incident has (Options.Match): with no
	// service, it agrees with every service's incident. A rule that also
	// fired with the label caught it as any other.
	Loose bool `json:"loose,omitempty"`
}

func (r RuleResult) noisy(at NoisyAt) bool { return IsNoisy(r.Judged(), r.White, at) }

// IncidentResult is one incident and the rules that caught it.
type IncidentResult struct {
	incident.Incident
	// Real says it was written from a firing, by a person's "This was
	// real" (docs/design/judging.md, item 3).
	Real     bool    `json:"real,omitempty"`
	CaughtBy []Catch `json:"caught_by"`
	// FiredAfter are the rules that didn't catch it but fired once it was
	// over, their condition having held in it (Judged.After), each by its
	// first such firing: Delay from its start, as CaughtBy's.
	FiredAfter []Catch `json:"fired_after,omitempty"`
	// NotKnown says no rule replayed caught it, while a rule judged
	// couldn't be replayed (its Unsupported set, and not ByHistory), which
	// may have caught it. It is still counted by Missed.
	NotKnown bool `json:"not_known,omitempty"`
}

// ImpactShort is of the rules judged, those that fire only once their
// condition held longer than --impact-for (For): Rules of Judged, Need the
// most common of how long they need, their "for" and one evaluation (with
// no "for", an evaluation), and how late their metrics arrive, and Most
// how many need that. Lagged are of Rules those whose metrics arrive late
// (--late, query_offset, a Grafana query's time range). An incident found by --impact as short as For
// can't be caught by them: shown, it is still judged as any other. With
// named signals, Signal is the one whose "for" is shortest, and the rules
// judged are those judged against its windows.
type ImpactShort struct {
	For    time.Duration `json:"impact_for"`
	Signal string        `json:"signal,omitempty"`
	Rules  int           `json:"rules"`
	Judged int           `json:"judged"`
	Need   time.Duration `json:"need"`
	Most   int           `json:"need_rules"`
	Lagged int           `json:"late_rules,omitempty"`
}

// Missed reports whether no rule caught the incident.
func (i IncidentResult) Missed() bool { return len(i.CaughtBy) == 0 }

// knownMissed reports whether nothing caught the incident and that is
// known: it isn't NotKnown.
func (i IncidentResult) knownMissed() bool { return i.Missed() && !i.NotKnown }

// unknown reports whether nothing caught the incident and whether one did
// isn't known (NotKnown).
func (i IncidentResult) unknown() bool { return i.Missed() && i.NotKnown }

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
	// NotKnownMetric makes Prometheus write lapilli_incidents_not_known:
	// lapilli verdict --metrics sets it, as only a run that replays rules
	// can fail to; serve's /metrics never does.
	NotKnownMetric bool `json:"-"`
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
	// condition must hold for their "for" and one evaluation more, and
	// their metrics arrive late.
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
	// EpisodesNotCounted says, in lapilli serve, that its ledger hasn't
	// counted the episodes of all of the period yet (after an upgrade, or
	// a change of zone): its rules' and firings' are left out.
	EpisodesNotCounted bool `json:"-"`
	// People says, in lapilli serve, that people's verdicts on its firings
	// are kept (docs/design/judging.md, item 3): its metrics give each
	// rule's answers to "needed action".
	People bool `json:"-"`
	// LateCut says, in lapilli serve, that more firings came near the end
	// of incidents than its ledger looks at in one read for those that came
	// after one (ledger.LateOf): the rest aren't said to have, so a rule's
	// Late may be fewer, and an incident's FiredAfter none, than they are.
	LateCut bool `json:"late_cut,omitempty"`
}

// Settings are what a report's firings were judged by, as Options has
// them: enough to judge them again from the report alone
// (docs/numbers.md).
type Settings struct {
	// Lead is how long before an incident a firing still catches it.
	Lead time.Duration `json:"lead_ns"`
	// Match are the labels a firing and an incident must agree on.
	Match []string `json:"match"`
	// MatchStrict says a firing without a label of Match that the incident
	// has doesn't catch it (--match-strict).
	MatchStrict bool `json:"match_strict,omitempty"`
	// LinkedOnly says an incident that lists its linked alerts is caught
	// by them alone (--linked-only).
	LinkedOnly bool `json:"linked_only,omitempty"`
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
	// ImpactNoData says an impact signal with no name was read, and a
	// selector it reads had no data: it keeps no record, and ImpactFrom
	// is missing.
	ImpactNoData bool `json:"impact_no_data,omitempty"`
	// ByWeek says a firing's record is its week's (lapilli serve), not its
	// own time's.
	ByWeek bool `json:"record_by_week,omitempty"`
	// ImpactSignals are the named impact signals, each with its "for", its
	// match (null for the match in effect), whether it keeps the record,
	// and from when it does; ImpactRules the rule entries, in order. None
	// with an impact signal that has no name.
	ImpactSignals []Signal    `json:"impact_signals,omitempty"`
	ImpactRules   []RuleEntry `json:"impact_rules,omitempty"`
	// Zone is the zone days are counted in, an IANA name: what each
	// firing's day (Judged.Day) is of, and what parts episodes at
	// midnight.
	Zone string `json:"zone,omitempty"`
}

// NoRecordOf is, with named signals, whether a period, or a row, has no
// record: none of its firings was judged, of firings with unjudged of them
// counted and not judged; with none, by none, whether nothing in it could
// be judged at all.
func NoRecordOf(firings, unjudged int, none func() bool) bool {
	if firings > 0 {
		return unjudged == firings
	}
	return none()
}

// Settings are what o judges by, as a report says them.
func (o Options) Settings() *Settings {
	match := o.Match
	if match == nil {
		match = []string{}
	}
	s := &Settings{Lead: o.Lead, Match: match, MatchStrict: o.MatchStrict, LinkedOnly: o.LinkedOnly, Refire: o.RefireWithin, UnknownLength: o.UnknownLen,
		RecordLookback: RecordLookback, ImpactFrom: o.ImpactFrom, ImpactNoData: o.ImpactNoData, ByWeek: o.RecordWeek != nil,
		Zone: o.zone().String()}
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
	// How long before a firing its rule's condition held, by the time of
	// the history it read: its "for" and how late its metrics arrive, of
	// a rule as it was at several times the least (Options.Versions).
	held := HeldFor(slices.Concat(known, o.Versions))
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
	after := make([]map[string]time.Duration, len(inPeriod))
	// The rules a firing the incident lists caught it with.
	linked := make([]map[string]bool, len(inPeriod))
	// The rules a firing that isn't loose caught it with (Catch.Loose).
	proper := make([]map[string]bool, len(inPeriod))
	for i := range catches {
		catches[i], after[i], linked[i], proper[i] = map[string]time.Duration{}, map[string]time.Duration{}, map[string]bool{}, map[string]bool{}
	}

	last := map[string]time.Time{} // rule and labels to when it last resolved
	firing.Sort(fs)
	// People's verdicts, each on the firing it finds again (Place): a
	// replay judged in memory writes nothing, so a verdict that finds no
	// firing, or that another holds, is dropped here, not marked.
	forOf := map[string]time.Duration{}
	for _, k := range known {
		forOf[k.Name] = max(forOf[k.Name], k.For)
	}
	_, people := Place(o.Verdicts, fs, o.Match, o.VerdictReach(func(rule string) time.Duration { return forOf[rule] }))
	notJudged := map[string]bool{}
	for fi, f := range fs {
		if !f.Active(o.From, o.To) {
			continue
		}
		j := Judged{Firing: f}
		var ties []int
		if named {
			ties = o.Signals.Ties(f.Rule, f.Labels, o.Match)
			j.Signals = o.Signals.Names(ties)
		}
		said := people[fi]
		if said != nil {
			j.Action = said.Action
		}
		nj, ok := notJudged[f.Rule]
		if !ok {
			nj = o.NotJudged(f.Rule)
			notJudged[f.Rule] = nj
		}
		j.NotJudged = nj
		lapilli := false // white as Lapilli judged it: caught an incident "This was real" didn't write
		for i, in := range inPeriod {
			if nj {
				continue
			}
			caught, link, loose := CatchesLinked(f, in, o)
			if !caught {
				continue
			}
			lapilli = lapilli || !in.Real
			// Said by a person to be unrelated to it: it doesn't catch it.
			if said != nil && slices.Contains(said.Unrelated, in.ID) {
				j.Unrelated = append(j.Unrelated, in.ID)
				continue
			}
			j.White = true
			j.Incidents = append(j.Incidents, in.ID)
			if link {
				linked[i][f.Rule] = true
			}
			if !loose {
				proper[i][f.Rule] = true
			}
			d := f.Start.Sub(in.Start)
			if have, ok := catches[i][f.Rule]; !ok || d < have {
				catches[i][f.Rule] = d
			}
		}
		j.Held = HeldFrom(f, held)
		if !j.White && !nj {
			j.After = cameAfter(f, j.Held, inPeriod, catches, after, o)
		}
		r := result(f.Rule, "")
		if len(known) == 0 {
			addSeverity(f.Rule, f.Labels["severity"])
		}
		r.Firings++
		switch j.Action {
		case ActionYes:
			r.ActionYes++
		case ActionNo:
			r.ActionNo++
		}
		if j.White {
			r.White++
		} else {
			r.Black++
			if len(j.After) > 0 {
				r.Late++
			}
			// White as Lapilli judged it, black by people's word.
			if len(j.Unrelated) > 0 && lapilli {
				r.Unrelated++
			}
			if nj || named && !rec.JudgesTied(f.Start, ties) || !named && !rec.Judges(f.Start) {
				j.NoRecord = true
				r.Unjudged++
				rep.Unjudged++
			}
			if nj {
				r.NotJudged++
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

	// With named signals, a period has no record when none of its firings
	// was judged, as each rule's and each row of lapilli serve's summary:
	// part of it may be, and part not (NoRecordOf).
	if named {
		rep.NoRecord = NoRecordOf(len(rep.Firings), rep.Unjudged, func() bool { return !rec.JudgesAny(o.From, o.To) })
	}
	for name, r := range byRule {
		r.NoRecord = r.Firings > 0 && r.Unjudged == r.Firings
		r.Noisy = r.noisy(at)
		slices.Sort(severities[name])
		r.Severity = strings.Join(severities[name], "/")
	}
	for i, in := range inPeriod {
		ir := IncidentResult{Incident: in, Real: in.Real, CaughtBy: []Catch{}}
		for rule, d := range catches[i] {
			ir.CaughtBy = append(ir.CaughtBy, Catch{Rule: rule, Delay: d, Noisy: byRule[rule].Noisy, Linked: linked[i][rule], Loose: !proper[i][rule]})
		}
		sortCatches(ir.CaughtBy)
		for rule, d := range after[i] {
			ir.FiredAfter = append(ir.FiredAfter, Catch{Rule: rule, Delay: d, Noisy: byRule[rule].Noisy})
		}
		sortCatches(ir.FiredAfter)
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
	rep.MarkNotKnown()
	rep.countEpisodes(o, at)
	rep.Credit(nil)
	return rep
}

// MarkNotKnown sets NotKnown on each incident nothing caught, while a rule
// couldn't be replayed: that rule may have caught it. A rule judged from
// its history (ByHistory) was judged, and a rule that can't fire
// (rules.CantFire) caught nothing: neither counts. Judge does it; a
// caller that sets a rule's Source after Judge does it again.
func (r *Report) MarkNotKnown() {
	unsure := slices.ContainsFunc(r.Rules, func(rr RuleResult) bool {
		return rr.Unsupported != "" && !rr.ByHistory() && !rules.CantFire(rr.Unsupported)
	})
	for i := range r.Incidents {
		r.Incidents[i].NotKnown = len(r.Incidents[i].CaughtBy) == 0 && unsure
	}
}

// sortCatches puts catches in order of their delay, then of their rule.
func sortCatches(cs []Catch) {
	sort.Slice(cs, func(a, b int) bool {
		if cs[a].Delay != cs[b].Delay {
			return cs[a].Delay < cs[b].Delay
		}
		return cs[a].Rule < cs[b].Rule
	})
}

// HeldFor is, by rule, how long before a firing its condition held, by
// the time of the history it read: its "for" and how late its metrics
// arrive, the least of the rules of its name (a rule as it stood at
// several times). A rule with neither held only once it fired.
func HeldFor(rs []rules.Rule) map[string]time.Duration {
	held := map[string]time.Duration{}
	for _, k := range rs {
		if d, ok := held[k.Name]; !ok || max(k.For, 0)+k.Lag() < d {
			held[k.Name] = max(k.For, 0) + k.Lag()
		}
	}
	return held
}

// HeldFrom is from when a firing's condition held: its own Held, or else
// its start less how long its rule's held (HeldFor); zero for its start.
func HeldFrom(f firing.Firing, held map[string]time.Duration) time.Time {
	if f.Held.IsZero() && held[f.Rule] > 0 {
		return f.Start.Add(-held[f.Rule])
	}
	return f.Held
}

// AfterEnd reports whether a firing whose condition held from held (zero
// for its start) came after an incident was over while its condition had
// held in it, its labels agreeing (About): its alert, too late. Whether
// its rule caught the incident otherwise isn't asked. A firing from
// elsewhere, with no rule or Held to say when its condition held, held
// from its start, and so came after none.
func AfterEnd(f firing.Firing, held time.Time, in incident.Incident, o Options) bool {
	if held.IsZero() {
		held = f.Start
	}
	end := in.EndOr(o.UnknownLen)
	return f.Start.After(end) && !held.After(end) && o.About(f.Rule, f.Labels, in)
}

// cameAfter is, of a black firing whose condition held from held (zero
// for its start), the incidents it came after the end of while its
// condition had held in them (AfterEnd), of those its rule hasn't caught
// (catches): their alert, too late. Each is noted in after, by the delay
// from its start of the rule's first such firing.
func cameAfter(f firing.Firing, held time.Time, ins []incident.Incident, catches, after []map[string]time.Duration, o Options) []string {
	var ids []string
	for i, in := range ins {
		if _, caught := catches[i][f.Rule]; caught || !AfterEnd(f, held, in, o) {
			continue
		}
		ids = append(ids, in.ID)
		d := f.Start.Sub(in.Start)
		if have, ok := after[i][f.Rule]; !ok || d < have {
			after[i][f.Rule] = d
		}
	}
	return ids
}

// Catches reports whether a firing catches an incident: it started between
// the lead it is judged by before the incident (Options.LeadFor: Lead, or
// a signal's or a rule entry's) and its end, and it is one of the alerts
// the incident lists (incident.Incident.Links), or, of one that lists none
// or without --linked-only, their labels agree on the labels it is matched
// on (Options.Agrees: Match, or a signal's or a rule entry's). One already
// firing before that caught nothing: it was on anyway.
func Catches(f firing.Firing, in incident.Incident, o Options) bool {
	caught, _, _ := CatchesLinked(f, in, o)
	return caught
}

// CatchesLinked is Catches, and whether it caught it as one of the alerts
// the incident lists: such a firing catches it whatever their labels say
// on the labels matched on (docs/design/links.md). And whether it caught it
// loose: it lacks a label of the match in effect that the incident has,
// where --match-strict would have it not catch it (Catch.Loose).
func CatchesLinked(f firing.Firing, in incident.Incident, o Options) (caught, linked, loose bool) {
	to := in.EndOr(o.UnknownLen)
	// A rule judged by nothing catches nothing, written incidents
	// included (docs/design/judging.md, "Rules judged by nothing").
	if o.Signals.NotJudgedRules() && o.NotJudged(f.Rule) {
		return false, false, false
	}
	if f.Start.After(to) || f.Start.Before(in.Start.Add(-o.MaxLead())) {
		return false, false, false
	}
	j := o.judgingOf(f.Rule, in)
	if f.Start.Before(in.Start.Add(-o.leadOf(j, in))) {
		return false, false, false
	}
	if in.Links(f.Rule, f.Labels) {
		return true, true, false
	}
	if o.LinkedOnly && len(in.Alerts) > 0 {
		return false, false, false
	}
	caught, loose = o.agreesLoose(j, f.Labels, in)
	return caught, false, loose
}

// About reports whether a firing of rule with labels is about an incident,
// whenever it came, as Catches has it: one of the alerts it lists, or, of
// one that lists none or without --linked-only, one whose labels agree
// with it; never of a rule judged by nothing. Near misses are looked for
// by it, so a rule the incident lists is looked at whatever --match says.
func (o Options) About(rule string, labels map[string]string, in incident.Incident) bool {
	if o.Signals.NotJudgedRules() && o.NotJudged(rule) {
		return false
	}
	if in.Links(rule, labels) {
		return true
	}
	if o.LinkedOnly && len(in.Alerts) > 0 {
		return false
	}
	return o.Agrees(rule, labels, in)
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

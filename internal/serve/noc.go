package serve

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// What an operations team asks of its alerts, week by week and team by
// team (docs/design/noc.md): how many critical firings needed someone, how
// many re-fired, came off hours or during a deploy, and which incidents and
// impact windows the alerts caught, and how soon. /api/v1/summary gives it
// for dashboards and other tools, in a shape that keeps its schema.

// SummarySchema is the version of /api/v1/summary's shape. A field is
// added without changing it; one changed or taken away changes it.
const SummarySchema = 1

// summaryRowSeen, when set, is told of each row as it is worked out: for
// tests of a request given up halfway.
var summaryRowSeen func()

// summaryMaxWeeks is how many weeks /api/v1/summary gives at most.
const summaryMaxWeeks = 53

type summary struct {
	Schema int `json:"schema"`
	// From and To are the span of the weeks given; Zone the time zone weeks
	// are counted in, from Monday.
	From     time.Time       `json:"from"`
	To       time.Time       `json:"to"`
	Zone     string          `json:"zone"`
	Settings summarySettings `json:"settings"`
	// LedgerTo is how far the ledger has firings: a week after it is
	// partial.
	LedgerTo time.Time    `json:"ledger_to"`
	Rows     []summaryRow `json:"rows"`
}

type summarySettings struct {
	Lead         string          `json:"lead"`
	Refire       string          `json:"refire"`
	DeployBefore string          `json:"deploy_before"`
	DeployAfter  string          `json:"deploy_after"`
	Match        []string        `json:"match"`
	NoisyAt      verdict.NoisyAt `json:"noisy_at"`
	// Critical are the severities counted as critical.
	Critical []string `json:"critical"`
	// Hours says off hours and asleep are counted; Deploys, that the
	// ledger keeps deploys.
	Hours   bool `json:"hours"`
	Deploys bool `json:"deploys"`
	// ImpactSignals are the named impact signals, each with its "for",
	// its match (null for the match in effect), whether it keeps the
	// record and from when it does, and its lead when it gives one;
	// ImpactRules the rule entries, in order (docs/design/signals.md).
	// None with no named signal.
	ImpactSignals []summarySignal `json:"impact_signals,omitempty"`
	ImpactRules   []summaryEntry  `json:"impact_rules,omitempty"`
}

// summarySignal is a named impact signal, as the summary says it: not its
// expression, which the Settings page and /settings.yaml give. Lead is
// left out when it follows settings.lead (docs/design/judging.md, "In the
// numbers").
type summarySignal struct {
	Name   string     `json:"name"`
	For    string     `json:"for"`
	Match  []string   `json:"match"`
	Record bool       `json:"record"`
	Start  *time.Time `json:"start"`
	Lead   string     `json:"lead,omitempty"`
}

// summaryEntry is a rule entry, as the summary says it: as the report has
// it (verdict.RuleEntry), its lead as a time, left out when it follows
// each signal's, and the labels it requires, left out for none.
type summaryEntry struct {
	Rules   string          `json:"rules,omitempty"`
	Labels  incident.Labels `json:"labels,omitempty"`
	Signals []string        `json:"signals"`
	Match   []string        `json:"match"`
	Lead    string          `json:"lead,omitempty"`
	Require []string        `json:"require,omitempty"`
}

// summaryRow is a week, for everyone (Team "") or for one team: its rules'
// firings, and the incidents it owns.
type summaryRow struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Partial bool      `json:"partial"`
	Team    string    `json:"team"`
	// Firings are those judged; Caught those that caught an incident.
	// Precision, and every share caught, is null with no firings, and with
	// no record (NoRecord).
	Firings   int      `json:"firings"`
	Caught    int      `json:"caught"`
	Precision *float64 `json:"precision"`
	// NoRecord says no incident was on in the week, or in the two weeks
	// before it, and no impact signal was read over it: its firings are
	// counted, not judged. With named impact signals, none of its firings
	// was judged, by the signals tied to them.
	NoRecord bool `json:"no_record"`
	// Unjudged are its firings that caught nothing and had no record:
	// counted, not judged, and in no precision, as the report counts them
	// (docs/design/signals.md, "How they are counted"). NotJudged are of
	// them those of rules an entry says are judged by nothing (judged:
	// false, docs/design/judging.md, "Rules judged by nothing").
	Unjudged  int `json:"unjudged"`
	NotJudged int `json:"not_judged"`
	// Excused are firings quiet windows excused, in none of the others.
	Excused    int                     `json:"excused"`
	Critical   summaryCount            `json:"critical"`
	BySeverity map[string]summaryCount `json:"by_severity"`
	Refired    summaryRefired          `json:"refired"`
	// OffHours and Asleep are null when no hours are given.
	OffHours      *summaryHours    `json:"off_hours"`
	Asleep        *summaryHours    `json:"asleep"`
	DuringDeploys summaryDeploys   `json:"during_deploys"`
	Incidents     summaryIncidents `json:"incidents"`
	Impacts       summaryImpacts   `json:"impacts"`
	// Episodes are the week's firings counted as episodes
	// (docs/design/judging.md, item 2), beside them, changing none of
	// them: null until the ledger has counted the week's (after an
	// upgrade, or a change of zone).
	Episodes *summaryEpisodes `json:"episodes"`
	// People are people's verdicts on the week's firings
	// (docs/design/judging.md, item 3), and the numbers as Lapilli's own
	// judging would have them.
	People summaryPeople `json:"people"`
	// Goals are the goals --config declares of the row's team (everyone's,
	// in a row for everyone; the team's own and each team's, in a team's),
	// each with its value and the week's result (docs/design/goals.md); left
	// out with none declared.
	Goals []summaryGoal `json:"goals,omitempty"`
	// crit are the critical firings by each list of severities a goal
	// counts as critical (ledger.CriticalKey); whole says the ledger holds
	// the week whole, after the one its first firing fell in; settling,
	// that the runs haven't gone past its end by what a week waits for
	// before its goals are met or missed (wholeAfter); byCatching, of a
	// team's row, that the team has no labels, and so owns only the
	// incidents its rules caught.
	crit       map[string]summaryCount
	whole      bool
	settling   bool
	byCatching bool
	// Marked holds each kind of marked window with an occurrence in the
	// week (docs/design/marked.md), deploys left out: they are
	// DuringDeploys. {} in a week with none.
	Marked map[string]summaryMarked `json:"marked"`
}

// summaryMarked is a week's firings and incidents during the windows of a
// kind: Windows, its occurrences in the week; the firings marked with it,
// those that caught something and re-fired, with precision inside the
// kind and outside it; and the incidents and impact windows that started
// during one of its occurrences and agree with it.
type summaryMarked struct {
	Windows          int              `json:"windows"`
	Firings          int              `json:"firings"`
	Caught           int              `json:"caught"`
	Precision        *float64         `json:"precision"`
	PrecisionOutside *float64         `json:"precision_outside"`
	Refired          int              `json:"refired"`
	Incidents        summaryIncidents `json:"incidents"`
	Impacts          summaryIncidents `json:"impacts"`
	// Episodes are those whose first firing is marked with the kind, and
	// EpisodesCaught those of them with a white firing; null, with
	// EpisodesPrecision, until the ledger has counted the week's.
	Episodes          *int     `json:"episodes"`
	EpisodesCaught    *int     `json:"episodes_caught"`
	EpisodesPrecision *float64 `json:"episodes_precision"`
}

// summaryEpisodes are a row's episodes: Total, Caught those with a white
// firing, Unjudged those not judged (no white firing, and no record at
// their first firing's), Precision Caught over those judged (null as the
// row's is), and NoisyRules how many of the row's rules are noisy by
// their episodes in the week, by the same thresholds as by firings
// (noisy_at), which decides nothing.
type summaryEpisodes struct {
	Total      int      `json:"total"`
	Caught     int      `json:"caught"`
	Unjudged   int      `json:"unjudged"`
	Precision  *float64 `json:"precision"`
	NoisyRules int      `json:"noisy_rules"`
}

// summaryPeople are a row's people's verdicts: Unrelated, its firings
// white as Lapilli judged them and black by a person's word, and
// UnrelatedExcused those of them a quiet window then excused; Real, the
// incidents "This was real" wrote from a firing of its rules that started
// in the week, RealWhite its firings white only by such incidents, and
// RealExcused those of them a quiet window that matches them would have
// excused as Lapilli judged them; CaughtByLapilli and PrecisionByLapilli,
// what caught something and the precision as Lapilli's own judging would
// have them; Action, the answers to "needed action".
type summaryPeople struct {
	Unrelated          int           `json:"unrelated"`
	UnrelatedExcused   int           `json:"unrelated_excused"`
	Real               int           `json:"real"`
	RealWhite          int           `json:"real_white"`
	RealExcused        int           `json:"real_excused"`
	CaughtByLapilli    int           `json:"caught_by_lapilli"`
	PrecisionByLapilli *float64      `json:"precision_by_lapilli"`
	Action             summaryAction `json:"action"`
}

// summaryAction are a row's answers to whether a firing needed someone to
// act: of its Firings, those Answered, Yes and No; Share, yes over those
// answered, given only once ActionAnsweredShare of the firings are
// answered (null below); YesBlack, needed action and caught nothing;
// NoWhite, needed none and caught something.
type summaryAction struct {
	Firings  int      `json:"firings"`
	Answered int      `json:"answered"`
	Yes      int      `json:"yes"`
	No       int      `json:"no"`
	Share    *float64 `json:"share"`
	YesBlack int      `json:"yes_black"`
	NoWhite  int      `json:"no_white"`
}

// ActionAnsweredShare is the share of a row's firings that must be
// answered before action.share is given: below it, the share would follow
// how much was answered, not the alerts.
const ActionAnsweredShare = 0.8

// actionShare is yes over those answered, once enough of the firings are.
func actionShare(a summaryAction) *float64 {
	if a.Answered == 0 || float64(a.Answered) < ActionAnsweredShare*float64(a.Firings) {
		return nil
	}
	return share(a.Yes, a.Answered)
}

type summaryCount struct {
	Firings int      `json:"firings"`
	Caught  int      `json:"caught"`
	Share   *float64 `json:"share_caught"`
}

type summaryRefired struct {
	Firings int      `json:"firings"`
	Share   *float64 `json:"share"`
	Rules   int      `json:"rules"`
	// Worst is the alert that re-fired most.
	Worst *ledger.Refired `json:"worst"`
}

type summaryHours struct {
	Firings int `json:"firings"`
	Nothing int `json:"nothing"`
}

type summaryDeploys struct {
	Firings int `json:"firings"`
	Caught  int `json:"caught"`
	// PrecisionWithout is the week's precision leaving them out.
	PrecisionWithout *float64 `json:"precision_without"`
}

type summaryIncidents struct {
	Total     int `json:"total"`
	Caught    int `json:"caught"`
	NoisyOnly int `json:"noisy_only"`
	Missed    int `json:"missed"`
	// MedianLead is how long before the incident's start the first alert
	// of a rule that isn't noisy came, the median of those caught, in
	// seconds: negative when it came after.
	MedianLead *int64 `json:"median_lead_seconds"`
}

// summaryImpacts are the impact windows of a week, all signals' counted
// together, and each named signal's apart (BySignal, {} with none named):
// a window of a signal since removed under its name, as it is kept.
type summaryImpacts struct {
	summaryIncidents
	BySignal map[string]summaryIncidents `json:"by_signal"`
}

// summaryOf works the summary out for the weeks from Monday w1 to Monday w2,
// a row a week for each of who: a team, or nil for everyone.
func (s *Server) summaryOf(ctx context.Context, w1, w2 int64, who []*ledger.Team, critical []string) (*summary, error) {
	o, err := s.options(ctx)
	if err != nil {
		return nil, err
	}
	own, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	cursor, err := s.l.Cursor(ctx)
	if err != nil {
		return nil, err
	}
	// What a week's goals wait for past its end, as the digest does.
	wait, err := s.wholeAfterNow(ctx, o)
	if err != nil {
		return nil, err
	}
	deploys, err := s.l.HasDeploys(ctx)
	if err != nil {
		return nil, err
	}
	// The goals, each row's checked against those of its team: their lists
	// of severities counted as critical, four at most, each counted over
	// the week's rules as critical is; and where the ledger's weeks begin.
	gs, err := s.goalsNow(ctx)
	if err != nil {
		return nil, err
	}
	lists := ledger.CriticalLists(gs)
	var firstWeek int64
	firstOK := false
	if len(gs) > 0 {
		first, _, err := s.l.Bounds(ctx)
		if err != nil {
			return nil, err
		}
		if !first.IsZero() {
			firstWeek, firstOK = ledger.Week(s.l.Day(first)), true
		}
	}
	at := verdict.NoisyAt{Firings: o.NoisyFirings, Precision: o.NoisyPrecision}
	if at.Firings == 0 {
		at.Firings = verdict.NoisyFirings
	}
	if at.Precision == 0 {
		at.Precision = verdict.NoisyPrecision
	}
	match := o.Match
	if match == nil {
		match = []string{}
	}
	out := &summary{Schema: SummarySchema, From: s.l.Midnight(w1), To: s.l.Midnight(w2 + 7), Zone: s.cfg.Loc.String(), LedgerTo: cursor,
		Settings: summarySettings{Lead: verdict.Duration(o.Lead), Refire: verdict.Duration(o.RefireWithin),
			DeployBefore: verdict.Duration(o.DeployBefore), DeployAfter: verdict.Duration(o.DeployAfter),
			Match: match, NoisyAt: at, Critical: critical, Deploys: deploys}, Rows: []summaryRow{}}
	if o.Signals.Named() {
		for _, x := range o.Signals.List {
			sg := summarySignal{Name: x.Name, For: verdict.Duration(x.For), Match: x.Match, Record: x.Record, Lead: leadShown(x.Lead)}
			if !x.Start.IsZero() {
				at := x.Start.UTC()
				sg.Start = &at
			}
			out.Settings.ImpactSignals = append(out.Settings.ImpactSignals, sg)
		}
		for _, e := range o.Signals.Entries {
			out.Settings.ImpactRules = append(out.Settings.ImpactRules, summaryEntry{Rules: e.Rules, Labels: e.Labels, Signals: e.Signals, Match: e.Match,
				Lead: leadShown(e.Lead), Require: e.Require})
		}
	}
	t := &tallies{by: map[int64]map[int64]ledger.Tally{}, o: o}
	for w := w1; w <= w2; w += 7 {
		t.weeks = append(t.weeks, w)
		t.by[w] = map[int64]ledger.Tally{}
	}
	if err := s.fillTallies(ctx, t, w1, w2); err != nil {
		return nil, err
	}
	if t.rules, err = s.l.RuleTable(ctx); err != nil {
		return nil, err
	}
	if err := s.readRecord(ctx, t); err != nil {
		return nil, err
	}
	hours, err := s.byHours(ctx, t)
	if err != nil {
		return nil, err
	}
	out.Settings.Hours = hours != nil
	incs, err := s.l.Incidents(ctx, out.From, out.To, false)
	if err != nil {
		return nil, err
	}
	// Each week's own, read once for every team's row; and the rules "This
	// was real" wrote any of them from.
	weekOf := map[int64][]ledger.Incident{}
	var realSeries []int64
	for _, in := range incs {
		w := ledger.Week(s.l.Day(in.Start))
		weekOf[w] = append(weekOf[w], in)
		if in.Real {
			realSeries = append(realSeries, in.RealSeries())
		}
	}
	realRule, err := s.l.RulesOfSeries(ctx, realSeries)
	if err != nil {
		return nil, err
	}
	marked, err := s.markedWeeks(ctx, t, w1, w2, own)
	if err != nil {
		return nil, err
	}
	// The goals over a period, each occurrence in the row of the week it
	// ended in (goalsperiod.go): none without such a goal.
	periods, err := s.periodGoals(ctx, w1, w2, who, gs, own, cursor.Add(-wait), firstWeek, firstOK)
	if err != nil {
		return nil, err
	}
	for _, w := range t.weeks {
		from, to := s.l.Midnight(w), s.l.Midnight(w+7)
		refired, err := s.l.MostRefired(ctx, from, to, -1, nil)
		if err != nil {
			return nil, err
		}
		for _, team := range who {
			if summaryRowSeen != nil {
				summaryRowSeen()
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			mine := func(rule string) bool { return team == nil || own.rule[rule] == team.ID }
			row := summaryRow{From: from, To: to, Partial: cursor.Before(to), BySeverity: map[string]summaryCount{},
				Impacts: summaryImpacts{BySignal: map[string]summaryIncidents{}}}
			if team != nil {
				row.Team = team.Name
			}
			var crit summaryCount
			crits := make([]summaryCount, len(lists))
			refiredRules := 0
			// The firings that caught nothing and had no record (counted,
			// not judged), of the row and of each of its counts, as the
			// report counts unjudged: none in a week with a record and no
			// named signal.
			var unjudgedDeploys, unjudgedCrit int
			unjudgedSev := map[string]int{}
			unjudgedCrits := make([]int, len(lists))
			var ep summaryEpisodes
			for id, x := range t.by[w] {
				rule := t.rules[id]
				if !mine(rule.Name) {
					continue
				}
				u := t.unjudged(w, id)
				ub := u.Firings - u.White
				ep.Total += x.Episodes
				ep.Caught += x.EpisodesWhite
				ep.Unjudged += u.Episodes - u.EpisodesWhite
				if j := t.judged(w, id); noisy(j.Episodes, j.EpisodesWhite, t.o) {
					ep.NoisyRules++
				}
				row.Firings += x.Firings
				row.Caught += x.White
				row.Unjudged += ub
				if t.o.NotJudged(rule.Name) {
					row.NotJudged += x.Firings
				}
				unjudgedDeploys += u.Deploy - u.DeployWhite
				row.Excused += x.Excused
				row.Refired.Firings += x.Refired
				row.DuringDeploys.Firings += x.Deploy
				row.DuringDeploys.Caught += x.DeployWhite
				if x.Refired > 0 {
					refiredRules++
				}
				sev := rule.Severity
				c := row.BySeverity[sev]
				c.Firings += x.Firings
				c.Caught += x.White
				row.BySeverity[sev] = c
				unjudgedSev[sev] += ub
				if slices.ContainsFunc(strings.Split(sev, "/"), func(v string) bool { return slices.Contains(critical, v) }) {
					crit.Firings += x.Firings
					crit.Caught += x.White
					unjudgedCrit += ub
				}
				for i, cs := range lists {
					if slices.ContainsFunc(strings.Split(sev, "/"), func(v string) bool { return slices.Contains(cs, v) }) {
						crits[i].Firings += x.Firings
						crits[i].Caught += x.White
						unjudgedCrits[i] += ub
					}
				}
			}
			// With no record, what was caught is no share of anything known:
			// no firing of the row judged (docs/design/signals.md, "How
			// they are counted"), all or none of a week's with no named
			// signal.
			row.NoRecord = !t.kept[w] || row.Firings > 0 && row.Unjudged == row.Firings
			if t.o.Signals.Named() {
				row.NoRecord = verdict.NoRecordOf(row.Firings, row.Unjudged, func() bool { return !t.keptAny(w) })
			}
			judged := func(n, of, unjudged int) *float64 {
				if row.NoRecord {
					return nil
				}
				return share(n, of-unjudged)
			}
			for sev, c := range row.BySeverity {
				c.Share = judged(c.Caught, c.Firings, unjudgedSev[sev])
				row.BySeverity[sev] = c
			}
			crit.Share = judged(crit.Caught, crit.Firings, unjudgedCrit)
			row.Critical = crit
			row.Precision = judged(row.Caught, row.Firings, row.Unjudged)
			if t.episodesCounted(w) {
				ep.Precision = judged(ep.Caught, ep.Total, ep.Unjudged)
				row.Episodes = &ep
			}
			row.Refired.Share, row.Refired.Rules = share(row.Refired.Firings, row.Firings), refiredRules
			row.People = t.people(w, mine, row, weekOf[w], realRule)
			for i, x := range refired {
				if mine(x.Rule) {
					row.Refired.Worst = &refired[i]
					break
				}
			}
			row.DuringDeploys.PrecisionWithout = judged(row.Caught-row.DuringDeploys.Caught, row.Firings-row.DuringDeploys.Firings, row.Unjudged-unjudgedDeploys)
			if hours != nil {
				row.OffHours, row.Asleep = &summaryHours{}, &summaryHours{}
				for id, x := range hours[w] {
					if !mine(t.rules[id].Name) {
						continue
					}
					row.OffHours.Firings += x.OffHours
					row.OffHours.Nothing += x.OffHoursBlack
					row.Asleep.Firings += x.Asleep
					row.Asleep.Nothing += x.AsleepBlack
				}
			}
			var leads, impactLeads []time.Duration
			bySignal := map[string][]time.Duration{}
			sigs := map[string]*summaryIncidents{}
			for _, in := range weekOf[w] {
				if team != nil && !own.ownsIncident(*team, in) {
					continue
				}
				sum, ls := &row.Incidents, &leads
				if in.Origin == "impact" {
					sum, ls = &row.Impacts.summaryIncidents, &impactLeads
				}
				// And a named signal's window, under its signal.
				var sig *summaryIncidents
				if in.Origin == "impact" && in.Signal != "" {
					if sig = sigs[in.Signal]; sig == nil {
						sig = &summaryIncidents{}
						sigs[in.Signal] = sig
					}
				}
				state := t.state(in, w)
				for _, c := range []*summaryIncidents{sum, sig} {
					if c == nil {
						continue
					}
					c.Total++
					switch state {
					case "caught":
						c.Caught++
					case "noisy":
						c.NoisyOnly++
					default:
						c.Missed++
					}
				}
				if state == "caught" {
					for _, c := range in.CaughtBy {
						if !t.noisy(w, t.id(c.Rule)) {
							*ls = append(*ls, -c.Delay)
							if sig != nil {
								bySignal[in.Signal] = append(bySignal[in.Signal], -c.Delay)
							}
							break
						}
					}
				}
			}
			row.Incidents.MedianLead, row.Impacts.MedianLead = medianSeconds(leads), medianSeconds(impactLeads)
			for name, c := range sigs {
				c.MedianLead = medianSeconds(bySignal[name])
				row.Impacts.BySignal[name] = *c
			}
			if mine := rowGoals(gs, team); len(mine) > 0 {
				row.crit = make(map[string]summaryCount, len(lists))
				for i, cs := range lists {
					c := crits[i]
					c.Share = judged(c.Caught, c.Firings, unjudgedCrits[i])
					row.crit[ledger.CriticalKey(cs)] = c
				}
				row.whole = firstOK && w > firstWeek && !row.Partial
				row.settling = cursor.Add(-wait).Before(to)
				row.byCatching = team != nil && len(team.Labels) == 0
				for _, g := range mine {
					row.Goals = append(row.Goals, goalOn(g, &row))
				}
			}
			row.Goals = append(row.Goals, periods[periodAt{w, teamIDOf(team)}]...)
			row.Marked = marked.row(w, team, mine, row, t, weekOf[w], judged)
			out.Rows = append(out.Rows, row)
		}
	}
	return out, nil
}

// medianSeconds is the median of ds, in seconds; nil for none.
func medianSeconds(ds []time.Duration) *int64 {
	if len(ds) == 0 {
		return nil
	}
	w := weekView{Delays: ds}
	d, _ := w.median()
	n := int64(d / time.Second)
	return &n
}

// apiSummary is GET /api/v1/summary?from=&to=&team=&critical=: from and to
// a day each (2026-11-02), or an RFC 3339 time, the weeks they fall in and
// those between, the last four whole weeks and this one when not given, 53
// at most; team, repeatable, a team's name, or * alone for everyone and
// every team, everyone's when not given; critical, the severities counted
// as critical, comma-separated (default critical), 32 at most. A summary of
// more than 53 rows, or of every team, is heavy: it takes the one turn
// there is for such a request, held until the answer is written.
func (s *Server) apiSummary(w http.ResponseWriter, r *http.Request) {
	ask, err := s.summaryAsk(r.Context(), r.URL.Query())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if ask.heavy() {
		done, err := s.busy(r.Context())
		if err != nil {
			s.oops(w, r, err)
			return
		}
		defer done()
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(busyWait))
	}
	s.cached(func(w http.ResponseWriter, r *http.Request) {
		sum, err := s.summaryOf(r.Context(), ask.w1, ask.w2, ask.who, ask.critical)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		writeJSON(w, sum)
	})(w, r)
}

// summaryAsked is what a summary is asked for: its weeks, for whom, and
// what counts as critical.
type summaryAsked struct {
	w1, w2   int64
	who      []*ledger.Team
	critical []string
	every    bool // team=*
}

// heavy reports whether a summary takes the heavy turn: every team's, or
// more rows than a year's of one.
func (a summaryAsked) heavy() bool {
	return a.every || ((a.w2-a.w1)/7+1)*int64(len(a.who)) > summaryMaxWeeks
}

// Bounds on what a summary is asked for.
const (
	summaryMaxCritical    = 32
	summaryMaxSeverityLen = 64
)

// summaryAsk reads what a summary is asked for, as apiSummary and MCP's
// summary take it, and refuses what is past its bounds.
func (s *Server) summaryAsk(ctx context.Context, q url.Values) (summaryAsked, error) {
	today := s.l.Day(s.now())
	a := summaryAsked{w2: ledger.Week(today), critical: []string{"critical"}}
	a.w1 = a.w2 - 4*7
	for _, x := range []struct {
		name string
		to   *int64
	}{{"from", &a.w1}, {"to", &a.w2}} {
		v := q.Get(x.name)
		if v == "" {
			continue
		}
		day, err := dateDay(v)
		if err != nil {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return a, bad("%s %q: want a day like 2026-11-02, or an RFC 3339 time", x.name, v)
			}
			day = s.l.Day(t)
		}
		*x.to = ledger.Week(day)
	}
	switch {
	case a.w2 < a.w1:
		return a, bad("to is before from")
	case (a.w2-a.w1)/7+1 > summaryMaxWeeks:
		return a, bad("a summary is of %d weeks at most", summaryMaxWeeks)
	}
	if v := q.Get("critical"); v != "" {
		a.critical = nil
		for _, c := range strings.Split(v, ",") {
			if c = strings.TrimSpace(c); c != "" && !slices.Contains(a.critical, c) {
				if len(c) > summaryMaxSeverityLen || len(a.critical) == summaryMaxCritical {
					return a, bad("critical is %d severities at most, each %d bytes at most", summaryMaxCritical, summaryMaxSeverityLen)
				}
				a.critical = append(a.critical, c)
			}
		}
	}
	own, err := s.owners(ctx)
	if err != nil {
		return a, err
	}
	a.who = []*ledger.Team{nil}
	names := q["team"]
	if len(names) == 0 {
		return a, nil
	}
	if slices.Contains(names, "*") {
		if len(names) > 1 {
			return a, bad("team=* is everyone and every team: give it alone")
		}
		a.every = true
		for i := range own.teams {
			a.who = append(a.who, &own.teams[i])
		}
		return a, nil
	}
	if len(names) > len(own.teams) {
		return a, bad("there are %d teams: name each once", len(own.teams))
	}
	a.who = nil
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		t, ok := own.team(name)
		if !ok {
			return a, bad("there is no team called %q", name)
		}
		a.who = append(a.who, &t)
	}
	return a, nil
}

// summaryFor is the summary a query asks for, through MCP: in the heavy
// turn when it is heavy, as apiSummary's.
func (s *Server) summaryFor(ctx context.Context, q url.Values) (*summary, error) {
	a, err := s.summaryAsk(ctx, q)
	if err != nil {
		return nil, err
	}
	if a.heavy() {
		done, err := s.busy(ctx)
		if err != nil {
			return nil, err
		}
		defer done()
	}
	return s.summaryOf(ctx, a.w1, a.w2, a.who, a.critical)
}

// refires are a span's re-fires and firings during deploys, as the digest
// and reports tell of them, of the rules in a report.
type refires struct {
	Firings int      `json:"firings"`
	Refired int      `json:"refired"`
	Share   *float64 `json:"share"`
	Rules   int      `json:"rules"`
	// Worst is the alert that re-fired most in the span.
	Worst *ledger.Refired `json:"worst"`
	// Weeks are how many of the Of whole weeks before this one, of the
	// twelve, the ledger has all of, had a re-fire (tallies.whole).
	Weeks int `json:"weeks"`
	Of    int `json:"of"`
	// During deploys: firings, those of them that caught something, and
	// precision with them and without.
	Deploys          int      `json:"during_deploys"`
	DeploysCaught    int      `json:"during_deploys_caught"`
	Precision        *float64 `json:"precision"`
	PrecisionWithout *float64 `json:"precision_without"`
	caught           int
	judged           int // the firings judged, deploysJudged of them during deploys
	deploysJudged    int
	deploysKept      bool
}

// refiresOf works refires out for the rules of rep, over [from, to).
func (s *Server) refiresOf(ctx context.Context, rep *verdict.Report, from, to time.Time) (*refires, error) {
	in := map[string]bool{}
	x := &refires{}
	white := 0
	for _, r := range rep.Rules {
		in[r.Rule] = true
		x.Firings += r.Firings
		white += r.White
		x.Refired += r.Refired
		x.Deploys += r.DuringDeploys
		x.DeploysCaught += r.DeploysCaught
		// Precision of the firings judged alone: a week with no record's
		// that caught nothing aren't (verdict.RuleResult.Unjudged).
		x.judged += r.Firings - r.Unjudged
		x.deploysJudged += r.DuringDeploys - r.DeploysUnjudged
		if r.Refired > 0 {
			x.Rules++
		}
	}
	x.Share, x.caught = share(x.Refired, x.Firings), white
	x.Precision, x.PrecisionWithout = share(white, x.judged), share(white-x.DeploysCaught, x.judged-x.deploysJudged)
	if x.Refired > 0 {
		worst, err := s.l.MostRefired(ctx, from, to, 1, func(rule string) bool { return in[rule] })
		if err != nil {
			return nil, err
		}
		if len(worst) > 0 {
			x.Worst = &worst[0]
		}
	}
	t, err := s.tallies(ctx, weeks+1)
	if err != nil {
		return nil, err
	}
	x.Weeks, x.Of = t.refireWeeks(t.weeks, func(id int64) bool { return in[t.rules[id].Name] })
	if x.deploysKept, err = s.l.HasDeploys(ctx); err != nil {
		return nil, err
	}
	return x, nil
}

// refireLine says how many firings re-fired, the alert that did most, and
// in how many whole weeks one did.
func (x *refires) refireLine(l string) string {
	weeks := ""
	if x.Of > 0 {
		weeks = " " + tr(l, "Weeks with a re-fire, of the last %d whole weeks: %d.", x.Of, x.Weeks)
	}
	if x.Refired == 0 {
		return tr(l, "No firing re-fired.") + weeks
	}
	line := tr(l, "Re-fired: %d of %d firings (%s), of %d rules.", x.Refired, x.Firings, pct(x.Refired, x.Firings), x.Rules)
	if w := x.Worst; w != nil {
		line += " " + tr(l, "The most, %s %s: %d times.", verdict.Clean(w.Rule), verdict.Clean(labelsLine(w.Labels)), w.Refired)
	}
	return line + weeks
}

// deployLine says how many firings came during deploys; "" when the
// ledger keeps none.
func (x *refires) deployLine(l string) string {
	if !x.deploysKept {
		return ""
	}
	return tr(l, "During deploys: %d firings, %d of them catching something; precision %s with them, %s without.",
		x.Deploys, x.DeploysCaught, pct(x.caught, x.judged), pct(x.caught-x.DeploysCaught, x.judged-x.deploysJudged))
}

// labelsLine is a label set as a person reads it, in braces.
func labelsLine(m map[string]string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, k+"="+m[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// people works out a row's people's verdicts (summaryPeople) from the
// week's tallies by the signals its firings are tied to, of the rules
// mine says are the row's, its incidents and the rules "This was real"
// wrote them from.
func (t *tallies) people(w int64, mine func(string) bool, row summaryRow, incs []ledger.Incident, realRule map[int64]string) summaryPeople {
	var p summaryPeople
	var judgedL, judgedWhiteL int
	for _, x := range t.parts {
		if x.At != w || !mine(t.rules[x.Rule].Name) {
			continue
		}
		p.Unrelated += x.Unrelated
		p.UnrelatedExcused += x.UnrelatedExcused
		p.RealWhite += x.RealWhite
		p.RealExcused += x.RealExcused
		p.Action.Yes += x.ActionYes
		p.Action.No += x.ActionNo
		p.Action.YesBlack += x.ActionYesBlack
		p.Action.NoWhite += x.ActionNoWhite
		_, white, judged := t.byLapilli(x)
		p.CaughtByLapilli += white
		judgedL += judged
		judgedWhiteL += min(white, judged)
	}
	for _, in := range incs {
		if rule, ok := realRule[in.RealSeries()]; ok && in.Real && mine(rule) {
			p.Real++
		}
	}
	if judgedL > 0 {
		p.PrecisionByLapilli = share(judgedWhiteL, judgedL)
	}
	p.Action.Firings = row.Firings
	p.Action.Answered = p.Action.Yes + p.Action.No
	p.Action.Share = actionShare(p.Action)
	return p
}

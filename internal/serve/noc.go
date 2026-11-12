package serve

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

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
}

// summaryRow is a week, for everyone (Team "") or for one team: its rules'
// firings, and the incidents it owns.
type summaryRow struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Partial bool      `json:"partial"`
	Team    string    `json:"team"`
	// Firings are those judged; Caught those that caught an incident.
	Firings   int      `json:"firings"`
	Caught    int      `json:"caught"`
	Precision *float64 `json:"precision"`
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
	Impacts       summaryIncidents `json:"impacts"`
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
	deploys, err := s.l.HasDeploys(ctx)
	if err != nil {
		return nil, err
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
	t := &tallies{by: map[int64]map[int64]ledger.Tally{}, o: o}
	for w := w1; w <= w2; w += 7 {
		t.weeks = append(t.weeks, w)
		t.by[w] = map[int64]ledger.Tally{}
	}
	rows, err := s.l.Weeks(ctx, w1, w2)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		t.by[r.At][r.Rule] = r
	}
	if t.rules, err = s.l.RuleTable(ctx); err != nil {
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
	// Each week's own, read once for every team's row.
	weekOf := map[int64][]ledger.Incident{}
	for _, in := range incs {
		w := ledger.Week(s.l.Day(in.Start))
		weekOf[w] = append(weekOf[w], in)
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
			row := summaryRow{From: from, To: to, Partial: cursor.Before(to), BySeverity: map[string]summaryCount{}}
			if team != nil {
				row.Team = team.Name
			}
			var crit summaryCount
			refiredRules := 0
			for id, x := range t.by[w] {
				rule := t.rules[id]
				if !mine(rule.Name) {
					continue
				}
				row.Firings += x.Firings
				row.Caught += x.White
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
				if slices.ContainsFunc(strings.Split(sev, "/"), func(v string) bool { return slices.Contains(critical, v) }) {
					crit.Firings += x.Firings
					crit.Caught += x.White
				}
			}
			for sev, c := range row.BySeverity {
				c.Share = share(c.Caught, c.Firings)
				row.BySeverity[sev] = c
			}
			crit.Share = share(crit.Caught, crit.Firings)
			row.Critical = crit
			row.Precision = share(row.Caught, row.Firings)
			row.Refired.Share, row.Refired.Rules = share(row.Refired.Firings, row.Firings), refiredRules
			for i, x := range refired {
				if mine(x.Rule) {
					row.Refired.Worst = &refired[i]
					break
				}
			}
			row.DuringDeploys.PrecisionWithout = share(row.Caught-row.DuringDeploys.Caught, row.Firings-row.DuringDeploys.Firings)
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
			for _, in := range weekOf[w] {
				if team != nil && !own.ownsIncident(*team, in) {
					continue
				}
				sum, ls := &row.Incidents, &leads
				if in.Origin == "impact" {
					sum, ls = &row.Impacts, &impactLeads
				}
				sum.Total++
				switch t.state(in, w) {
				case "caught":
					sum.Caught++
					for _, c := range in.CaughtBy {
						if !t.noisy(w, t.id(c.Rule)) {
							*ls = append(*ls, -c.Delay)
							break
						}
					}
				case "noisy":
					sum.NoisyOnly++
				default:
					sum.Missed++
				}
			}
			row.Incidents.MedianLead, row.Impacts.MedianLead = medianSeconds(leads), medianSeconds(impactLeads)
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
		t, err := time.ParseInLocation("2006-01-02", v, s.cfg.Loc)
		if err != nil {
			if t, err = time.Parse(time.RFC3339, v); err != nil {
				return a, bad("%s %q: want a day like 2026-11-02, or an RFC 3339 time", x.name, v)
			}
		}
		*x.to = ledger.Week(s.l.Day(t))
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
		if r.Refired > 0 {
			x.Rules++
		}
	}
	x.Share, x.caught = share(x.Refired, x.Firings), white
	x.Precision, x.PrecisionWithout = share(white, x.Firings), share(white-x.DeploysCaught, x.Firings-x.Deploys)
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
		x.Deploys, x.DeploysCaught, pct(x.caught, x.Firings), pct(x.caught-x.DeploysCaught, x.Firings-x.Deploys))
}

// labelsLine is a label set as a person reads it, in braces.
func labelsLine(m map[string]string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, k+"="+m[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

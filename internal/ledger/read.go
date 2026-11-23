package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// RuleRow is a rule and its id.
type RuleRow struct {
	ID int64
	Rule
	// Sent is, for a rule copied in, the name its source sends it by: its
	// Name, but where another rule had that.
	Sent string
}

// RuleTable is every rule the ledger has, by id.
func (l *Ledger) RuleTable(ctx context.Context) (map[int64]RuleRow, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT id, name, grp, severity, unsupported, source, sent FROM rules")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]RuleRow{}
	judged := false
	for rows.Next() {
		var r RuleRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Group, &r.Severity, &r.Unsupported, &r.Source, &r.Sent); err != nil {
			return nil, err
		}
		out[r.ID] = r
		judged = judged || r.Source == GrafanaSource && r.Severity == ""
	}
	if err := rows.Err(); err != nil || !judged {
		return out, err
	}
	rows.Close()
	// A rule judged from Grafana's history is the recorded rule its
	// firings are copied in as, which keeps no severity of its own: its
	// labels', as the runs read them (SetJudgedSeverity), so that it shows
	// and sorts as every other rule does.
	sev, err := l.judgedSeverity(ctx, l.r)
	if err != nil {
		return nil, err
	}
	for id, r := range out {
		if r.Source == GrafanaSource && r.Severity == "" && sev[r.Name] != "" {
			r.Severity = sev[r.Name]
			out[id] = r
		}
	}
	return out, nil
}

// Tally is a rule's stones over a day or a week.
type Tally struct {
	At   int64 // the day, or the week's Monday
	Rule int64
	// Firings are those judged: not those a quiet window excused.
	Firings int
	White   int
	Refired int
	Excused int
	// Deploy are the firings judged that came during a deploy, and
	// DeployWhite those of them that caught something.
	Deploy, DeployWhite int
	// Signals are the signals the firings' alerts are tied to, as the
	// ledger keeps them (verdict.SignalBits), of a tally by them (WeeksBy,
	// DaysBy): 0 for none, every alert's while no signal is named.
	Signals int64
	// Episodes are the episodes whose first firing is counted here
	// (docs/design/judging.md, item 2), and EpisodesWhite those of them
	// with a white firing: beside the firings, changing none of them.
	Episodes, EpisodesWhite int
	// People's verdicts (docs/design/judging.md, item 3): Unrelated the
	// firings white as Lapilli judged them a person said were unrelated,
	// excused or not, UnrelatedExcused those of them a quiet window then
	// excused; RealWhite the firings white only by what "This was real"
	// wrote, RealExcused those of them a quiet window would have excused
	// as Lapilli judged them; ActionYes and ActionNo the answers to needed
	// action, of the firings not excused.
	Unrelated, UnrelatedExcused, RealWhite, RealExcused, ActionYes, ActionNo int
	// ActionYesBlack are of ActionYes those that caught nothing,
	// ActionNoWhite of ActionNo those that caught something.
	ActionYesBlack, ActionNoWhite int
}

// Add is t with x's counts added.
func (t Tally) Add(x Tally) Tally {
	t.Firings += x.Firings
	t.White += x.White
	t.Refired += x.Refired
	t.Excused += x.Excused
	t.Deploy += x.Deploy
	t.DeployWhite += x.DeployWhite
	t.Episodes += x.Episodes
	t.EpisodesWhite += x.EpisodesWhite
	t.Unrelated += x.Unrelated
	t.UnrelatedExcused += x.UnrelatedExcused
	t.RealWhite += x.RealWhite
	t.RealExcused += x.RealExcused
	t.ActionYes += x.ActionYes
	t.ActionNo += x.ActionNo
	t.ActionYesBlack += x.ActionYesBlack
	t.ActionNoWhite += x.ActionNoWhite
	return t
}

// Black is how many firings caught nothing.
func (t Tally) Black() int { return t.Firings - t.White }

// Days are each rule's stones by day, from day to day, whatever signals
// its alerts are tied to.
func (l *Ledger) Days(ctx context.Context, from, to int64) ([]Tally, error) {
	return l.tallies(ctx, `SELECT day, rule_id, sum(firings), sum(white), sum(refired), sum(excused), sum(deploy), sum(deploy_white), 0, sum(episodes), sum(episodes_white), sum(unrelated), sum(unrelated_excused), sum(real_white), sum(real_excused), sum(action_yes), sum(action_no), sum(action_yes_black), sum(action_no_white) FROM days
		WHERE day >= ? AND day <= ? GROUP BY day, rule_id ORDER BY day, rule_id`, from, to)
}

// Weeks are each rule's stones by week, from the week starting on one
// Monday to that starting on another, whatever signals its alerts are
// tied to.
func (l *Ledger) Weeks(ctx context.Context, from, to int64) ([]Tally, error) {
	return l.tallies(ctx, `SELECT week, rule_id, sum(firings), sum(white), sum(refired), sum(excused), sum(deploy), sum(deploy_white), 0, sum(episodes), sum(episodes_white), sum(unrelated), sum(unrelated_excused), sum(real_white), sum(real_excused), sum(action_yes), sum(action_no), sum(action_yes_black), sum(action_no_white) FROM weeks
		WHERE week >= ? AND week <= ? GROUP BY week, rule_id ORDER BY week, rule_id`, from, to)
}

// DaysBy are Days, apart by the signals the firings' alerts are tied to:
// what a firing is judged by (docs/design/signals.md).
func (l *Ledger) DaysBy(ctx context.Context, from, to int64) ([]Tally, error) {
	return l.tallies(ctx, "SELECT day, rule_id, firings, white, refired, excused, deploy, deploy_white, signals, episodes, episodes_white, "+peopleCols+" FROM days WHERE day >= ? AND day <= ? ORDER BY day, rule_id, signals", from, to)
}

// WeeksBy are Weeks, apart by the signals the firings' alerts are tied to.
func (l *Ledger) WeeksBy(ctx context.Context, from, to int64) ([]Tally, error) {
	return l.tallies(ctx, "SELECT week, rule_id, firings, white, refired, excused, deploy, deploy_white, signals, episodes, episodes_white, "+peopleCols+" FROM weeks WHERE week >= ? AND week <= ? ORDER BY week, rule_id, signals", from, to)
}

// peopleCols are the tallies' columns of people's verdicts, in Tally's
// order.
const peopleCols = "unrelated, unrelated_excused, real_white, real_excused, action_yes, action_no, action_yes_black, action_no_white"

func (l *Ledger) tallies(ctx context.Context, q string, from, to int64) ([]Tally, error) {
	rows, err := l.r.QueryContext(ctx, q, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tally
	for rows.Next() {
		var t Tally
		if err := rows.Scan(&t.At, &t.Rule, &t.Firings, &t.White, &t.Refired, &t.Excused, &t.Deploy, &t.DeployWhite, &t.Signals, &t.Episodes, &t.EpisodesWhite,
			&t.Unrelated, &t.UnrelatedExcused, &t.RealWhite, &t.RealExcused, &t.ActionYes, &t.ActionNo, &t.ActionYesBlack, &t.ActionNoWhite); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Incidents are the incidents that started in [from, to), the earliest
// first, with the rules that caught each; with all, also the impact
// windows that don't count.
func (l *Ledger) Incidents(ctx context.Context, from, to time.Time, all bool) ([]Incident, error) {
	q := `SELECT ` + incidentCols + `
		FROM incidents i WHERE i.start_at >= ? AND i.start_at < ?`
	if !all {
		q += " AND i.counts = 1"
	}
	return l.incidents(ctx, q+" ORDER BY i.start_at, i.key", from.Unix(), to.Unix())
}

// IncidentByID is the incident with that id.
func (l *Ledger) IncidentByID(ctx context.Context, key string) (Incident, error) {
	out, err := l.incidents(ctx, `SELECT `+incidentCols+`
		FROM incidents i WHERE i.key = ?`, key)
	if err != nil {
		return Incident{}, err
	}
	if len(out) == 0 {
		return Incident{}, ErrNotFound
	}
	return out[0], nil
}

// incidentCols are the columns of incidents l.incidents reads, in order.
const incidentCols = "i.id, i.key, i.origin, i.title, i.start_at, i.end_at, i.labels, i.state, i.counts, i.updated, i.updated_by, i.signal, i.match_labels, i.real_of, i.alerts"

func (l *Ledger) incidents(ctx context.Context, q string, args ...any) ([]Incident, error) {
	rows, err := l.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var out []Incident
	var ids []int64
	for rows.Next() {
		var in Incident
		var id, start, updated int64
		var end sql.NullInt64
		var labels, alerts string
		var match sql.NullString
		if err := rows.Scan(&id, &in.ID, &in.Origin, &in.Title, &start, &end, &labels, &in.State, &in.Counts, &updated, &in.By, &in.Signal, &match, &in.RealOf, &alerts); err != nil {
			rows.Close()
			return nil, err
		}
		if in.Alerts, err = alertsOf(alerts); err != nil {
			rows.Close()
			return nil, err
		}
		in.Real = in.RealOf != ""
		in.Start, in.End, in.Updated, in.Match = unix(start), fromNull(end), unix(updated), matchOf(match)
		in.Candidate = in.Origin == "impact"
		if err := json.Unmarshal([]byte(labels), &in.Labels); err != nil {
			rows.Close()
			return nil, err
		}
		if len(in.Labels) == 0 {
			in.Labels = nil
		}
		in.CaughtBy = []verdict.Catch{}
		out = append(out, in)
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}
	index := map[int64]int{}
	for i, id := range ids {
		index[id] = i
	}
	idsJSON, _ := json.Marshal(ids)
	rows, err = l.r.QueryContext(ctx, `SELECT c.incident_id, r.name, c.delay, c.linked FROM catches c JOIN rules r ON r.id = c.rule_id
		WHERE c.incident_id IN (`+l.d.ints("?")+`)`, string(idsJSON))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, delay int64
		var name string
		var linked bool
		if err := rows.Scan(&id, &name, &delay, &linked); err != nil {
			return nil, err
		}
		in := &out[index[id]]
		in.CaughtBy = append(in.CaughtBy, verdict.Catch{Rule: name, Delay: time.Duration(delay) * time.Second, Linked: linked})
	}
	for i := range out {
		sortCatches(out[i].CaughtBy)
	}
	return out, rows.Err()
}

func sortCatches(cs []verdict.Catch) {
	sort.Slice(cs, func(a, b int) bool {
		if cs[a].Delay != cs[b].Delay {
			return cs[a].Delay < cs[b].Delay
		}
		return cs[a].Rule < cs[b].Rule
	})
}

// Firing is a firing and its stones, as the ledger has them.
type Firing struct {
	ID int64
	// Series is the id of its alert: with its start, what keeps a person's
	// verdict on it (docs/design/judging.md, item 3).
	Series int64
	firing.Firing
	White, Refired bool
	// Excused is a firing a quiet window excused: neither white nor black.
	Excused bool
	// Deploy is a firing that came during a deploy of what it is about.
	Deploy bool
	// Kinds are the kinds of marked window it came during, in order
	// (docs/design/marked.md).
	Kinds []string
	// Signals are the signals its alert is tied to, as the ledger keeps
	// them (verdict.SignalBits).
	Signals int64
	// Episode is the id of the first firing of the episode it is of
	// (docs/design/judging.md, item 2), its own for a first; 0 for one a
	// quiet window excused, or not walked yet.
	Episode int64
	// Person are the bits judging keeps of people's word on it
	// (docs/design/judging.md, item 3): Unrelated, NeededAction. Unrelated
	// are the incidents it catches as Lapilli judges it that a person
	// said it is unrelated to.
	Person    int64
	Unrelated []string
}

// SaidUnrelated reports whether a person's word turned it black: white as
// Lapilli judged it, said unrelated to all it caught.
func (f Firing) SaidUnrelated() bool { return f.Person&personUnrelated != 0 }

// Action is a person's answer to whether it needed someone to act:
// verdict.ActionYes, verdict.ActionNo, or "".
func (f Firing) Action() string {
	switch {
	case f.Person&personYes != 0:
		return verdict.ActionYes
	case f.Person&personNo != 0:
		return verdict.ActionNo
	}
	return ""
}

// RealOnly reports whether it is white only by incidents "This was real"
// wrote: black as Lapilli judged it, white by people's word.
func (f Firing) RealOnly() bool { return f.Person&personRealWhite != 0 }

// FiringQuery picks firings: those that started in [From, To), of one rule
// when Rule is given, only black stones with Black; the latest first, Limit
// at a time, those before Before (an ID from the last page) when given.
type FiringQuery struct {
	From, To time.Time
	Rule     string
	Black    bool
	Excused  bool // only those a quiet window excused
	Refired  bool // only those that re-fired
	Deploy   bool // only those that came during a deploy
	// Kind, when given, picks those marked with the kind of marked window
	// of that name.
	Kind   string
	Before int64
	Limit  int
}

// Firings are the firings a query picks.
func (l *Ledger) Firings(ctx context.Context, q FiringQuery) ([]Firing, error) {
	sq := `SELECT f.id, r.name, s.id, s.labels, f.start_at, f.end_at, f.white, f.refired, f.excused, f.deploy, f.marked, s.signals, f.episode, f.person, f.unrelated
		FROM firings f JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id
		WHERE f.start_at >= ? AND f.start_at < ?`
	args := []any{q.From.Unix(), q.To.Unix()}
	if q.Rule != "" {
		sq += " AND f.rule_id = (SELECT id FROM rules WHERE name = ?)"
		args = append(args, q.Rule)
	}
	if q.Black {
		sq += " AND f.white = 0 AND f.excused = 0"
	}
	if q.Excused {
		sq += " AND f.excused = 1"
	}
	if q.Refired {
		sq += " AND f.refired = 1 AND f.excused = 0"
	}
	if q.Deploy {
		sq += " AND f.deploy = 1"
	}
	if q.Kind != "" {
		sq += " AND f.marked != 0 AND (f.marked & (" + kindBit + ")) != 0"
		args = append(args, q.Kind)
	}
	if q.Before > 0 {
		sq += " AND (f.start_at, f.id) < (SELECT start_at, id FROM firings WHERE id = ?)"
		args = append(args, q.Before)
	}
	sq += " ORDER BY f.start_at DESC, f.id DESC"
	if q.Limit > 0 {
		sq += " LIMIT ?"
		args = append(args, q.Limit)
	}
	rows, err := l.r.QueryContext(ctx, sq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Firing
	var bits []int64
	marked := false
	for rows.Next() {
		var f Firing
		var sid, start, b int64
		var end sql.NullInt64
		var labels, unrelated string
		if err := rows.Scan(&f.ID, &f.Rule, &sid, &labels, &start, &end, &f.White, &f.Refired, &f.Excused, &f.Deploy, &b, &f.Signals, &f.Episode, &f.Person, &unrelated); err != nil {
			return nil, err
		}
		f.Series, f.Unrelated = sid, unrelatedKeys(unrelated)
		if f.Labels, err = l.labels(sid, labels); err != nil {
			return nil, err
		}
		f.Start, f.End = unix(start), fromNull(end)
		out = append(out, f)
		bits = append(bits, b)
		marked = marked || b != 0
	}
	if err := rows.Err(); err != nil || !marked {
		return out, err
	}
	return out, l.nameKinds(ctx, out, bits)
}

// nameKinds gives each firing the names of the kinds its bits mark.
func (l *Ledger) nameKinds(ctx context.Context, fs []Firing, bits []int64) error {
	kinds, err := l.kinds(ctx, l.r)
	if err != nil {
		return err
	}
	for i := range fs {
		fs[i].Kinds = KindsOf(bits[i], kinds)
	}
	return nil
}

// FiringByID is one firing.
func (l *Ledger) FiringByID(ctx context.Context, id int64) (Firing, error) {
	var f Firing
	var sid, start, bits int64
	var end sql.NullInt64
	var labels, unrelated string
	err := l.r.QueryRowContext(ctx, `SELECT f.id, r.name, s.id, s.labels, f.start_at, f.end_at, f.white, f.refired, f.excused, f.deploy, f.marked, f.episode, f.person, f.unrelated
		FROM firings f JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id WHERE f.id = ?`, id).
		Scan(&f.ID, &f.Rule, &sid, &labels, &start, &end, &f.White, &f.Refired, &f.Excused, &f.Deploy, &bits, &f.Episode, &f.Person, &unrelated)
	f.Series, f.Unrelated = sid, unrelatedKeys(unrelated)
	if err == sql.ErrNoRows {
		return f, ErrNotFound
	}
	if err != nil {
		return f, err
	}
	if f.Labels, err = l.labels(sid, labels); err != nil {
		return f, err
	}
	f.Start, f.End = unix(start), fromNull(end)
	if bits != 0 {
		fs := []Firing{f}
		err = l.nameKinds(ctx, fs, []int64{bits})
		f = fs[0]
	}
	return f, err
}

// Report is the verdict on [from, to) as the ledger has it, in the form
// lapilli verdict --json writes: the firings that started in it and the
// incidents that did, each firing's stones given against every incident the
// ledger has. With firings false, the firings themselves are left out.
//
// A firing is judged by week: those of a week whose record is kept
// (WeekKept), and any that caught something; the rest are counted, not
// judged (verdict.RuleResult.Unjudged).
func (l *Ledger) Report(ctx context.Context, from, to time.Time, o verdict.Options, firings bool) (*verdict.Report, error) {
	return l.ReportFor(ctx, from, to, o, firings, nil)
}

// ReportFor is Report, with the record kept only by the incidents keeps
// says keep it, all when nil: a team's, by what it owns.
func (l *Ledger) ReportFor(ctx context.Context, from, to time.Time, o verdict.Options, firings bool, keeps func(Incident) bool) (*verdict.Report, error) {
	o.From, o.To = from, to
	at := verdict.NoisyAt{Firings: o.NoisyFirings, Precision: o.NoisyPrecision}
	if at.Firings == 0 {
		at.Firings = verdict.NoisyFirings
	}
	if at.Precision == 0 {
		at.Precision = verdict.NoisyPrecision
	}
	rep := &verdict.Report{From: from, To: to, NoisyAt: at, Rules: []verdict.RuleResult{}, Incidents: []verdict.IncidentResult{}, Firings: []verdict.Judged{}, SomeRules: o.SomeRules,
		People: true}
	table, err := l.RuleTable(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]*verdict.RuleResult{}
	result := func(r RuleRow) *verdict.RuleResult {
		if rr, ok := byName[r.Name]; ok {
			return rr
		}
		rr := &verdict.RuleResult{Rule: r.Name, Group: r.Group, Severity: r.Severity, Source: r.Source}
		byName[r.Name] = rr
		return rr
	}
	names := map[string]int64{}
	for id, r := range table {
		names[r.Name] = id
	}
	versions, err := l.Versions(ctx, from, to, false)
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		if !v.Since.IsZero() && v.Since.After(from) && v.Since.Before(to) {
			rep.RulesChanged = append(rep.RulesChanged, v.Since)
		}
		for _, n := range v.Names {
			r, ok := table[names[n]]
			if !ok {
				r = RuleRow{Rule: Rule{Name: n}}
			}
			rr := result(r)
			rr.Unsupported = r.Unsupported
			rr.Replayed = r.Unsupported == ""
		}
	}
	parts := l.parts(from.Unix(), to.Unix())
	kinds, err := l.kinds(ctx, l.r)
	if err != nil {
		return nil, err
	}
	marks := len(kinds) > 0
	// judgedOf says whether a rule's firings tied to signals (as the
	// ledger keeps them) were judged; one week's, or the span's when
	// every week of it was judged alike.
	count := func(parts []reportPart, judgedOf func(rule string, signals int64) bool) error {
		for _, p := range parts {
			if p.b <= p.a {
				continue
			}
			rows, err := l.r.QueryContext(ctx, p.q, p.a, p.b)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id, signals int64
				var n, white, again, excused, deploy, deployWhite, episodes, episodesWhite int
				var unrelated, unrelatedExcused, realWhite, realExcused, yes, no, yesBlack, noWhite int
				if err := rows.Scan(&id, &signals, &n, &white, &again, &excused, &deploy, &deployWhite, &episodes, &episodesWhite,
					&unrelated, &unrelatedExcused, &realWhite, &realExcused, &yes, &no, &yesBlack, &noWhite); err != nil {
					rows.Close()
					return err
				}
				rr := result(table[id])
				judged := judgedOf(rr.Rule, signals)
				// People's verdicts (docs/design/judging.md, item 3): of
				// the firings counted, those a person's word turned black,
				// and the answers to needed action.
				rr.Unrelated += unrelated - unrelatedExcused
				rr.ActionYes += yes
				rr.ActionNo += no
				if o.NotJudged(rr.Rule) {
					rr.NotJudged += n - white
				}
				rr.Firings += n
				rr.White += white
				rr.Black += n - white
				rr.Refired += again
				rr.Excused += excused
				rr.DuringDeploys += deploy
				rr.DeploysCaught += deployWhite
				// An episode in its first firing's row, judged by its
				// record; one with a white firing, judged whatever it is.
				rr.Episodes.Total += episodes
				rr.Episodes.White += episodesWhite
				if !judged {
					rr.Unjudged += n - white
					rr.DeploysUnjudged += deploy - deployWhite
					rep.Unjudged += n - white
					rr.Episodes.Unjudged += episodes - episodesWhite
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if !marks {
				continue
			}
			// And by kind of marked window, from the same days and hours.
			q := countMarkedDays
			if p.q == countFirings {
				q = countMarkedFirings
			}
			rows, err = l.r.QueryContext(ctx, q, p.a, p.b)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id, signals int64
				var kind string
				var n, white, again int
				if err := rows.Scan(&id, &kind, &signals, &n, &white, &again); err != nil {
					rows.Close()
					return err
				}
				rr := result(table[id])
				judged := judgedOf(rr.Rule, signals)
				if rr.Marked == nil {
					rr.Marked = map[string]verdict.MarkedCount{}
				}
				c := rr.Marked[kind]
				c.Firings += n
				c.Caught += white
				c.Refired += again
				if !judged {
					c.Unjudged += n - white
				}
				rr.Marked[kind] = c
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		return nil
	}
	rec, err := l.record(ctx, from, to, o, keeps)
	if err != nil {
		return nil, err
	}
	// Each week judged by its own record: the span whole when every week of
	// it is judged alike, or else a week at a time. With none kept, the span
	// has no record.
	weeks := l.spanWeeks(from, to)
	kept := map[int64]bool{}
	alike := true
	rep.NoRecord = true
	named := o.Signals.Named()
	for _, w := range weeks {
		kept[w] = l.weekKept(rec, w)
		alike = alike && kept[w] == kept[weeks[0]]
		rep.NoRecord = rep.NoRecord && !kept[w]
	}
	// With named signals, a week's firings are judged by the signals tied
	// to them (docs/design/signals.md, "The record, signal by signal"):
	// a week at a time, each rule's by its entry's, or else its alerts'.
	judgedIn := func(w int64) func(string, int64) bool {
		if !named {
			return func(rule string, _ int64) bool { return kept[w] && !o.NotJudged(rule) }
		}
		at := unix(l.midnight(w))
		return func(rule string, signals int64) bool {
			return !o.NotJudged(rule) && rec.JudgesTied(at, o.Signals.TiedOf(rule, signals))
		}
	}
	if named {
		rep.NoRecord = !rec.JudgesAny(unix(l.midnight(weeks[0])), unix(l.midnight(weeks[len(weeks)-1]+7)))
	}
	if alike && !named && !o.Signals.NotJudgedRules() {
		if err := count(parts, judgedIn(weeks[0])); err != nil {
			return nil, err
		}
	} else {
		for _, w := range weeks {
			a, b := max(from.Unix(), l.midnight(w)), min(to.Unix(), l.midnight(w+7))
			if err := count(l.parts(a, b), judgedIn(w)); err != nil {
				return nil, err
			}
		}
	}
	// The episodes, once the ledger has counted every week of the span
	// (EpisodesCounted): until then, none is given.
	counted, err := l.EpisodesCounted(ctx, from)
	if err != nil {
		return nil, err
	}
	rep.EpisodesNotCounted = !counted
	// The firings that came after an incident was over, their condition
	// having held in it, as lapilli verdict says it (LateOf): with o.Held.
	late, err := l.LateOf(ctx, from, to, o, firings)
	if err != nil {
		return nil, err
	}
	rep.LateCut = late.Cut
	total := 0
	for _, rr := range byName {
		rr.Late = late.Rules[rr.Rule]
		rr.NoRecord = rr.Firings > 0 && rr.Unjudged == rr.Firings
		rr.Noisy = verdict.IsNoisy(rr.Judged(), rr.White, at)
		rr.ImpactRule = o.Signals.EntryNumber(rr.Rule)
		if counted {
			rr.Episodes.Noisy = verdict.IsNoisy(rr.Episodes.Judged(), rr.Episodes.White, at)
		} else {
			rr.Episodes = verdict.EpisodeCount{}
		}
		rep.Rules = append(rep.Rules, *rr)
		total += rr.Firings
	}
	if named {
		// None of its firings judged, as the summary's rows say it.
		whole := rep.NoRecord
		rep.NoRecord = verdict.NoRecordOf(total, rep.Unjudged, func() bool { return whole })
	}
	if o.Hours != nil && to.Sub(from) <= HoursLongest {
		rep.Hours = o.Hours.Used()
		if err := l.countHours(ctx, rep.Rules, table, from, to, func(string) *verdict.Hours { return o.Hours }); err != nil {
			return nil, err
		}
	}
	SortRules(rep.Rules)

	incs, err := l.Incidents(ctx, from, to, false)
	if err != nil {
		return nil, err
	}
	for _, in := range incs {
		ir := verdict.IncidentResult{Incident: in.Incident, Real: in.Real, CaughtBy: in.CaughtBy, FiredAfter: late.Fired[in.ID]}
		for i, c := range ir.CaughtBy {
			if rr, ok := byName[c.Rule]; ok {
				ir.CaughtBy[i].Noisy = rr.Noisy
			}
		}
		for i, c := range ir.FiredAfter {
			if rr, ok := byName[c.Rule]; ok {
				ir.FiredAfter[i].Noisy = rr.Noisy
			}
		}
		rep.Incidents = append(rep.Incidents, ir)
	}
	// What can still fire is what the last version in the period held, and
	// every recorded rule: its source may send more.
	var inEffect func(string) bool
	if n := len(versions); n > 0 {
		names := versions[n-1].Names
		inEffect = func(rule string) bool {
			return slices.Contains(names, rule) || byName[rule] != nil && byName[rule].Source != ""
		}
	}
	rep.Credit(inEffect)
	if !firings {
		rep.Firings = nil
		return rep, nil
	}
	jd, err := l.judger(ctx, from, to, o, rec)
	if err != nil {
		return nil, err
	}
	fs, err := l.Firings(ctx, FiringQuery{From: from, To: to})
	if err != nil {
		return nil, err
	}
	slices.Reverse(fs)
	sort.SliceStable(fs, func(a, b int) bool {
		if !fs[a].Start.Equal(fs[b].Start) {
			return fs[a].Start.Before(fs[b].Start)
		}
		return fs[a].Rule < fs[b].Rule
	})
	jd.numbering(counted)
	jd.after = late.After
	for _, f := range fs {
		if f.Excused {
			continue // counted in the rule's Excused, not judged
		}
		rep.Firings = append(rep.Firings, jd.judge(f))
	}
	return rep, nil
}

// judger gives a report's firings their stones one at a time, as a report
// has them: each judged or not by the record, and the incidents near the
// span it caught.
type judger struct {
	l     *Ledger
	rec   verdict.Record
	o     verdict.Options
	named bool
	near  []incident.Incident
	// episodes numbers the episodes, from 1 in order of their first
	// firing as the firings are judged, by the id of their first firing;
	// nil when they aren't counted (numbering).
	episodes map[int64]int
	// after are, by firing id, the incidents each came after the end of
	// (Late.After).
	after map[int64][]string
}

// numbering has the episodes of the firings judged numbered, when they are
// counted.
func (jd *judger) numbering(counted bool) {
	jd.episodes = nil
	if counted {
		jd.episodes = map[int64]int{}
	}
}

// number is the number of the episode whose first firing has the id
// first, in the order the firings are judged (or passed over, number
// called on them alike): 0 when episodes aren't counted, or for none.
func (jd *judger) number(first int64) int {
	if jd.episodes == nil || first == 0 {
		return 0
	}
	n, ok := jd.episodes[first]
	if !ok {
		n = len(jd.episodes) + 1
		jd.episodes[first] = n
	}
	return n
}

func (l *Ledger) judger(ctx context.Context, from, to time.Time, o verdict.Options, rec verdict.Record) (*judger, error) {
	// Which incidents each firing caught: those near the period.
	cs, err := l.counting(ctx, l.r, from.Unix(), to.Unix(), o)
	if err != nil {
		return nil, err
	}
	jd := &judger{l: l, rec: rec, o: o, named: o.Signals.Named(), near: make([]incident.Incident, len(cs))}
	for i, c := range cs {
		jd.near[i] = c.Incident
	}
	return jd, nil
}

func (jd *judger) judge(f Firing) verdict.Judged {
	judged := jd.l.FiringJudged(jd.rec, jd.o, f.Rule, f.Start, f.Signals)
	var signals []string
	if jd.named {
		signals = jd.o.Signals.Names(jd.o.Signals.TiedOf(f.Rule, f.Signals))
	}
	j := verdict.Judged{Firing: f.Firing, White: f.White, NoRecord: !f.White && !judged,
		Refired: f.Refired, DuringDeploy: f.Deploy, Marked: f.Kinds, Signals: signals,
		Day: f.Start.In(jd.l.loc).Format(time.DateOnly), Action: f.Action(), NotJudged: jd.o.NotJudged(f.Rule)}
	j.Episode = jd.number(f.Episode)
	// From when its condition held, and the incidents it came after, as
	// lapilli verdict says them.
	j.Held = verdict.HeldFrom(f.Firing, jd.o.Held)
	j.After = jd.after[f.ID]
	for _, in := range jd.near {
		if verdict.Catches(f.Firing, in, jd.o) {
			// Said by a person to be unrelated to it: not caught.
			if slices.Contains(f.Unrelated, in.ID) {
				j.Unrelated = append(j.Unrelated, in.ID)
				continue
			}
			j.Incidents = append(j.Incidents, in.ID)
		}
	}
	return j
}

// ReportFirings are the firings ReportFor gives with firings, judged alike
// and in the same order (of their start, then of their rule), of the rules
// in says, every rule's when nil: the first most of them, and how many
// there are. Only those most are read whole, so a span of millions costs
// their count beside them, not their labels.
func (l *Ledger) ReportFirings(ctx context.Context, from, to time.Time, o verdict.Options, keeps func(Incident) bool,
	in func(string) bool, most int) ([]verdict.Judged, int, error) {
	o.From, o.To = from, to
	if in == nil {
		in = func(string) bool { return true }
	}
	most = max(most, 0)
	table, err := l.RuleTable(ctx)
	if err != nil {
		return nil, 0, err
	}
	rec, err := l.record(ctx, from, to, o, keeps)
	if err != nil {
		return nil, 0, err
	}
	jd, err := l.judger(ctx, from, to, o, rec)
	if err != nil {
		return nil, 0, err
	}
	kinds, err := l.kinds(ctx, l.r)
	if err != nil {
		return nil, 0, err
	}
	// In order of their start, then of their id, as Firings gives them
	// reversed; each start's put in order of their rule once all are read,
	// as ReportFor's are, before any is cut.
	counted, err := l.EpisodesCounted(ctx, from)
	if err != nil {
		return nil, 0, err
	}
	jd.numbering(counted)
	// The incidents each came after the end of, as ReportFor says them.
	late, err := l.LateOf(ctx, from, to, o, true)
	if err != nil {
		return nil, 0, err
	}
	jd.after = late.After
	rows, err := l.r.QueryContext(ctx, `SELECT f.id, f.rule_id, s.id, s.labels, f.start_at, f.end_at, f.white, f.refired, f.deploy, f.marked, s.signals, f.episode, f.person, f.unrelated
		FROM firings f JOIN series s ON s.id = f.series_id WHERE f.start_at >= ? AND f.start_at < ? AND f.excused = 0
		ORDER BY f.start_at, f.id`, from.Unix(), to.Unix())
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	type row struct {
		f                Firing
		sid, bits, start int64
		labels           string
		end              sql.NullInt64
		// shown is of a rule in says: the others are read only so that
		// the episodes are numbered as the whole report numbers them.
		shown bool
	}
	out := []verdict.Judged{}
	var start []row
	more := false
	flush := func() error {
		slices.SortStableFunc(start, func(a, b row) int { return strings.Compare(a.f.Rule, b.f.Rule) })
		for _, r := range start {
			if !r.shown {
				jd.number(r.f.Episode)
				continue
			}
			if len(out) == most {
				more = true
				break
			}
			var err error
			if r.f.Labels, err = l.labels(r.sid, r.labels); err != nil {
				return err
			}
			r.f.Start, r.f.End, r.f.Kinds = unix(r.start), fromNull(r.end), KindsOf(r.bits, kinds)
			out = append(out, jd.judge(r.f))
		}
		start = start[:0]
		return nil
	}
	for rows.Next() {
		var r row
		var rule int64
		var unrelated string
		if err := rows.Scan(&r.f.ID, &rule, &r.sid, &r.labels, &r.start, &r.end, &r.f.White, &r.f.Refired, &r.f.Deploy, &r.bits, &r.f.Signals, &r.f.Episode,
			&r.f.Person, &unrelated); err != nil {
			return nil, 0, err
		}
		r.f.Series, r.f.Unrelated = r.sid, unrelatedKeys(unrelated)
		r.f.Rule = table[rule].Name
		r.shown = in(r.f.Rule)
		if len(start) > 0 && r.start != start[0].start {
			if err := flush(); err != nil {
				return nil, 0, err
			}
			if more {
				break
			}
		}
		if len(out) == most && len(start) == 0 {
			// Every one given: past them is this one, if it is shown;
			// another rule's is passed over.
			if r.shown {
				more = true
				break
			}
			continue
		}
		start = append(start, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if !more {
		if err := flush(); err != nil {
			return nil, 0, err
		}
	}
	rows.Close()
	if !more {
		return out, len(out), nil
	}
	n := 0
	err = l.eachRow(ctx, func(sc scanner) error {
		var rule int64
		var c int
		if err := sc.Scan(&rule, &c); err != nil {
			return err
		}
		if in(table[rule].Name) {
			n += c
		}
		return nil
	}, "SELECT rule_id, count(*) FROM firings WHERE start_at >= ? AND start_at < ? AND excused = 0 GROUP BY rule_id", from.Unix(), to.Unix())
	// Counted after: one written since is counted, one taken away isn't.
	return out, max(n, len(out)+1), err
}

// reportPart is a query of a report's counts over [a, b).
type reportPart struct {
	q    string
	a, b int64
}

const (
	// By the signals the alerts are tied to, too.
	countFirings = `SELECT f.rule_id, s.signals, count(*) - sum(f.excused), sum(f.white), sum(f.refired * (1 - f.excused)), sum(f.excused), sum(f.deploy), sum(f.deploy * f.white),
		` + firstFirings + `, sum(f.episode_white), ` + personSums + `
		FROM firings f JOIN series s ON s.id = f.series_id WHERE f.start_at >= ? AND f.start_at < ? GROUP BY f.rule_id, s.signals`
	countDays = `SELECT rule_id, signals, sum(firings), sum(white), sum(refired), sum(excused), sum(deploy), sum(deploy_white), sum(episodes), sum(episodes_white),
		sum(unrelated), sum(unrelated_excused), sum(real_white), sum(real_excused), sum(action_yes), sum(action_no), sum(action_yes_black), sum(action_no_white)
		FROM days WHERE day >= ? AND day < ? GROUP BY rule_id, signals`
	// By kind of marked window, from the firings and from the days.
	countMarkedFirings = `SELECT f.rule_id, k.name, s.signals, count(*), sum(f.white), sum(f.refired) FROM firings f JOIN marked_kinds k ON ` + markedOn + `
		JOIN series s ON s.id = f.series_id WHERE f.start_at >= ? AND f.start_at < ? AND f.marked != 0 GROUP BY f.rule_id, k.name, s.signals`
	countMarkedDays = `SELECT d.rule_id, k.name, d.signals, sum(d.firings), sum(d.white), sum(d.refired) FROM marked_days d JOIN marked_kinds k ON k.id = d.kind_id
		WHERE d.day >= ? AND d.day < ? GROUP BY d.rule_id, k.name, d.signals`
)

// parts are the queries of a report's counts over [lo, hi): whole days from
// the daily tallies; the hours at either end, from the firings.
func (l *Ledger) parts(lo, hi int64) []reportPart {
	d1, d2 := l.day(lo), l.day(hi)
	if l.midnight(d1) < lo {
		d1++
	}
	if d1 >= d2 {
		return []reportPart{{countFirings, lo, hi}}
	}
	return []reportPart{{countFirings, lo, l.midnight(d1)}, {countDays, d1, d2}, {countFirings, l.midnight(d2), hi}}
}

// recordOn are the incidents firings are judged against (written down, or
// impact windows that count) on at some time in [?1, ?2): started before
// the end, and ended at the start or after it; one with no end, taken to
// last as long as a firing catches it for, started at ?3 (the start less
// that) or after. Read by the index of when they ended (schema 018), so
// only those near are: the start's bound has a + before it, so that SQLite
// doesn't read every incident before the end by the index of starts.
const recordOn = `i.counts = 1 AND +i.start_at < ?2 AND (i.end_at >= ?1 OR i.end_at IS NULL AND i.start_at >= ?3)`

// RecordIncidents are the incidents on at some time in [from, to) (recordOn),
// one with no end taken to last unknown, as a firing catches it; each with
// what caught it, for who owns it.
func (l *Ledger) RecordIncidents(ctx context.Context, from, to time.Time, unknown time.Duration) ([]Incident, error) {
	return l.incidents(ctx, `SELECT `+incidentCols+`
		FROM incidents i WHERE `+recordOn+` ORDER BY i.start_at, i.key`, recordArgs(from, to, unknown)...)
}

func recordArgs(from, to time.Time, unknown time.Duration) []any {
	return []any{from.Unix(), to.Unix(), from.Unix() - int64(max(unknown, 0)/time.Second)}
}

// RecordOf is what keeps the record of [from, to)'s weeks, and of any time
// in them: the incidents on from two weeks before the first to the end of
// the last, of those keeps says keep it (all, when nil), and the impact
// signal read from o.ImpactFrom.
func (l *Ledger) RecordOf(ctx context.Context, from, to time.Time, o verdict.Options, keeps func(Incident) bool) (verdict.Record, error) {
	return l.record(ctx, from, to, o, keeps)
}

func (l *Ledger) record(ctx context.Context, from, to time.Time, o verdict.Options, keeps func(Incident) bool) (verdict.Record, error) {
	wf, wt := l.weeksOf(from, to)
	incs, err := l.RecordIncidents(ctx, verdict.RecordFrom(wf, wt), wt, o.UnknownLen)
	if err != nil {
		return verdict.Record{}, err
	}
	var plain []incident.Incident
	for _, in := range incs {
		if keeps == nil || keeps(in) {
			plain = append(plain, in.Incident)
		}
	}
	return verdict.RecordOf(plain, o), nil
}

// weeksOf is from the Monday of the first week [from, to) has some of to
// the end of the last.
func (l *Ledger) weeksOf(from, to time.Time) (time.Time, time.Time) {
	ws := l.spanWeeks(from, to)
	return unix(l.midnight(ws[0])), unix(l.midnight(ws[len(ws)-1] + 7))
}

// WeekOf is the week t is in, from its Monday to the next, as the ledger
// keeps weeks: what lapilli serve judges the record by
// (verdict.Options.RecordWeek).
func (l *Ledger) WeekOf(t time.Time) (time.Time, time.Time) {
	w := Week(l.day(t.Unix()))
	return unix(l.midnight(w)), unix(l.midnight(w + 7))
}

// RecordKept reports whether the record of one of [from, to)'s weeks is
// kept (WeekKept): the impact signal read over all of one, or an incident
// on from two weeks before the first to the end of the last, a window of
// a signal not in effect none of them (verdict.Options.NotInEffect).
//
// With named signals, a week's firings are judged by the signals tied to
// them: one of the weeks is kept when any firing of it would be judged
// (verdict.Record.JudgesAny), a window of a signal or a signal read by
// its Monday among what keeps it.
func (l *Ledger) RecordKept(ctx context.Context, from, to time.Time, o verdict.Options) (bool, error) {
	if o.Signals.Named() {
		rec, err := l.record(ctx, from, to, o, nil)
		if err != nil {
			return false, err
		}
		return slices.ContainsFunc(l.spanWeeks(from, to), func(w int64) bool { return l.weekKept(rec, w) }), nil
	}
	wf, wt := l.weeksOf(from, to)
	if last, _ := l.WeekOf(wt.Add(-time.Second)); !o.ImpactFrom.IsZero() && !o.ImpactFrom.After(last) {
		return true, nil
	}
	// With no named signal in effect, a named signal's window is one of a
	// signal not in effect: it keeps no record.
	var n int
	err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM incidents i WHERE "+recordOn+" AND (i.origin <> 'impact' OR i.signal = '') LIMIT 1) x",
		recordArgs(verdict.RecordFrom(wf, wt), wt, o.UnknownLen)...).Scan(&n)
	return n > 0, err
}

// FiringJudged reports whether a firing of rule that started at start, its
// alert tied to signals (as the ledger keeps them, Firing.Signals), is
// judged by rec should it catch nothing: by its week's record, or, with
// named signals, by the signals tied to it (docs/design/signals.md). One
// that caught something is judged whatever (verdict.RuleResult.Judged).
// Report says each firing so, and a page that lists or draws firings asks
// the same, so that none of them calls black what the report doesn't
// judge.
func (l *Ledger) FiringJudged(rec verdict.Record, o verdict.Options, rule string, start time.Time, signals int64) bool {
	// A rule an entry says is judged by nothing (judged: false).
	if o.NotJudged(rule) {
		return false
	}
	w := Week(l.day(start.Unix()))
	if o.Signals.Named() {
		return rec.JudgesTied(unix(l.midnight(w)), o.Signals.TiedOf(rule, signals))
	}
	return l.weekKept(rec, w)
}

// WeekKept reports whether a week's record is kept, by rec: the granularity
// lapilli serve judges firings by. w is its Monday.
func (l *Ledger) WeekKept(rec verdict.Record, w int64) bool { return l.weekKept(rec, w) }

func (l *Ledger) weekKept(rec verdict.Record, w int64) bool {
	if rec.Named() {
		return rec.JudgesAny(unix(l.midnight(w)), unix(l.midnight(w+7)))
	}
	return rec.Kept(unix(l.midnight(w)), unix(l.midnight(w+7)))
}

// SpanWeeks are the Mondays of the weeks [from, to) has some of: one at
// least.
func (l *Ledger) SpanWeeks(from, to time.Time) []int64 { return l.spanWeeks(from, to) }

func (l *Ledger) spanWeeks(from, to time.Time) []int64 {
	var out []int64
	for w := Week(l.day(from.Unix())); l.midnight(w) < to.Unix(); w += 7 {
		out = append(out, w)
	}
	if len(out) == 0 {
		out = append(out, Week(l.day(from.Unix())))
	}
	return out
}

// ImpactSince is from when the impact signal (--impact) was read, all of
// it, by the runs; zero when it never was.
func (l *Ledger) ImpactSince(ctx context.Context) (time.Time, error) {
	v, err := sigMeta(ctx, l.r, "", metaImpactSince)
	if err != nil || v == "" || v == impactNone {
		return time.Time{}, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return unix(n), err
}

// SortRules puts the noisiest rules first, as verdict.Judge does.
func SortRules(rs []verdict.RuleResult) {
	sort.Slice(rs, func(a, b int) bool {
		x, y := rs[a], rs[b]
		if x.Black != y.Black {
			return x.Black > y.Black
		}
		if x.Firings != y.Firings {
			return x.Firings > y.Firings
		}
		return x.Rule < y.Rule
	})
}

// Bounds is when the earliest and latest firings the ledger has started;
// zero when it has none.
func (l *Ledger) Bounds(ctx context.Context) (time.Time, time.Time, error) {
	// Apart, each is one step down the index; together, a scan of it.
	var lo, hi sql.NullInt64
	if err := l.r.QueryRowContext(ctx, "SELECT min(start_at) FROM firings").Scan(&lo); err != nil {
		return time.Time{}, time.Time{}, err
	}
	err := l.r.QueryRowContext(ctx, "SELECT max(start_at) FROM firings").Scan(&hi)
	return fromNull(lo), fromNull(hi), err
}

// CaughtBy are the latest incidents a rule caught, the latest first.
func (l *Ledger) CaughtBy(ctx context.Context, rule string, limit int) ([]Incident, error) {
	return l.incidents(ctx, `SELECT `+incidentCols+`
		FROM incidents i JOIN catches c ON c.incident_id = i.id JOIN rules r ON r.id = c.rule_id
		WHERE r.name = ? ORDER BY i.start_at DESC, i.key LIMIT ?`, rule, limit)
}

// HoursLongest is the longest period a report counts firings by the hours
// they came in: each is read again, not taken from the daily tallies.
const HoursLongest = 31 * 24 * time.Hour

// HourTally is how many of a rule's firings came off hours, and while
// people slept, and how many of each caught nothing.
type HourTally struct{ OffHours, OffHoursBlack, Asleep, AsleepBlack int }

// HoursKey tells hours apart: two with the same key count every firing
// alike.
func HoursKey(h *verdict.Hours) string {
	u := h.Used()
	if h.Shifts != nil {
		// An on-call schedule's turns as last read, too.
		u.Shifts = h.Shifts.Key()
	}
	return u.Zone + " " + u.Work + " " + u.Sleep + " " + u.Shifts
}

// CountHours counts, for each rule, when its firings in [from, to) came,
// excused ones aside, by the hours of says it goes by: nil for none. It
// reads the firings once for each of the hours, however many rules go by
// them.
func (l *Ledger) CountHours(ctx context.Context, rules []verdict.RuleResult, from, to time.Time, of func(rule string) *verdict.Hours) error {
	table, err := l.RuleTable(ctx)
	if err != nil {
		return err
	}
	return l.countHours(ctx, rules, table, from, to, of)
}

func (l *Ledger) countHours(ctx context.Context, rules []verdict.RuleResult, table map[int64]RuleRow, from, to time.Time, of func(rule string) *verdict.Hours) (err error) {
	byName := make(map[string]*verdict.RuleResult, len(rules))
	hours := map[string]*verdict.Hours{}
	keys := make(map[string]string, len(rules)) // rule to its hours' key
	for i := range rules {
		rr := &rules[i]
		rr.OffHours, rr.OffHoursBlack, rr.Asleep, rr.AsleepBlack = 0, 0, 0, 0
		byName[rr.Rule] = rr
		if h := of(rr.Rule); h != nil {
			k := HoursKey(h)
			hours[k], keys[rr.Rule] = h, k
		}
	}
	// The hours most rules go by are counted a stretch of hours at a time,
	// for every rule; the others by reading their own rules' firings, so
	// that many teams' hours cost no more than reading the firings once.
	most := ""
	n := map[string]int{}
	for _, k := range keys {
		if n[k]++; most == "" || n[k] > n[most] || n[k] == n[most] && k < most {
			most = k
		}
	}
	ids := map[string][]int64{}
	for id, r := range table {
		if k, ok := keys[r.Name]; ok && k != most {
			ids[k] = append(ids[k], id)
		}
	}
	for k, h := range hours {
		var by map[int64]HourTally
		if k == most {
			by, err = l.RuleHours(ctx, from, to, h)
		} else {
			var all map[int64]map[int64]HourTally
			all, err = l.hoursOf(ctx, from, to, h, ids[k], false, func(int64) int64 { return 0 })
			by = all[0]
		}
		if err != nil {
			return err
		}
		// Rules of one name are one rule, whatever their groups.
		for id, x := range by {
			name := table[id].Name
			if rr := byName[name]; rr != nil && keys[name] == k {
				rr.OffHours += x.OffHours
				rr.OffHoursBlack += x.OffHoursBlack
				rr.Asleep += x.Asleep
				rr.AsleepBlack += x.AsleepBlack
			}
		}
	}
	return nil
}

// ByHours tallies, for each week from the Monday from up to the Monday to,
// and each rule, when its firings came, excused ones aside.
func (l *Ledger) ByHours(ctx context.Context, from, to int64, h *verdict.Hours) (map[int64]map[int64]HourTally, error) {
	return l.byHours(ctx, unix(l.midnight(from)), unix(l.midnight(to)), h, func(at int64) int64 { return Week(l.day(at)) })
}

// ByHoursOf is ByHours for the rules ids only, reading their firings: for a
// few rules, quicker than a stretch of hours at a time for every rule.
func (l *Ledger) ByHoursOf(ctx context.Context, from, to int64, h *verdict.Hours, ids []int64) (map[int64]map[int64]HourTally, error) {
	return l.hoursOf(ctx, unix(l.midnight(from)), unix(l.midnight(to)), h, ids, false, func(at int64) int64 { return Week(l.day(at)) })
}

// hoursOf reads the firings of the rules ids in [from, to), along the index
// on rule and start, or, all, of every rule, and tallies when each came.
//
// In Postgres each rule is searched for apart (LATERAL, OFFSET 0 so it stays
// so): asked for a few rules' firings of a week at once, Postgres rates
// reading the whole week by firings_start, firings coming in time order,
// about as cheap as a search a rule, and often reads it, fifty times the
// rows; for fifty teams' hours, a query a team and a week, 3.4 s on CI's
// Postgres 18 where 0.15 s would do.
func (l *Ledger) hoursOf(ctx context.Context, from, to time.Time, h *verdict.Hours, ids []int64, all bool, key func(at int64) int64) (map[int64]map[int64]HourTally, error) {
	out := map[int64]map[int64]HourTally{}
	for all || len(ids) > 0 {
		q := `SELECT rule_id, start_at, white FROM firings WHERE start_at >= ? AND start_at < ? AND excused = 0`
		args := []any{from.Unix(), to.Unix()}
		if !all {
			part := ids[:min(len(ids), 500)]
			ids = ids[len(part):]
			if l.d == postgres {
				b, err := json.Marshal(part)
				if err != nil {
					return nil, err
				}
				q = `SELECT f.rule_id, f.start_at, f.white FROM (` + l.d.ints("?3") + `) AS r (id),
					LATERAL (SELECT rule_id, start_at, white FROM firings WHERE rule_id = r.id AND start_at >= ?1 AND start_at < ?2 AND excused = 0 OFFSET 0) AS f`
				args = append(args, string(b))
			} else {
				for _, id := range part {
					args = append(args, id)
				}
				q += ` AND rule_id IN (?` + strings.Repeat(", ?", len(part)-1) + `)`
			}
		}
		all = false
		rows, err := l.r.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var rule, at int64
			var white bool
			if err := rows.Scan(&rule, &at, &white); err != nil {
				rows.Close()
				return nil, err
			}
			black := 1
			if white {
				black = 0
			}
			k := key(at)
			if out[k] == nil {
				out[k] = map[int64]HourTally{}
			}
			x := out[k][rule]
			switch h.Of(unix(at)) {
			case verdict.Asleep:
				x.Asleep, x.AsleepBlack = x.Asleep+1, x.AsleepBlack+black
			case verdict.OffHours:
				x.OffHours, x.OffHoursBlack = x.OffHours+1, x.OffHoursBlack+black
			default:
				continue
			}
			out[k][rule] = x
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RuleHours tallies, for each rule, when its firings in [from, to) came,
// excused ones aside.
func (l *Ledger) RuleHours(ctx context.Context, from, to time.Time, h *verdict.Hours) (map[int64]HourTally, error) {
	by, err := l.byHours(ctx, from, to, h, func(int64) int64 { return 0 })
	return by[0], err
}

func (l *Ledger) byHours(ctx context.Context, from, to time.Time, h *verdict.Hours, key func(at int64) int64) (map[int64]map[int64]HourTally, error) {
	out := map[int64]map[int64]HourTally{}
	spans := h.Spans(from, to)
	if days := int(to.Sub(from)/(24*time.Hour)) + 1; len(spans) > 6*days {
		// Hours that change often, as short shifts round several zones do:
		// one read of the firings, row by row, is quicker.
		return l.hoursOf(ctx, from, to, h, nil, true, key)
	}
	// A query a stretch of hours, along the index on when firings started:
	// SQLite counting them all at once, grouped, takes far longer.
	for _, sp := range spans {
		k := key(sp.From.Unix())
		rows, err := l.r.QueryContext(ctx, `SELECT rule_id, count(*), sum(white) FROM firings
			WHERE start_at >= ? AND start_at < ? AND excused = 0 GROUP BY rule_id`, sp.From.Unix(), sp.To.Unix())
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var rule int64
			var n, white int
			if err := rows.Scan(&rule, &n, &white); err != nil {
				rows.Close()
				return nil, err
			}
			if out[k] == nil {
				out[k] = map[int64]HourTally{}
			}
			x := out[k][rule]
			if sp.Of == verdict.Asleep {
				x.Asleep, x.AsleepBlack = x.Asleep+n, x.AsleepBlack+n-white
			} else {
				x.OffHours, x.OffHoursBlack = x.OffHours+n, x.OffHoursBlack+n-white
			}
			out[k][rule] = x
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"sort"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// RuleRow is a rule and its id.
type RuleRow struct {
	ID int64
	Rule
}

// RuleTable is every rule the ledger has, by id.
func (l *Ledger) RuleTable(ctx context.Context) (map[int64]RuleRow, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT id, name, grp, severity, unsupported FROM rules")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]RuleRow{}
	for rows.Next() {
		var r RuleRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Group, &r.Severity, &r.Unsupported); err != nil {
			return nil, err
		}
		out[r.ID] = r
	}
	return out, rows.Err()
}

// Tally is a rule's stones over a day or a week.
type Tally struct {
	At      int64 // the day, or the week's Monday
	Rule    int64
	Firings int
	White   int
	Refired int
}

// Black is how many firings caught nothing.
func (t Tally) Black() int { return t.Firings - t.White }

// Days are each rule's stones by day, from day to day.
func (l *Ledger) Days(ctx context.Context, from, to int64) ([]Tally, error) {
	return l.tallies(ctx, "SELECT day, rule_id, firings, white, refired FROM days WHERE day >= ? AND day <= ? ORDER BY day, rule_id", from, to)
}

// Weeks are each rule's stones by week, from the week starting on one
// Monday to that starting on another.
func (l *Ledger) Weeks(ctx context.Context, from, to int64) ([]Tally, error) {
	return l.tallies(ctx, "SELECT week, rule_id, firings, white, refired FROM weeks WHERE week >= ? AND week <= ? ORDER BY week, rule_id", from, to)
}

func (l *Ledger) tallies(ctx context.Context, q string, from, to int64) ([]Tally, error) {
	rows, err := l.r.QueryContext(ctx, q, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tally
	for rows.Next() {
		var t Tally
		if err := rows.Scan(&t.At, &t.Rule, &t.Firings, &t.White, &t.Refired); err != nil {
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
	q := `SELECT i.id, i.key, i.origin, i.title, i.start_at, i.end_at, i.labels, i.state, i.counts, i.updated, i.updated_by
		FROM incidents i WHERE i.start_at >= ? AND i.start_at < ?`
	if !all {
		q += " AND i.counts = 1"
	}
	return l.incidents(ctx, q+" ORDER BY i.start_at, i.key", from.Unix(), to.Unix())
}

// IncidentByID is the incident with that id.
func (l *Ledger) IncidentByID(ctx context.Context, key string) (Incident, error) {
	out, err := l.incidents(ctx, `SELECT i.id, i.key, i.origin, i.title, i.start_at, i.end_at, i.labels, i.state, i.counts, i.updated, i.updated_by
		FROM incidents i WHERE i.key = ?`, key)
	if err != nil {
		return Incident{}, err
	}
	if len(out) == 0 {
		return Incident{}, ErrNotFound
	}
	return out[0], nil
}

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
		var labels string
		if err := rows.Scan(&id, &in.ID, &in.Origin, &in.Title, &start, &end, &labels, &in.State, &in.Counts, &updated, &in.By); err != nil {
			rows.Close()
			return nil, err
		}
		in.Start, in.End, in.Updated = unix(start), fromNull(end), unix(updated)
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
	rows, err = l.r.QueryContext(ctx, `SELECT c.incident_id, r.name, c.delay FROM catches c JOIN rules r ON r.id = c.rule_id
		WHERE c.incident_id IN (SELECT value FROM json_each(?))`, string(idsJSON))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, delay int64
		var name string
		if err := rows.Scan(&id, &name, &delay); err != nil {
			return nil, err
		}
		in := &out[index[id]]
		in.CaughtBy = append(in.CaughtBy, verdict.Catch{Rule: name, Delay: time.Duration(delay) * time.Second})
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
	firing.Firing
	White, Refired bool
}

// FiringQuery picks firings: those that started in [From, To), of one rule
// when Rule is given, only black stones with Black; the latest first, Limit
// at a time, those before Before (an ID from the last page) when given.
type FiringQuery struct {
	From, To time.Time
	Rule     string
	Black    bool
	Before   int64
	Limit    int
}

// Firings are the firings a query picks.
func (l *Ledger) Firings(ctx context.Context, q FiringQuery) ([]Firing, error) {
	sq := `SELECT f.id, r.name, s.id, s.labels, f.start_at, f.end_at, f.white, f.refired
		FROM firings f JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id
		WHERE f.start_at >= ? AND f.start_at < ?`
	args := []any{q.From.Unix(), q.To.Unix()}
	if q.Rule != "" {
		sq += " AND f.rule_id = (SELECT id FROM rules WHERE name = ?)"
		args = append(args, q.Rule)
	}
	if q.Black {
		sq += " AND f.white = 0"
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
	for rows.Next() {
		var f Firing
		var sid, start int64
		var end sql.NullInt64
		var labels string
		if err := rows.Scan(&f.ID, &f.Rule, &sid, &labels, &start, &end, &f.White, &f.Refired); err != nil {
			return nil, err
		}
		if f.Labels, err = l.labels(sid, labels); err != nil {
			return nil, err
		}
		f.Start, f.End = unix(start), fromNull(end)
		out = append(out, f)
	}
	return out, rows.Err()
}

// FiringByID is one firing.
func (l *Ledger) FiringByID(ctx context.Context, id int64) (Firing, error) {
	var f Firing
	var sid, start int64
	var end sql.NullInt64
	var labels string
	err := l.r.QueryRowContext(ctx, `SELECT f.id, r.name, s.id, s.labels, f.start_at, f.end_at, f.white, f.refired
		FROM firings f JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id WHERE f.id = ?`, id).
		Scan(&f.ID, &f.Rule, &sid, &labels, &start, &end, &f.White, &f.Refired)
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
	return f, nil
}

// Report is the verdict on [from, to) as the ledger has it, in the form
// lapilli verdict --json writes: the firings that started in it and the
// incidents that did, each firing's stones given against every incident the
// ledger has. With firings false, the firings themselves are left out.
func (l *Ledger) Report(ctx context.Context, from, to time.Time, o verdict.Options, firings bool) (*verdict.Report, error) {
	o.From, o.To = from, to
	at := verdict.NoisyAt{Firings: o.NoisyFirings, Precision: o.NoisyPrecision}
	if at.Firings == 0 {
		at.Firings = verdict.NoisyFirings
	}
	if at.Precision == 0 {
		at.Precision = verdict.NoisyPrecision
	}
	rep := &verdict.Report{From: from, To: to, NoisyAt: at, Rules: []verdict.RuleResult{}, Incidents: []verdict.IncidentResult{}, Firings: []verdict.Judged{}}
	table, err := l.RuleTable(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]*verdict.RuleResult{}
	result := func(r RuleRow) *verdict.RuleResult {
		if rr, ok := byName[r.Name]; ok {
			return rr
		}
		rr := &verdict.RuleResult{Rule: r.Name, Group: r.Group, Severity: r.Severity}
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
	// Whole days come from the daily tallies; the hours at either end, from
	// the firings.
	d1, d2 := l.day(from.Unix()), l.day(to.Unix())
	if l.midnight(d1) < from.Unix() {
		d1++
	}
	type part struct {
		q    string
		a, b int64
	}
	byFirings := "SELECT rule_id, count(*), sum(white), sum(refired) FROM firings WHERE start_at >= ? AND start_at < ? GROUP BY rule_id"
	parts := []part{{byFirings, from.Unix(), to.Unix()}}
	if d1 < d2 {
		parts = []part{
			{byFirings, from.Unix(), l.midnight(d1)},
			{"SELECT rule_id, sum(firings), sum(white), sum(refired) FROM days WHERE day >= ? AND day < ? GROUP BY rule_id", d1, d2},
			{byFirings, l.midnight(d2), to.Unix()},
		}
	}
	for _, p := range parts {
		if p.b <= p.a {
			continue
		}
		rows, err := l.r.QueryContext(ctx, p.q, p.a, p.b)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var n, white, again int
			if err := rows.Scan(&id, &n, &white, &again); err != nil {
				rows.Close()
				return nil, err
			}
			rr := result(table[id])
			rr.Firings += n
			rr.White += white
			rr.Black += n - white
			rr.Refired += again
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	for _, rr := range byName {
		p, ok := rr.Precision()
		rr.Noisy = ok && rr.Firings >= at.Firings && p < at.Precision
		rep.Rules = append(rep.Rules, *rr)
	}
	SortRules(rep.Rules)

	incs, err := l.Incidents(ctx, from, to, false)
	if err != nil {
		return nil, err
	}
	for _, in := range incs {
		ir := verdict.IncidentResult{Incident: in.Incident, CaughtBy: in.CaughtBy}
		for i, c := range ir.CaughtBy {
			if rr, ok := byName[c.Rule]; ok {
				ir.CaughtBy[i].Noisy = rr.Noisy
			}
		}
		rep.Incidents = append(rep.Incidents, ir)
	}
	if !firings {
		rep.Firings = nil
		return rep, nil
	}
	// Which incidents each firing caught: those near the period.
	cs, err := l.counting(ctx, l.r, from.Unix(), to.Unix(), o)
	if err != nil {
		return nil, err
	}
	near := make([]incident.Incident, len(cs))
	for i, c := range cs {
		near[i] = c.Incident
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
	for _, f := range fs {
		j := verdict.Judged{Firing: f.Firing, White: f.White}
		for _, in := range near {
			if verdict.Catches(f.Firing, in, o) {
				j.Incidents = append(j.Incidents, in.ID)
			}
		}
		rep.Firings = append(rep.Firings, j)
	}
	return rep, nil
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
	return l.incidents(ctx, `SELECT i.id, i.key, i.origin, i.title, i.start_at, i.end_at, i.labels, i.state, i.counts, i.updated, i.updated_by
		FROM incidents i JOIN catches c ON c.incident_id = i.id JOIN rules r ON r.id = c.rule_id
		WHERE r.name = ? ORDER BY i.start_at DESC, i.key LIMIT ?`, rule, limit)
}

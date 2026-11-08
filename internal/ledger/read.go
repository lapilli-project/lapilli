package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"sort"
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
	At   int64 // the day, or the week's Monday
	Rule int64
	// Firings are those judged: not those a quiet window excused.
	Firings int
	White   int
	Refired int
	Excused int
}

// Black is how many firings caught nothing.
func (t Tally) Black() int { return t.Firings - t.White }

// Days are each rule's stones by day, from day to day.
func (l *Ledger) Days(ctx context.Context, from, to int64) ([]Tally, error) {
	return l.tallies(ctx, "SELECT day, rule_id, firings, white, refired, excused FROM days WHERE day >= ? AND day <= ? ORDER BY day, rule_id", from, to)
}

// Weeks are each rule's stones by week, from the week starting on one
// Monday to that starting on another.
func (l *Ledger) Weeks(ctx context.Context, from, to int64) ([]Tally, error) {
	return l.tallies(ctx, "SELECT week, rule_id, firings, white, refired, excused FROM weeks WHERE week >= ? AND week <= ? ORDER BY week, rule_id", from, to)
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
		if err := rows.Scan(&t.At, &t.Rule, &t.Firings, &t.White, &t.Refired, &t.Excused); err != nil {
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
	// Excused is a firing a quiet window excused: neither white nor black.
	Excused bool
}

// FiringQuery picks firings: those that started in [From, To), of one rule
// when Rule is given, only black stones with Black; the latest first, Limit
// at a time, those before Before (an ID from the last page) when given.
type FiringQuery struct {
	From, To time.Time
	Rule     string
	Black    bool
	Excused  bool // only those a quiet window excused
	Before   int64
	Limit    int
}

// Firings are the firings a query picks.
func (l *Ledger) Firings(ctx context.Context, q FiringQuery) ([]Firing, error) {
	sq := `SELECT f.id, r.name, s.id, s.labels, f.start_at, f.end_at, f.white, f.refired, f.excused
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
		if err := rows.Scan(&f.ID, &f.Rule, &sid, &labels, &start, &end, &f.White, &f.Refired, &f.Excused); err != nil {
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
	err := l.r.QueryRowContext(ctx, `SELECT f.id, r.name, s.id, s.labels, f.start_at, f.end_at, f.white, f.refired, f.excused
		FROM firings f JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id WHERE f.id = ?`, id).
		Scan(&f.ID, &f.Rule, &sid, &labels, &start, &end, &f.White, &f.Refired, &f.Excused)
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
	rep := &verdict.Report{From: from, To: to, NoisyAt: at, Rules: []verdict.RuleResult{}, Incidents: []verdict.IncidentResult{}, Firings: []verdict.Judged{}, SomeRules: o.SomeRules}
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
	byFirings := `SELECT rule_id, count(*) - sum(excused), sum(white), sum(refired * (1 - excused)), sum(excused) FROM firings
		WHERE start_at >= ? AND start_at < ? GROUP BY rule_id`
	parts := []part{{byFirings, from.Unix(), to.Unix()}}
	if d1 < d2 {
		parts = []part{
			{byFirings, from.Unix(), l.midnight(d1)},
			{"SELECT rule_id, sum(firings), sum(white), sum(refired), sum(excused) FROM days WHERE day >= ? AND day < ? GROUP BY rule_id", d1, d2},
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
			var n, white, again, excused int
			if err := rows.Scan(&id, &n, &white, &again, &excused); err != nil {
				rows.Close()
				return nil, err
			}
			rr := result(table[id])
			rr.Firings += n
			rr.White += white
			rr.Black += n - white
			rr.Refired += again
			rr.Excused += excused
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
		ir := verdict.IncidentResult{Incident: in.Incident, CaughtBy: in.CaughtBy}
		for i, c := range ir.CaughtBy {
			if rr, ok := byName[c.Rule]; ok {
				ir.CaughtBy[i].Noisy = rr.Noisy
			}
		}
		rep.Incidents = append(rep.Incidents, ir)
	}
	// What can still fire is what the last version in the period held.
	var inEffect func(string) bool
	if n := len(versions); n > 0 {
		names := versions[n-1].Names
		inEffect = func(rule string) bool { return slices.Contains(names, rule) }
	}
	rep.Credit(inEffect)
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
		if f.Excused {
			continue // counted in the rule's Excused, not judged
		}
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
func (l *Ledger) hoursOf(ctx context.Context, from, to time.Time, h *verdict.Hours, ids []int64, all bool, key func(at int64) int64) (map[int64]map[int64]HourTally, error) {
	out := map[int64]map[int64]HourTally{}
	for all || len(ids) > 0 {
		q := `SELECT rule_id, start_at, white FROM firings WHERE start_at >= ? AND start_at < ? AND excused = 0`
		args := []any{from.Unix(), to.Unix()}
		if !all {
			part := ids[:min(len(ids), 500)]
			ids = ids[len(part):]
			for _, id := range part {
				args = append(args, id)
			}
			q += ` AND rule_id IN (?` + strings.Repeat(", ?", len(part)-1) + `)`
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

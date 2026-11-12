package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// ruleID is the id of the rule of that name, made if there is none.
func (l *Ledger) ruleID(t *txn, name string) (int64, error) {
	if id, ok := t.rules[name]; ok {
		return id, nil
	}
	l.ids.RLock()
	id, ok := l.ids.rules[name]
	l.ids.RUnlock()
	if ok && !t.gone[name] {
		return id, nil
	}
	err := t.QueryRowContext(t.ctx, "SELECT id FROM rules WHERE name = ?", name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = t.QueryRowContext(t.ctx, "INSERT INTO rules (name) VALUES (?) RETURNING id", name).Scan(&id)
	}
	if err != nil {
		return 0, err
	}
	t.rules[name] = id
	return id, nil
}

// seriesID is the id of a rule's alert with those labels, made if there is
// none.
func (l *Ledger) seriesID(t *txn, rule int64, labels map[string]string) (int64, error) {
	if labels == nil {
		labels = map[string]string{}
	}
	b, err := json.Marshal(labels) // keys sorted: one text for one label set
	if err != nil {
		return 0, err
	}
	k := seriesKey{rule, string(b)}
	if id, ok := t.series[k]; ok {
		return id, nil
	}
	l.ids.RLock()
	id, ok := l.ids.series[k]
	l.ids.RUnlock()
	if ok {
		return id, nil
	}
	err = t.QueryRowContext(t.ctx, "SELECT id FROM series WHERE rule_id = ? AND labels = ?", rule, k.labels).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = t.QueryRowContext(t.ctx, "INSERT INTO series (rule_id, labels) VALUES (?, ?) RETURNING id", rule, k.labels).Scan(&id)
	}
	if err != nil {
		return 0, err
	}
	t.series[k] = id
	return id, nil
}

// labels are a series' labels, read once and kept: a series never changes.
func (l *Ledger) labels(id int64, text string) (map[string]string, error) {
	l.ids.RLock()
	c, ok := l.ids.labels[id]
	l.ids.RUnlock()
	if ok && c.text == text {
		return c.m, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return nil, err
	}
	l.ids.Lock()
	l.ids.labels[id] = labelsOf{text, m}
	l.ids.Unlock()
	return m, nil
}

// counted is an incident judged against, as the ledger keeps it.
type counted struct {
	id int64
	incident.Incident
}

// window is the span of firing starts an incident can be caught in.
func window(in incident.Incident, o verdict.Options) (int64, int64) {
	return in.Start.Add(-o.Lead).Unix(), in.EndOr(o.UnknownLen).Unix()
}

// counting reads the incidents judged against whose windows meet [lo, hi].
func (l *Ledger) counting(ctx context.Context, q queryer, lo, hi int64, o verdict.Options) ([]counted, error) {
	return l.incidentsMeeting(ctx, q, lo, hi, o, "counts = 1")
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (l *Ledger) incidentsMeeting(ctx context.Context, q queryer, lo, hi int64, o verdict.Options, where string) ([]counted, error) {
	lead, unknown := int64(o.Lead/time.Second), int64(o.UnknownLen/time.Second)
	rows, err := q.QueryContext(ctx, `SELECT id, key, origin, title, start_at, end_at, labels FROM incidents
		WHERE `+where+` AND start_at <= ? AND coalesce(end_at, start_at + ?) >= ? ORDER BY start_at, key`,
		hi+lead, unknown, lo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []counted
	for rows.Next() {
		c, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, counted{c.row, c.Incident})
	}
	return out, rows.Err()
}

type scanner interface{ Scan(...any) error }

type incidentRow struct {
	row    int64
	origin string
	incident.Incident
}

func scanIncident(s scanner) (incidentRow, error) {
	var r incidentRow
	var start int64
	var end sql.NullInt64
	var labels string
	if err := s.Scan(&r.row, &r.ID, &r.origin, &r.Title, &start, &end, &labels); err != nil {
		return r, err
	}
	r.Start, r.End = unix(start), fromNull(end)
	r.Candidate = r.origin == "impact"
	if err := json.Unmarshal([]byte(labels), &r.Labels); err != nil {
		return r, err
	}
	if len(r.Labels) == 0 {
		r.Labels = nil
	}
	return r, nil
}

// judge gives stones again to the firings that started in [lo, hi], and to
// the incidents they can catch, then counts the days and weeks again. Every
// write calls it over what it changed.
func (l *Ledger) judge(t *txn, lo, hi int64, o verdict.Options) error {
	if hi < lo {
		return nil
	}
	if judging != nil {
		judging(lo, hi)
	}
	flo, fhi, err := l.cover(t, lo, hi, o)
	if err != nil {
		return err
	}
	lo, hi = min(lo, flo), max(hi, fhi)
	incs, err := l.counting(t.ctx, t, lo, hi, o)
	if err != nil {
		return err
	}
	if err := l.stones(t, lo, hi, incs, o); err != nil {
		return err
	}
	if err := l.catches(t, lo, hi, incs, o); err != nil {
		return err
	}
	return l.tally(t, lo, hi)
}

// judging, when set, is told of each span judge is asked to judge: for
// tests of how far a write reaches.
var judging func(lo, hi int64)

// cover works out which impact windows meeting [lo, hi] count: those not
// confirmed or dismissed, and not covered by an incident written down, as
// incident.Merge has it. It returns the span of firings whose stones
// change with them.
func (l *Ledger) cover(t *txn, lo, hi int64, o verdict.Options) (int64, int64, error) {
	flo, fhi := lo, hi
	cands, err := l.incidentsMeeting(t.ctx, t, lo, hi, verdict.Options{}, "origin = 'impact'")
	if err != nil || len(cands) == 0 {
		return flo, fhi, err
	}
	var states = map[int64]struct {
		state  string
		counts bool
	}{}
	span := [2]int64{cands[0].Start.Unix(), 0}
	for _, c := range cands {
		span[1] = max(span[1], c.End.Unix())
	}
	rows, err := t.QueryContext(t.ctx, "SELECT id, state, counts FROM incidents WHERE origin = 'impact' AND start_at <= ? AND end_at >= ?", hi, lo)
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var id int64
		var s struct {
			state  string
			counts bool
		}
		if err := rows.Scan(&id, &s.state, &s.counts); err != nil {
			rows.Close()
			return 0, 0, err
		}
		states[id] = s
	}
	rows.Close()
	written, err := l.incidentsMeeting(t.ctx, t, span[0], span[1], o, "origin != 'impact'")
	if err != nil {
		return 0, 0, err
	}
	ws := make([]incident.Incident, len(written))
	for i, w := range written {
		ws[i] = w.Incident
	}
	for _, c := range cands {
		counts := states[c.id].state == "" && len(incident.Merge(ws, []incident.Incident{c.Incident}, o.UnknownLen, o.Match)) > len(ws)
		if counts == states[c.id].counts {
			continue
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE incidents SET counts = ? WHERE id = ?", counts, c.id); err != nil {
			return 0, 0, err
		}
		a, b := window(c.Incident, o)
		flo, fhi = min(flo, a), max(fhi, b)
	}
	return flo, fhi, nil
}

// stones gives each firing that started in [lo, hi] its stones: white when
// it catches an incident, refired when it came soon after the same alert
// last resolved; and marks those that came during a deploy.
func (l *Ledger) stones(t *txn, lo, hi int64, incs []counted, o verdict.Options) error {
	quiet, err := l.windowsMeeting(t.ctx, t, lo, hi)
	if err != nil {
		return err
	}
	excuses := Excusing(quiet, unix(lo), unix(hi))
	deployed, err := l.deploysMeeting(t.ctx, t, lo, hi, o.DeployBefore, o.DeployAfter)
	if err != nil {
		return err
	}
	deploying := during(deployed, o.DeployBefore, o.DeployAfter)
	rows, err := t.QueryContext(t.ctx, `SELECT f.id, f.series_id, r.name, f.start_at, f.end_at, f.white, f.refired, f.excused, f.deploy, s.labels
		FROM firings f JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id
		WHERE f.start_at >= ? AND f.start_at <= ? ORDER BY f.series_id, f.start_at, f.id`, lo, hi)
	if err != nil {
		return err
	}
	type row struct {
		id, series                    int64
		f                             firing.Firing
		white, again, excused, deploy bool
	}
	var fs []row
	for rows.Next() {
		var r row
		var start int64
		var end sql.NullInt64
		var text string
		if err := rows.Scan(&r.id, &r.series, &r.f.Rule, &start, &end, &r.white, &r.again, &r.excused, &r.deploy, &text); err != nil {
			rows.Close()
			return err
		}
		if r.f.Labels, err = l.labels(r.series, text); err != nil {
			rows.Close()
			return err
		}
		r.f.Start, r.f.End = unix(start), fromNull(end)
		fs = append(fs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	prev, err := t.PrepareContext(t.ctx, "SELECT end_at FROM firings WHERE series_id = ? AND start_at < ? ORDER BY start_at DESC LIMIT 1")
	if err != nil {
		return err
	}
	defer prev.Close()
	up, err := t.PrepareContext(t.ctx, "UPDATE firings SET white = ?, refired = ?, excused = ?, deploy = ? WHERE id = ?")
	if err != nil {
		return err
	}
	defer up.Close()
	var last time.Time // when the series' previous firing resolved
	for i, r := range fs {
		if i == 0 || r.series != fs[i-1].series {
			var end sql.NullInt64
			if err := prev.QueryRowContext(t.ctx, r.series, lo).Scan(&end); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			last = fromNull(end)
		}
		white := slices.ContainsFunc(incs, func(in counted) bool { return verdict.Catches(r.f, in.Incident, o) })
		again := verdict.Refired(last, r.f.Start, o)
		// A firing that caught something counts, quiet window or not.
		excused := !white && excuses(r.f)
		// During a deploy is a mark beside the stones, of what is judged.
		deploy := !excused && deploying(r.f)
		last = r.f.End
		if white == r.white && again == r.again && excused == r.excused && deploy == r.deploy {
			continue
		}
		if _, err := up.ExecContext(t.ctx, white, again, excused, deploy, r.id); err != nil {
			return err
		}
	}
	return nil
}

// catches works out again which rules caught each incident whose window
// meets [lo, hi], from every firing in its window, and drops the catches of
// incidents there that no longer count.
func (l *Ledger) catches(t *txn, lo, hi int64, incs []counted, o verdict.Options) error {
	lead, unknown := int64(o.Lead/time.Second), int64(o.UnknownLen/time.Second)
	if _, err := t.ExecContext(t.ctx, `DELETE FROM catches WHERE incident_id IN (SELECT id FROM incidents
		WHERE counts = 0 AND start_at <= ? AND coalesce(end_at, start_at + ?) >= ?)`, hi+lead, unknown, lo); err != nil {
		return err
	}
	q, err := t.PrepareContext(t.ctx, `SELECT f.rule_id, f.series_id, f.start_at, s.labels FROM firings f JOIN series s ON s.id = f.series_id
		WHERE f.start_at >= ? AND f.start_at <= ?`)
	if err != nil {
		return err
	}
	defer q.Close()
	del, err := t.PrepareContext(t.ctx, "DELETE FROM catches WHERE incident_id = ?")
	if err != nil {
		return err
	}
	defer del.Close()
	ins, err := t.PrepareContext(t.ctx, "INSERT INTO catches (incident_id, rule_id, delay) VALUES (?, ?, ?)")
	if err != nil {
		return err
	}
	defer ins.Close()
	for _, in := range incs {
		a, b := window(in.Incident, o)
		rows, err := q.QueryContext(t.ctx, a, b)
		if err != nil {
			return err
		}
		first := map[int64]int64{}
		for rows.Next() {
			var rule, sid, start int64
			var text string
			if err := rows.Scan(&rule, &sid, &start, &text); err != nil {
				rows.Close()
				return err
			}
			labels, err := l.labels(sid, text)
			if err != nil {
				rows.Close()
				return err
			}
			if !verdict.Catches(firing.Firing{Labels: labels, Start: unix(start)}, in.Incident, o) {
				continue
			}
			d := start - in.Start.Unix()
			if have, ok := first[rule]; !ok || d < have {
				first[rule] = d
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if _, err := del.ExecContext(t.ctx, in.id); err != nil {
			return err
		}
		for _, rule := range slices.Sorted(maps.Keys(first)) {
			if _, err := ins.ExecContext(t.ctx, in.id, rule, first[rule]); err != nil {
				return err
			}
		}
	}
	return nil
}

// tally counts each rule's stones again for the days and weeks [lo, hi]
// falls in.
func (l *Ledger) tally(t *txn, lo, hi int64) error {
	d1, d2 := l.day(lo), l.day(hi)
	if _, err := t.ExecContext(t.ctx, "DELETE FROM days WHERE day >= ? AND day <= ?", d1, d2); err != nil {
		return err
	}
	// An excused firing is counted apart: neither white nor black.
	if _, err := t.ExecContext(t.ctx, `INSERT INTO days (day, rule_id, firings, white, refired, excused, deploy, deploy_white)
		SELECT day, rule_id, count(*) - sum(excused), sum(white), sum(refired * (1 - excused)), sum(excused), sum(deploy), sum(deploy * white) FROM firings
		WHERE start_at >= ? AND start_at < ? GROUP BY day, rule_id`, l.midnight(d1), l.midnight(d2+1)); err != nil {
		return err
	}
	w1, w2 := Week(d1), Week(d2)
	if w2-w1 > 7*400 {
		t.all = true // a span of years: every week, rather than a list of them
	} else {
		for w := w1; w <= w2; w += 7 {
			t.weeks = append(t.weeks, w)
		}
	}
	if _, err := t.ExecContext(t.ctx, "DELETE FROM weeks WHERE week >= ? AND week <= ?", w1, w2); err != nil {
		return err
	}
	_, err := t.ExecContext(t.ctx, `INSERT INTO weeks (week, rule_id, firings, white, refired, excused, deploy, deploy_white)
		SELECT day - (day + 3) % 7, rule_id, sum(firings), sum(white), sum(refired), sum(excused), sum(deploy), sum(deploy_white) FROM days
		WHERE day >= ? AND day <= ? GROUP BY 1, 2`, w1, w2+6)
	return err
}

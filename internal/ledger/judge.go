package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
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
// none: the labels as kept (keptLabels).
func (l *Ledger) seriesID(t *txn, rule int64, labels map[string]string) (int64, error) {
	k, err := keyOf(t.d, rule, labels)
	if err != nil {
		return 0, err
	}
	if id, ok := t.series[k]; ok {
		return id, nil
	}
	l.ids.RLock()
	id, ok := l.ids.series[k]
	l.ids.RUnlock()
	if ok {
		return id, nil
	}
	// In Postgres, through series_labels, which holds the labels' md5.
	err = t.QueryRowContext(t.ctx, t.d.pick("SELECT id FROM series WHERE rule_id = ? AND labels = ?",
		"SELECT id FROM series WHERE rule_id = ?1 AND md5(labels) = md5(?2) AND labels = ?2"), rule, k.labels).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		var untied int64
		if untied, err = t.untied(); err == nil {
			err = t.QueryRowContext(t.ctx, "INSERT INTO series (rule_id, labels, signals) VALUES (?, ?, ?) RETURNING id", rule, k.labels, untied).Scan(&id)
		}
	}
	if err != nil {
		return 0, err
	}
	t.series[k] = id
	return id, nil
}

// findSeries finds, in Postgres, the alerts of the keys given that the
// ledger has, and makes those it hasn't, many a statement rather than a
// round trip or two each; seriesID then has them all. SQLite's are found
// one at a time, as they cost nothing to ask for.
func (l *Ledger) findSeries(t *txn, keys []seriesKey) error {
	if t.d != postgres {
		return nil
	}
	var want []seriesKey
	seen := map[seriesKey]bool{}
	l.ids.RLock()
	for _, k := range keys {
		if _, ok := t.series[k]; ok || seen[k] {
			continue
		}
		if _, ok := l.ids.series[k]; ok {
			continue
		}
		seen[k] = true
		want = append(want, k)
	}
	l.ids.RUnlock()
	var missing []seriesKey
	for part := range slices.Chunk(want, pgRows) {
		pairs := make([][2]any, len(part))
		for i, k := range part {
			pairs[i] = [2]any{k.rule, k.labels}
		}
		b, err := json.Marshal(pairs)
		if err != nil {
			return err
		}
		rows, err := t.QueryContext(t.ctx, `SELECT s.id, s.rule_id, s.labels FROM series s
			JOIN (SELECT CAST(e ->> 0 AS BIGINT) AS rule_id, e ->> 1 AS labels FROM jsonb_array_elements(CAST(? AS jsonb)) AS e) AS k
			ON s.rule_id = k.rule_id AND md5(s.labels) = md5(k.labels) AND s.labels = k.labels`, string(b))
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var k seriesKey
			if err := rows.Scan(&id, &k.rule, &k.labels); err != nil {
				rows.Close()
				return err
			}
			t.series[k] = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, k := range part {
			if _, ok := t.series[k]; !ok {
				missing = append(missing, k)
			}
		}
	}
	ids, err := l.makeSeries(t, missing)
	if err != nil {
		return err
	}
	for i, k := range missing {
		t.series[k] = ids[i]
	}
	return nil
}

// keyOf is the key of a rule's alert with those labels: their text, keys
// sorted, one text for one label set.
func keyOf(d dialect, rule int64, labels map[string]string) (seriesKey, error) {
	if labels == nil {
		labels = map[string]string{}
	}
	b, err := json.Marshal(keptLabels(labels)) // keys sorted: one text for one label set
	return seriesKey{rule, d.kept(string(b))}, err
}

// makeSeries makes alerts new to the ledger, in turn, and gives their ids:
// in Postgres, many a statement.
func (l *Ledger) makeSeries(t *txn, ks []seriesKey) ([]int64, error) {
	ids := make([]int64, 0, len(ks))
	untied, err := t.untied()
	if err != nil {
		return nil, err
	}
	if t.d != postgres {
		st, err := t.PrepareContext(t.ctx, "INSERT INTO series (rule_id, labels, signals) VALUES (?, ?, ?) RETURNING id")
		if err != nil {
			return nil, err
		}
		defer st.Close()
		for _, k := range ks {
			var id int64
			if err := st.QueryRowContext(t.ctx, k.rule, k.labels, untied).Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, nil
	}
	for len(ks) > 0 {
		part := ks[:min(len(ks), pgRows)]
		ks = ks[len(part):]
		args := make([]any, 0, 2*len(part))
		for _, k := range part {
			args = append(args, k.rule, k.labels)
		}
		rows, err := t.QueryContext(t.ctx, "INSERT INTO series (rule_id, labels, signals) VALUES "+
			strings.TrimSuffix(strings.Repeat(fmt.Sprintf("(CAST(? AS BIGINT), CAST(? AS TEXT), %d), ", untied), len(part)), ", ")+" RETURNING id, rule_id, labels", args...)
		if err != nil {
			return nil, err
		}
		got := map[seriesKey]int64{}
		for rows.Next() {
			var id int64
			var k seriesKey
			if err := rows.Scan(&id, &k.rule, &k.labels); err != nil {
				rows.Close()
				return nil, err
			}
			got[k] = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, k := range part {
			ids = append(ids, got[k])
		}
	}
	return ids, nil
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
	if was, ok := l.ids.labels[id]; ok {
		l.ids.labelsBytes -= labelsCost(was.text)
	}
	l.ids.labels[id] = labelsOf{text, m}
	l.ids.labelsBytes += labelsCost(text)
	l.trimCached()
	l.ids.Unlock()
	return m, nil
}

// keptLabels are a replayed alert's labels as the ledger keeps them
// (verdict.KeptLabels): 4 KB in all, a name of 128 bytes and a value of
// 256, as the alerts it is sent; cut past them, with a hash.
func keptLabels(m map[string]string) map[string]string { return verdict.KeptLabels(m) }

// counted is an incident judged against, as the ledger keeps it.
type counted struct {
	id int64
	incident.Incident
}

// window is the span of firing starts an incident can be caught in: from
// the longest lead in effect before it (verdict.Options.MaxLead), each
// firing's own judged as it is looked at (verdict.Catches).
func window(in incident.Incident, o verdict.Options) (int64, int64) {
	return in.Start.Add(-o.MaxLead()).Unix(), in.EndOr(o.UnknownLen).Unix()
}

// counting reads the incidents judged against whose windows meet [lo, hi].
func (l *Ledger) counting(ctx context.Context, q queryer, lo, hi int64, o verdict.Options) ([]counted, error) {
	return l.incidentsMeeting(ctx, q, lo, hi, o, "counts = 1")
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (l *Ledger) incidentsMeeting(ctx context.Context, q queryer, lo, hi int64, o verdict.Options, where string) ([]counted, error) {
	lead, unknown := int64(o.MaxLead()/time.Second), int64(o.UnknownLen/time.Second)
	rows, err := q.QueryContext(ctx, `SELECT `+incidentRowCols+` FROM incidents
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

// incidentRowCols are the columns of incidents scanIncident reads, in order.
const incidentRowCols = "id, key, origin, title, start_at, end_at, labels, signal, match_labels, real_of, alerts"

type incidentRow struct {
	row    int64
	origin string
	// realOf is the alert and start "This was real" wrote it from
	// (realKey), "" for none.
	realOf string
	incident.Incident
}

func scanIncident(s scanner) (incidentRow, error) {
	var r incidentRow
	var start int64
	var end sql.NullInt64
	var labels, alerts string
	var match sql.NullString
	if err := s.Scan(&r.row, &r.ID, &r.origin, &r.Title, &start, &end, &labels, &r.Signal, &match, &r.realOf, &alerts); err != nil {
		return r, err
	}
	r.Start, r.End, r.Match, r.Real = unix(start), fromNull(end), matchOf(match), r.realOf != ""
	r.Candidate = r.origin == "impact"
	if err := json.Unmarshal([]byte(labels), &r.Labels); err != nil {
		return r, err
	}
	var err error
	if r.Alerts, err = alertsOf(alerts); err != nil {
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
//
// With options read before the named signals last changed (staleSignals),
// what it judges is pended too, to be judged again with options read anew
// (JudgePending): the stones it gives stand till then.
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
	// People's verdicts on firings no longer there, each to the firing it
	// finds again, which is judged with the rest (docs/design/judging.md,
	// item 3).
	if lo, hi, err = l.moveVerdicts(t, lo, hi, o); err != nil {
		return err
	}
	if stale, err := l.staleSignals(t, o); err != nil {
		return err
	} else if stale {
		if err := pend(t, [][2]int64{{lo, hi}}); err != nil {
			return err
		}
	}
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
	// The alerts new since, tied to the signals that could have seen
	// them, before their firings are counted.
	if err := l.tieUntied(t, o); err != nil {
		return err
	}
	// The episodes of the days the tally counts, from the stones just
	// given (docs/design/judging.md, item 2).
	if err := l.walkEpisodes(t, lo, hi, o); err != nil {
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
// last resolved; and marks those that came during a deploy, and with the
// kinds of the marked windows they came during.
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
	ms, err := l.markedMeeting(t.ctx, t, lo, hi)
	if err != nil {
		return err
	}
	marks := marking(ms, unix(lo), unix(hi), func(at int64) int64 { return Week(l.day(at)) })
	// People's verdicts on the firings judged (docs/design/judging.md,
	// item 3), by alert and start.
	said, err := l.verdictsNear(t, lo, hi)
	if err != nil {
		return err
	}
	rows, err := t.QueryContext(t.ctx, `SELECT f.id, f.series_id, r.name, f.start_at, f.end_at, f.white, f.refired, f.excused, f.deploy, f.marked, s.labels, f.person, f.unrelated
		FROM firings f JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id
		WHERE f.start_at >= ? AND f.start_at <= ? ORDER BY f.series_id, f.start_at, f.id`, lo, hi)
	if err != nil {
		return err
	}
	type row struct {
		id, series                    int64
		f                             firing.Firing
		white, again, excused, deploy bool
		marked, person                int64
		unrelated                     string
	}
	var fs []row
	for rows.Next() {
		var r row
		var start int64
		var end sql.NullInt64
		var text string
		if err := rows.Scan(&r.id, &r.series, &r.f.Rule, &start, &end, &r.white, &r.again, &r.excused, &r.deploy, &r.marked, &text, &r.person, &r.unrelated); err != nil {
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
	// In Postgres, when each series' firing before lo resolved, asked for
	// all the series at once, rather than a round trip each.
	var ends map[int64]sql.NullInt64
	if t.d == postgres {
		var ids []int64
		for i, r := range fs {
			if i == 0 || r.series != fs[i-1].series {
				ids = append(ids, r.series)
			}
		}
		if ends, err = l.endsBefore(t, ids, lo); err != nil {
			return err
		}
	}
	up, err := t.updating("firings", []string{"white", "refired", "excused", "deploy", "marked", "person"})
	if err != nil {
		return err
	}
	defer up.close()
	moved := map[int64]string{} // verdicts to be looked at again, by id, and why
	var last time.Time          // when the series' previous firing resolved
	for i, r := range fs {
		if i == 0 || r.series != fs[i-1].series {
			var end sql.NullInt64
			if ends != nil {
				end = ends[r.series]
			} else if err := prev.QueryRowContext(t.ctx, r.series, lo).Scan(&end); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			last = fromNull(end)
		}
		var caught []counted
		for _, in := range incs {
			if verdict.Catches(r.f, in.Incident, o) {
				caught = append(caught, in)
			}
		}
		// People's word on it: an incident it was said to be unrelated to
		// it doesn't catch; and whether it needed action.
		k := [2]int64{r.series, r.f.Start.Unix()}
		v := said.at[k]
		white, person, unrelated, why := personOf(caught, v, said.held[k])
		if why != "" && v.ID != 0 && v.Moved == "" {
			moved[v.ID] = why
		}
		again := verdict.Refired(last, r.f.Start, o)
		// A firing that caught something counts, quiet window or not.
		excused := !white && excuses(r.f)
		// White only by what "This was real" wrote, in a quiet window
		// that matches it: as Lapilli judged it, excused.
		if person&personRealWhite != 0 && excuses(r.f) {
			person |= personRealQuiet
		}
		if text := unrelatedText(unrelated); text != r.unrelated {
			if _, err := t.ExecContext(t.ctx, "UPDATE firings SET unrelated = ? WHERE id = ?", text, r.id); err != nil {
				return err
			}
		}
		// During a deploy is a mark beside the stones, of what is judged.
		deploy := !excused && deploying(r.f)
		// And the kinds of marked window it came during: marks, which
		// change nothing else (docs/design/marked.md).
		var marked int64
		if !excused {
			marked = marks(r.f)
		}
		last = r.f.End
		if white == r.white && again == r.again && excused == r.excused && deploy == r.deploy && marked == r.marked && person == r.person {
			continue
		}
		if err := up.add(white, again, excused, deploy, marked, person, r.id); err != nil {
			return err
		}
	}
	if err := up.close(); err != nil {
		return err
	}
	for id, why := range moved {
		if _, err := t.ExecContext(t.ctx, "UPDATE verdicts SET moved = ? WHERE id = ? AND moved = ''", why, id); err != nil {
			return err
		}
	}
	return nil
}

// endsBefore is, for each series, when its last firing to start before a
// time resolved: of those that started at once, the last written, as
// SQLite's index gives it.
func (l *Ledger) endsBefore(t *txn, ids []int64, before int64) (map[int64]sql.NullInt64, error) {
	out := map[int64]sql.NullInt64{}
	for len(ids) > 0 {
		part := ids[:min(len(ids), pgRows)]
		ids = ids[len(part):]
		b, err := json.Marshal(part)
		if err != nil {
			return nil, err
		}
		rows, err := t.QueryContext(t.ctx, `SELECT s.id, (SELECT f.end_at FROM firings f WHERE f.series_id = s.id AND f.start_at < ?
			ORDER BY f.start_at DESC, f.id DESC LIMIT 1) FROM (`+t.d.ints("?")+`) AS s (id)`, before, string(b))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var end sql.NullInt64
			if err := rows.Scan(&id, &end); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = end
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// catches works out again which rules caught each incident whose window
// meets [lo, hi], from every firing in its window, and drops the catches of
// incidents there that no longer count.
func (l *Ledger) catches(t *txn, lo, hi int64, incs []counted, o verdict.Options) error {
	lead, unknown := int64(o.MaxLead()/time.Second), int64(o.UnknownLen/time.Second)
	if _, err := t.ExecContext(t.ctx, `DELETE FROM catches WHERE incident_id IN (SELECT id FROM incidents
		WHERE counts = 0 AND start_at <= ? AND coalesce(end_at, start_at + ?) >= ?)`, hi+lead, unknown, lo); err != nil {
		return err
	}
	q, err := t.PrepareContext(t.ctx, `SELECT f.rule_id, f.series_id, f.start_at, s.labels, r.name, f.unrelated FROM firings f JOIN series s ON s.id = f.series_id
		JOIN rules r ON r.id = f.rule_id WHERE f.start_at >= ? AND f.start_at <= ?`)
	if err != nil {
		return err
	}
	defer q.Close()
	del, err := t.PrepareContext(t.ctx, "DELETE FROM catches WHERE incident_id = ?")
	if err != nil {
		return err
	}
	defer del.Close()
	ins, err := t.PrepareContext(t.ctx, "INSERT INTO catches (incident_id, rule_id, delay, linked, loose) VALUES (?, ?, ?, ?, ?)")
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
		linked := map[int64]bool{}     // the rules a firing it lists caught it with
		proper := map[int64]bool{}     // the rules a firing that isn't loose caught it with
		lacked := map[int64][]string{} // the labels the loose firings of a rule lacked
		for rows.Next() {
			var rule, sid, start int64
			var text, name, unrelated string
			if err := rows.Scan(&rule, &sid, &start, &text, &name, &unrelated); err != nil {
				rows.Close()
				return err
			}
			// A firing a person said is unrelated to it didn't catch it:
			// what caught it is worked out from the others.
			if unrelated != "" && slices.Contains(unrelatedKeys(unrelated), in.ID) {
				continue
			}
			labels, err := l.labels(sid, text)
			if err != nil {
				rows.Close()
				return err
			}
			// With its rule, whose entry may keep it from a signal's
			// windows (docs/design/signals.md, "Judged by its own match").
			caught, link, lack := verdict.CatchesLinked(firing.Firing{Rule: name, Labels: labels, Start: unix(start)}, in.Incident, o)
			if !caught {
				continue
			}
			linked[rule] = linked[rule] || link
			if len(lack) == 0 {
				proper[rule] = true
			} else {
				lacked[rule] = verdict.MergeLacked(lacked[rule], lack)
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
			loose := ""
			if !proper[rule] {
				loose = strings.Join(lacked[rule], ",")
			}
			if _, err := ins.ExecContext(t.ctx, in.id, rule, first[rule], linked[rule], loose); err != nil {
				return err
			}
		}
	}
	return nil
}

// personSums are, of the firings a tally counts, the people's bits
// (firings.person): those said unrelated, and of them those excused;
// those white only by "This was real", and of them those a quiet window
// would have excused; and, of those not excused, the answers to needed
// action, and of them those that needed action and caught nothing, and
// those that needed none and caught something.
const personSums = `sum((f.person & 1)), sum((f.person & 1) * f.excused), sum((f.person & 8) / 8), sum((f.person & 16) / 16),
	sum(((f.person & 2) / 2) * (1 - f.excused)), sum(((f.person & 4) / 4) * (1 - f.excused)),
	sum(((f.person & 2) / 2) * (1 - f.excused) * (1 - f.white)), sum(((f.person & 4) / 4) * f.white)`

// tally counts each rule's stones again for the days and weeks [lo, hi]
// falls in.
func (l *Ledger) tally(t *txn, lo, hi int64) error {
	d1, d2 := l.day(lo), l.day(hi)
	if _, err := t.ExecContext(t.ctx, "DELETE FROM days WHERE day >= ? AND day <= ?", d1, d2); err != nil {
		return err
	}
	// An excused firing is counted apart: neither white nor black. By
	// the signals its alert is tied to, too, so that a rule's firings are
	// judged by them apart (docs/design/signals.md, "In the ledger").
	if _, err := t.ExecContext(t.ctx, `INSERT INTO days (day, rule_id, signals, firings, white, refired, excused, deploy, deploy_white, episodes, episodes_white,
			unrelated, unrelated_excused, real_white, real_excused, action_yes, action_no, action_yes_black, action_no_white)
		SELECT f.day, f.rule_id, s.signals, count(*) - sum(f.excused), sum(f.white), sum(f.refired * (1 - f.excused)), sum(f.excused), sum(f.deploy), sum(f.deploy * f.white),
			`+firstFirings+`, sum(f.episode_white), `+personSums+`
		FROM firings f JOIN series s ON s.id = f.series_id
		WHERE f.start_at >= ? AND f.start_at < ? GROUP BY f.day, f.rule_id, s.signals`, l.midnight(d1), l.midnight(d2+1)); err != nil {
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
	if _, err := t.ExecContext(t.ctx, `INSERT INTO weeks (week, rule_id, signals, firings, white, refired, excused, deploy, deploy_white, episodes, episodes_white,
			unrelated, unrelated_excused, real_white, real_excused, action_yes, action_no, action_yes_black, action_no_white)
		SELECT day - (day + 3) % 7, rule_id, signals, sum(firings), sum(white), sum(refired), sum(excused), sum(deploy), sum(deploy_white), sum(episodes), sum(episodes_white),
			sum(unrelated), sum(unrelated_excused), sum(real_white), sum(real_excused), sum(action_yes), sum(action_no), sum(action_yes_black), sum(action_no_white) FROM days
		WHERE day >= ? AND day <= ? GROUP BY 1, 2, 3`, w1, w2+6); err != nil {
		return err
	}
	// The firings marked with each kind, in the same tally.
	return l.tallyMarked(t, d1, d2, w1, w2)
}

package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// Rule is what the ledger knows of a rule besides its firings.
type Rule struct {
	Name, Group, Severity string
	// Unsupported is why it couldn't be replayed the last time it was.
	Unsupported string
	// Source is, for a rule whose firings are recorded elsewhere and copied
	// in, the source they come from; "" for a replayed rule.
	Source string
}

// Found is what a run found over the period [From, To].
type Found struct {
	From, To time.Time
	// Missed is, by rule, an earlier From a run before couldn't replay the
	// rule from: its firings are replaced from there.
	Missed map[string]time.Time
	// Since is, by rule, how long before its From its replay started, so
	// as to see a firing's pending time; its From for a rule not in it. A
	// firing that started before its From continues one the ledger has.
	Since map[string]time.Time
	// Fresh is, by rule, a time after which a firing the replay saw start
	// started then, pending in the replay and not already on before it:
	// its start plus the rule's longest "for" and group interval.
	Fresh map[string]time.Time
	// Rules are the rules the run knew of.
	Rules []Rule
	// Keep are rules the run couldn't replay this time, a query failing or
	// time running out: their firings are kept as they were, and the next
	// run replays them from this one's From.
	Keep map[string]bool
	// Firings are every rule's firings, including those that started
	// before From and were still on at it.
	Firings []firing.Firing
	// Impact, when there is an impact expression, is what it found from
	// ImpactSince on.
	Impact      []incident.Incident
	ImpactOn    bool
	ImpactSince time.Time
	Run         Run
}

// from is where a rule's firings are replaced from.
func (f Found) from(rule string) time.Time {
	if m, ok := f.Missed[rule]; ok && m.Before(f.From) {
		return m
	}
	return f.From
}

func (f Found) since(rule string) time.Time {
	if s, ok := f.Since[rule]; ok {
		return s
	}
	return f.from(rule)
}

// Run is one run, kept so the runs can be looked back on.
type Run struct {
	ID                int64
	Kind              string // backfill, hourly, review, digest
	Started, Finished time.Time
	From, To          time.Time
	Firings           int
	Failed            map[string]string // rule to why it couldn't be replayed
	Error             string
}

// missedFor is how long a rule's missed period is held for it: past that,
// it is given up.
const missedFor = 7 * 24 * time.Hour

// Record puts what a run found in the ledger, in place of what earlier runs
// found from From on, gives the stones again where they may have changed,
// and moves the cursor to To. It all happens, or none of it does.
func (l *Ledger) Record(ctx context.Context, f Found, o verdict.Options) error {
	return l.write(ctx, func(t *txn) error {
		// A recorded rule holding a name the run judges a rule under is
		// named apart first.
		seen := make(map[string]bool, len(f.Rules))
		var names []string
		for _, r := range f.Rules {
			if !seen[r.Name] {
				seen[r.Name] = true
				names = append(names, r.Name)
			}
		}
		for _, x := range f.Firings {
			if !seen[x.Rule] {
				seen[x.Rule] = true
				names = append(names, x.Rule)
			}
		}
		list, _, err := l.latestTies(t.ctx, t)
		if err != nil {
			return err
		}
		kept, err := l.tie(t, list)
		if err != nil {
			return err
		}
		if err := l.yield(t, names, kept); err != nil {
			return err
		}
		if err := l.rules(t, f.Rules); err != nil {
			return err
		}
		to := f.To.Unix()
		lo := f.From.Unix()
		for name := range f.Missed {
			if !f.Keep[name] {
				lo = min(lo, f.from(name).Unix())
			}
		}
		// The alerts of the firings below, found or made at once, in the
		// order the loops below come to them, as SQLite makes them one at a
		// time: their rules' ids and theirs are the same in both, and so
		// are ties broken by id.
		if t.d == postgres {
			var keys []seriesKey
			for _, continued := range []bool{true, false} {
				for _, x := range f.Firings {
					before := x.Start.Before(f.from(x.Rule))
					if f.Keep[x.Rule] || before != continued || !before && x.Start.After(f.To) {
						continue
					}
					rule, err := l.ruleID(t, x.Rule)
					if err != nil {
						return err
					}
					k, err := keyOf(t.d, rule, x.Labels)
					if err != nil {
						return err
					}
					keys = append(keys, k)
				}
			}
			if err := l.findSeries(t, keys); err != nil {
				return err
			}
		}
		// Firings that started before their rule's From: an alert the
		// ledger has as still on, or one that started in the margin and was
		// found before.
		continued := map[int64]bool{}
		for _, x := range f.Firings {
			if !x.Start.Before(f.from(x.Rule)) || f.Keep[x.Rule] {
				continue
			}
			rule, err := l.ruleID(t, x.Rule)
			if err != nil {
				return err
			}
			sid, err := l.seriesID(t, rule, x.Labels)
			if err != nil {
				return err
			}
			var id, start int64
			// The same firing, found before; or, for one the replay saw
			// only from where it started, already on, the one on then.
			s := x.Start.Unix()
			if fresh, ok := f.Fresh[x.Rule]; ok && x.Start.After(fresh) {
				err = t.QueryRowContext(t.ctx, "SELECT id, start_at FROM firings WHERE series_id = ? AND start_at = ? LIMIT 1", sid, s).Scan(&id, &start)
			} else {
				err = t.QueryRowContext(t.ctx, `SELECT id, start_at FROM firings WHERE series_id = ? AND start_at <= ?
					AND (start_at = ? OR end_at IS NULL OR end_at >= ?) ORDER BY start_at = ? DESC, start_at DESC LIMIT 1`,
					sid, s, s, f.since(x.Rule).Unix(), s).Scan(&id, &start)
			}
			switch {
			case errors.Is(err, sql.ErrNoRows):
				if id, err = l.insert(t, rule, sid, x); err != nil {
					return err
				}
				lo = min(lo, s)
			case err != nil:
				return err
			default:
				if _, err := t.ExecContext(t.ctx, "UPDATE firings SET end_at = ? WHERE id = ?", nullable(x.End), id); err != nil {
					return err
				}
			}
			continued[id] = true
		}
		// An alert the ledger has as still on, not found on now, resolved
		// before the replay started. A recorded one ends when its source
		// says so.
		rows, err := t.QueryContext(t.ctx, `SELECT f.id, f.start_at, r.name FROM firings f JOIN rules r ON r.id = f.rule_id
			WHERE f.end_at IS NULL AND f.start_at < ? AND r.source = ''`, f.From.Unix())
		if err != nil {
			return err
		}
		ended := map[int64]int64{}
		for rows.Next() {
			var id, start int64
			var name string
			if err := rows.Scan(&id, &start, &name); err != nil {
				rows.Close()
				return err
			}
			if continued[id] || f.Keep[name] || start >= f.from(name).Unix() {
				continue
			}
			ended[id] = f.since(name).Unix()
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for id, at := range ended {
			if _, err := t.ExecContext(t.ctx, "UPDATE firings SET end_at = "+t.d.greatest("start_at", "?")+" WHERE id = ?", at, id); err != nil {
				return err
			}
		}

		// From From on, the run's firings replace what was there; for a
		// rule an earlier run missed, from where it missed. Recorded
		// firings are their source's to replace.
		keep := make([]string, 0, len(f.Keep))
		for name := range f.Keep {
			keep = append(keep, name)
		}
		slices.Sort(keep)
		keepJSON, _ := json.Marshal(keep)
		if _, err := t.ExecContext(t.ctx, `DELETE FROM firings WHERE start_at >= ? AND start_at <= ?
			AND rule_id NOT IN (`+t.d.pick("SELECT r.id FROM rules r, json_each(?) k WHERE r.name = k.value",
			"SELECT id FROM rules WHERE name IN ("+t.d.texts("?")+")")+`)
			AND rule_id NOT IN (SELECT id FROM rules WHERE source != '')`, f.From.Unix(), to, string(keepJSON)); err != nil {
			return err
		}
		for name := range f.Missed {
			if f.Keep[name] || !f.from(name).Before(f.From) {
				continue
			}
			if _, err := t.ExecContext(t.ctx, `DELETE FROM firings WHERE rule_id = (SELECT id FROM rules WHERE name = ? AND source = '')
				AND start_at >= ? AND start_at < ?`, name, f.from(name).Unix(), f.From.Unix()); err != nil {
				return err
			}
		}
		n := 0
		ins, err := t.inserting("firings", []string{"series_id", "rule_id", "start_at", "end_at", "day"}, []string{"BIGINT", "BIGINT", "BIGINT", "BIGINT", "BIGINT"})
		if err != nil {
			return err
		}
		for _, x := range f.Firings {
			if x.Start.Before(f.from(x.Rule)) || x.Start.After(f.To) || f.Keep[x.Rule] {
				continue
			}
			rule, err := l.ruleID(t, x.Rule)
			if err != nil {
				ins.close()
				return err
			}
			sid, err := l.seriesID(t, rule, x.Labels)
			if err != nil {
				ins.close()
				return err
			}
			if err := ins.add(sid, rule, x.Start.Unix(), nullable(x.End), l.day(x.Start.Unix())); err != nil {
				ins.close()
				return err
			}
			n++
		}
		if err := ins.close(); err != nil {
			return err
		}

		if f.ImpactOn {
			ilo, err := l.impact(t, f)
			if err != nil {
				return err
			}
			lo = min(lo, ilo)
		}
		if err := l.judge(t, lo-int64(o.Lead/time.Second), to, o); err != nil {
			return err
		}
		if err := l.miss(t, f); err != nil {
			return err
		}
		f.Run.Firings = n
		if err := l.addRun(t, f.Run); err != nil {
			return err
		}
		return setMeta(t.ctx, t, "cursor", strconv.FormatInt(to, 10))
	})
}

// miss keeps, for the next run, where each rule this run couldn't replay
// was missed from, and forgets those it could.
func (l *Ledger) miss(t *txn, f Found) error {
	missed, err := l.missed(t.ctx, t)
	if err != nil {
		return err
	}
	for name := range f.Keep {
		at := f.from(name)
		if was, ok := missed[name]; !ok || at.Before(was) {
			missed[name] = at
		}
	}
	for _, r := range f.Rules {
		if !f.Keep[r.Name] {
			delete(missed, r.Name)
		}
	}
	for name, at := range missed {
		if f.To.Sub(at) > missedFor {
			delete(missed, name) // given up: too long to keep replaying
		}
	}
	b, err := json.Marshal(missed)
	if err != nil {
		return err
	}
	return setMeta(t.ctx, t, "missed", string(b))
}

// Missed is, by rule, where a run couldn't replay it from, for the next
// run to start there.
func (l *Ledger) Missed(ctx context.Context) (map[string]time.Time, error) {
	return l.missed(ctx, l.r)
}

func (l *Ledger) missed(ctx context.Context, q querier) (map[string]time.Time, error) {
	v, err := l.meta(ctx, q, "missed")
	out := map[string]time.Time{}
	if err != nil || v == "" {
		return out, err
	}
	return out, json.Unmarshal([]byte(v), &out)
}

func (l *Ledger) insert(t *txn, rule, series int64, x firing.Firing) (int64, error) {
	var id int64
	err := t.QueryRowContext(t.ctx, "INSERT INTO firings (series_id, rule_id, start_at, end_at, day) VALUES (?, ?, ?, ?, ?) RETURNING id",
		series, rule, x.Start.Unix(), nullable(x.End), l.day(x.Start.Unix())).Scan(&id)
	return id, err
}

// rules keeps what the run knew of each rule.
func (l *Ledger) rules(t *txn, rs []Rule) error {
	for _, r := range rs {
		id, err := l.ruleID(t, r.Name)
		if err != nil {
			return err
		}
		// A recorded rule's severity is how its source tells it apart: an
		// alarm's history keeps its own.
		if _, err := t.ExecContext(t.ctx, "UPDATE rules SET grp = ?, severity = CASE WHEN source = '' THEN ? ELSE severity END, unsupported = ? WHERE id = ?",
			r.Group, r.Severity, r.Unsupported, id); err != nil {
			return err
		}
	}
	return nil
}

// impact puts the impact windows a run found in place of those found
// before from From on, keeping what people decided about them: a window
// that overlaps one confirmed or dismissed is too. It returns the earliest
// start whose firings' stones may have changed.
func (l *Ledger) impact(t *txn, f Found) (int64, error) {
	from := f.From.Unix()
	lo := from
	type old struct {
		start, end, updated int64
		state, by           string
		labels              incident.Labels
	}
	rows, err := t.QueryContext(t.ctx, `SELECT start_at, end_at, state, labels, updated, updated_by FROM incidents
		WHERE origin = 'impact' AND start_at >= ? AND state != ''`, from)
	if err != nil {
		return 0, err
	}
	var decided []old
	for rows.Next() {
		var o old
		var labels string
		if err := rows.Scan(&o.start, &o.end, &o.state, &labels, &o.updated, &o.by); err != nil {
			rows.Close()
			return 0, err
		}
		if err := json.Unmarshal([]byte(labels), &o.labels); err != nil {
			rows.Close()
			return 0, err
		}
		decided = append(decided, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if _, err := t.ExecContext(t.ctx, "DELETE FROM catches WHERE incident_id IN (SELECT id FROM incidents WHERE origin = 'impact' AND start_at >= ?)", from); err != nil {
		return 0, err
	}
	if _, err := t.ExecContext(t.ctx, "DELETE FROM incidents WHERE origin = 'impact' AND start_at >= ?", from); err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	for _, in := range f.Impact {
		labels, err := json.Marshal(orEmpty(in.Labels))
		if err != nil {
			return 0, err
		}
		if in.Start.Before(f.From) {
			// Still going at the start: the window found before goes on.
			res, err := t.ExecContext(t.ctx, `UPDATE incidents SET end_at = ? WHERE id = (SELECT id FROM incidents
				WHERE origin = 'impact' AND start_at < ? AND end_at >= ? ORDER BY start_at DESC LIMIT 1)`,
				in.End.Unix(), from, f.ImpactSince.Unix())
			if err != nil {
				return 0, err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				continue
			}
			lo = min(lo, in.Start.Unix())
		}
		// What was decided about a window it overlaps, sharing a value of
		// each label both have, holds for it.
		state, updated, by := "", now, ""
		for _, d := range decided {
			if d.start <= in.End.Unix() && d.end >= in.Start.Unix() && share(d.labels, in.Labels) {
				state, updated, by = d.state, d.updated, d.by
			}
		}
		key := "impact-" + in.Start.UTC().Format("20060102T1504Z")
		if _, err := t.ExecContext(t.ctx, `INSERT INTO incidents (key, origin, state, counts, title, start_at, end_at, labels, updated, updated_by)
			VALUES (?, 'impact', ?, 0, ?, ?, ?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`,
			key, state, in.Title, in.Start.Unix(), in.End.Unix(), string(labels), updated, by); err != nil {
			return 0, err
		}
	}
	return lo, nil
}

// share reports whether, for every label both have, they have a value in
// common.
func share(a, b incident.Labels) bool {
	for k, va := range a {
		if vb, ok := b[k]; ok && !slices.ContainsFunc(va, func(v string) bool { return slices.Contains(vb, v) }) {
			return false
		}
	}
	return true
}

func orEmpty(l incident.Labels) incident.Labels {
	if l == nil {
		return incident.Labels{}
	}
	return l
}

func (l *Ledger) addRun(t *txn, r Run) error {
	failed, err := json.Marshal(orEmptyMap(r.Failed))
	if err != nil {
		return err
	}
	_, err = t.ExecContext(t.ctx, `INSERT INTO runs (kind, started, finished, period_from, period_to, firings, failed, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, r.Kind, r.Started.Unix(), r.Finished.Unix(), unixOr0(r.From), unixOr0(r.To),
		r.Firings, string(failed), r.Error)
	return err
}

// AddRun keeps a run that recorded nothing: one that failed, a digest, a
// review.
func (l *Ledger) AddRun(ctx context.Context, r Run) error {
	return l.write(ctx, func(t *txn) error { return l.addRun(t, r) })
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func unixOr0(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// severities joins a rule's severities, as verdict.Judge does.
func Severities(ss []string) string {
	var out []string
	for _, s := range ss {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return strings.Join(out, "/")
}

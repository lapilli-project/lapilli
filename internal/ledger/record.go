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
}

// Found is what a run found over the period [From, To].
type Found struct {
	From, To time.Time
	// Since is, by rule, how long before From its replay started, so as to
	// see a firing's pending time; From for a rule not in it. A firing that
	// started before From continues one the ledger has.
	Since map[string]time.Time
	// Rules are the rules the run knew of.
	Rules []Rule
	// Keep are rules the run couldn't replay this time, a query failing or
	// time running out: their firings are kept as they were.
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

// Run is one run, kept so the runs can be looked back on.
type Run struct {
	ID                int64
	Kind              string // backfill, hourly, review, digest, judge
	Started, Finished time.Time
	From, To          time.Time
	Firings           int
	Failed            map[string]string // rule to why it couldn't be replayed
	Error             string
}

// Record puts what a run found in the ledger, in place of what earlier runs
// found from From on, gives the stones again where they may have changed,
// and moves the cursor to To. It all happens, or none of it does.
func (l *Ledger) Record(ctx context.Context, f Found, o verdict.Options) error {
	return l.write(ctx, func(t *txn) error {
		if err := l.rules(t, f.Rules); err != nil {
			return err
		}
		from, to := f.From.Unix(), f.To.Unix()
		lo := from
		// Firings that started before From: an alert the ledger has as
		// still on, or that started in the margin and was found before.
		continued := map[int64]bool{}
		for _, x := range f.Firings {
			if !x.Start.Before(f.From) || f.Keep[x.Rule] {
				continue
			}
			since := f.From
			if s, ok := f.Since[x.Rule]; ok {
				since = s
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
			// The same firing, found before; or one the replay saw only from
			// where it started, already on.
			err = t.QueryRowContext(t.ctx, `SELECT id, start_at FROM firings WHERE series_id = ? AND start_at <= ?
				AND (start_at = ? OR end_at IS NULL OR end_at >= ?) ORDER BY start_at = ? DESC, start_at DESC LIMIT 1`,
				sid, x.Start.Unix(), x.Start.Unix(), since.Unix(), x.Start.Unix()).Scan(&id, &start)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				if id, err = l.insert(t, rule, sid, x); err != nil {
					return err
				}
				lo = min(lo, x.Start.Unix())
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
		// before the replay started.
		rows, err := t.QueryContext(t.ctx, `SELECT f.id, r.name FROM firings f JOIN rules r ON r.id = f.rule_id
			WHERE f.end_at IS NULL AND f.start_at < ?`, from)
		if err != nil {
			return err
		}
		ended := map[int64]int64{}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return err
			}
			if continued[id] || f.Keep[name] {
				continue
			}
			since := f.From
			if s, ok := f.Since[name]; ok {
				since = s
			}
			ended[id] = since.Unix()
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for id, at := range ended {
			if _, err := t.ExecContext(t.ctx, "UPDATE firings SET end_at = max(start_at, ?) WHERE id = ?", at, id); err != nil {
				return err
			}
		}

		// From From on, the run's firings replace what was there.
		keep := make([]string, 0, len(f.Keep))
		for name := range f.Keep {
			keep = append(keep, name)
		}
		slices.Sort(keep)
		keepJSON, _ := json.Marshal(keep)
		if _, err := t.ExecContext(t.ctx, `DELETE FROM firings WHERE start_at >= ? AND start_at <= ?
			AND rule_id NOT IN (SELECT r.id FROM rules r, json_each(?) k WHERE r.name = k.value)`, from, to, string(keepJSON)); err != nil {
			return err
		}
		n := 0
		for _, x := range f.Firings {
			if x.Start.Before(f.From) || x.Start.After(f.To) || f.Keep[x.Rule] {
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
			if _, err := l.insert(t, rule, sid, x); err != nil {
				return err
			}
			n++
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
		f.Run.Firings = n
		if err := l.addRun(t, f.Run); err != nil {
			return err
		}
		return setMeta(t.ctx, t, "cursor", strconv.FormatInt(to, 10))
	})
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
		if _, err := t.ExecContext(t.ctx, "UPDATE rules SET grp = ?, severity = ?, unsupported = ? WHERE id = ?",
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
		start, end int64
		state      string
		labels     string
	}
	rows, err := t.QueryContext(t.ctx, "SELECT start_at, end_at, state, labels FROM incidents WHERE origin = 'impact' AND start_at >= ? AND state != ''", from)
	if err != nil {
		return 0, err
	}
	var decided []old
	for rows.Next() {
		var o old
		if err := rows.Scan(&o.start, &o.end, &o.state, &o.labels); err != nil {
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
		state := ""
		for _, d := range decided {
			if d.start <= in.End.Unix() && d.end >= in.Start.Unix() && d.labels == string(labels) {
				state = d.state
			}
		}
		key := "impact-" + in.Start.UTC().Format("20060102T1504Z")
		if _, err := t.ExecContext(t.ctx, `INSERT INTO incidents (key, origin, state, counts, title, start_at, end_at, labels, updated)
			VALUES (?, 'impact', ?, 0, ?, ?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`,
			key, state, in.Title, in.Start.Unix(), in.End.Unix(), string(labels), now); err != nil {
			return 0, err
		}
	}
	return lo, nil
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

package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/verdict"
)

// The replay beside what really fired, kept (docs/design/fidelity.md):
// lapilli serve checks a day once its firings are settled, and keeps what it
// found, by rule and day.

// maxMisses is how many firings found only one way are kept for each rule.
const maxMisses = 20

// Check is a rule's replay beside what really fired over some days.
type Check struct {
	Replayed int `json:"replayed"`
	Real     int `json:"real"`
	Alike    int `json:"alike"`
	// Off is how far apart the alike ones started, on average.
	Off time.Duration `json:"off_ns"`
	// Alert is the name what really fired goes by; Shared, that other rules
	// have it too, and these are the counts of them all.
	Alert  string `json:"alert,omitempty"`
	Shared bool   `json:"shared,omitempty"`
}

// SumChecks adds up the checks of the rules in says, counting the rules that
// share an alert once; differ are those that differ.
func SumChecks(cs map[string]Check, in func(string) bool, n int, ratio float64) (all Check, differ []string) {
	seen := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(cs)) {
		c := cs[name]
		if !in(name) {
			continue
		}
		if c.Differs(n, ratio) {
			differ = append(differ, name)
		}
		if c.Shared {
			if seen[c.Alert] {
				continue
			}
			seen[c.Alert] = true
		}
		all.Replayed += c.Replayed
		all.Real += c.Real
		all.Alike += c.Alike
	}
	return all, differ
}

// Either is how many firings there were either way: the alike ones once.
func (c Check) Either() int { return c.Replayed + c.Real - c.Alike }

// Share is the alike ones' share of those found either way; 1 for none.
func (c Check) Share() float64 {
	if c.Either() == 0 {
		return 1
	}
	return float64(c.Alike) / float64(c.Either())
}

// Differs reports whether, with at least n firings either way, fewer than
// ratio of them were alike.
func (c Check) Differs(n int, ratio float64) bool {
	return c.Either() >= n && c.Share() < ratio
}

// Miss is a firing found only one way.
type Miss struct {
	Way    string            `json:"way"`
	Start  time.Time         `json:"start"`
	Labels map[string]string `json:"labels"`
}

// ReplayedFirings are the firings of rules the ledger replays (not those
// copied in from elsewhere) that start in [from, to).
func (l *Ledger) ReplayedFirings(ctx context.Context, from, to time.Time) ([]firing.Firing, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT r.name, s.labels, f.start_at, f.end_at FROM firings f
		JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id
		WHERE f.start_at >= ? AND f.start_at < ? AND r.source = ''`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []firing.Firing
	for rows.Next() {
		var f firing.Firing
		var labels string
		var start int64
		var end *int64
		if err := rows.Scan(&f.Rule, &labels, &start, &end); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(labels), &f.Labels); err != nil {
			return nil, err
		}
		f.Start = unix(start)
		if end != nil {
			f.End = unix(*end)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FailedRules are the rules a run meeting [from, to) couldn't replay: what
// the ledger has of them then isn't the replay to check.
func (l *Ledger) FailedRules(ctx context.Context, from, to time.Time) (map[string]bool, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT failed FROM runs WHERE period_from < ? AND period_to > ? AND failed != '{}'", to.Unix(), from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, err
		}
		for k := range m {
			out[k] = true
		}
	}
	return out, rows.Err()
}

// CheckedTo is how far the check has got; zero before it began.
func (l *Ledger) CheckedTo(ctx context.Context) (time.Time, error) {
	v, err := l.meta(ctx, l.r, "checked_to")
	if err != nil || v == "" {
		return time.Time{}, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return unix(n), err
}

// PutCheck keeps what checking the day starting at midnight found, for the
// rules checked, in place of anything kept for them that day, and notes the
// check has got to to. c is by the name what really fired goes by; rules
// are the names the ledger judges the rules under, for each. A rule left
// out of c wasn't checked: its day stays as it was. Nothing a page shows is
// judged differently.
func (l *Ledger) PutCheck(ctx context.Context, midnight time.Time, c *verdict.SpanCheck, rules map[string][]string, to time.Time) error {
	day := l.day(midnight.Unix())
	return l.write(ctx, func(t *txn) error {
		put, err := t.PrepareContext(t.ctx, `INSERT INTO checked (rule, day, alert, shared, replayed, real, alike, off_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (rule, day) DO UPDATE SET alert = excluded.alert, shared = excluded.shared,
				replayed = excluded.replayed, real = excluded.real, alike = excluded.alike, off_ms = excluded.off_ms`)
		if err != nil {
			return err
		}
		defer put.Close()
		miss, err := t.PrepareContext(t.ctx, "INSERT INTO check_misses (rule, way, start_at, labels) VALUES (?, ?, ?, ?)")
		if err != nil {
			return err
		}
		defer miss.Close()
		trim, err := t.PrepareContext(t.ctx, `DELETE FROM check_misses WHERE rule = ?1 AND id NOT IN
			(SELECT id FROM check_misses WHERE rule = ?1 ORDER BY start_at DESC, id DESC LIMIT ?2)`)
		if err != nil {
			return err
		}
		defer trim.Close()
		for _, alert := range slices.Sorted(maps.Keys(c.Rules)) {
			r := c.Rules[alert]
			names := rules[alert]
			if len(names) == 0 {
				names = []string{alert}
			}
			for _, name := range names {
				if _, err := put.ExecContext(t.ctx, name, day, alert, len(names) > 1, r.Replayed, r.Real, r.Alike, r.OffSum.Milliseconds()); err != nil {
					return err
				}
				if len(r.Misses) == 0 {
					continue
				}
				for _, m := range r.Misses {
					labels, err := json.Marshal(orEmptyMap(m.Labels))
					if err != nil {
						return err
					}
					if _, err := miss.ExecContext(t.ctx, name, m.Way, m.Start.Unix(), string(labels)); err != nil {
						return err
					}
				}
				if _, err := trim.ExecContext(t.ctx, name, maxMisses); err != nil {
					return err
				}
			}
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM check_elsewhere WHERE day = ?", day); err != nil {
			return err
		}
		for name, n := range c.Elsewhere {
			if _, err := t.ExecContext(t.ctx, "INSERT INTO check_elsewhere (name, day, firings) VALUES (?, ?, ?)", name, day, n); err != nil {
				return err
			}
		}
		return setMeta(t.ctx, t, "checked_to", strconv.FormatInt(to.Unix(), 10))
	})
}

// SkipCheck notes the check has got to to without keeping anything: a day
// that couldn't be checked.
func (l *Ledger) SkipCheck(ctx context.Context, to time.Time) error {
	return l.writeAside(ctx, func(t *txn) error {
		return setMeta(t.ctx, t, "checked_to", strconv.FormatInt(to.Unix(), 10))
	})
}

// Checks are each rule's replay beside what really fired over the days
// [from, to) falls on, by rule.
func (l *Ledger) Checks(ctx context.Context, from, to time.Time) (map[string]Check, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT rule, max(alert), max(shared), sum(replayed), sum(real), sum(alike), sum(off_ms) FROM checked
		WHERE day >= ? AND day < ? GROUP BY rule`, l.day(from.Unix()), l.day(to.Unix()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Check{}
	for rows.Next() {
		var name string
		var c Check
		var off int64
		if err := rows.Scan(&name, &c.Alert, &c.Shared, &c.Replayed, &c.Real, &c.Alike, &off); err != nil {
			return nil, err
		}
		if c.Alike > 0 {
			c.Off = time.Duration(off/int64(c.Alike)) * time.Millisecond
		}
		out[name] = c
	}
	return out, rows.Err()
}

// CheckMisses are the last firings of a rule found only one way, the latest
// first.
func (l *Ledger) CheckMisses(ctx context.Context, rule string) ([]Miss, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT way, start_at, labels FROM check_misses WHERE rule = ? ORDER BY start_at DESC, id DESC", rule)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Miss
	for rows.Next() {
		var m Miss
		var start int64
		var labels string
		if err := rows.Scan(&m.Way, &start, &labels); err != nil {
			return nil, err
		}
		m.Start = unix(start)
		if err := json.Unmarshal([]byte(labels), &m.Labels); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CheckElsewhere are the alerts that really fired over the days [from, to)
// falls on that no rule given is named, with how many times.
func (l *Ledger) CheckElsewhere(ctx context.Context, from, to time.Time) (map[string]int, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT name, sum(firings) FROM check_elsewhere WHERE day >= ? AND day < ? GROUP BY name",
		l.day(from.Unix()), l.day(to.Unix()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

// RunsFrom is the earliest time a run replayed from; zero before the first.
func (l *Ledger) RunsFrom(ctx context.Context) (time.Time, error) {
	var t sql.NullInt64
	err := l.r.QueryRowContext(ctx, "SELECT min(period_from) FROM runs WHERE error = '' AND period_from > 0 AND kind IN ('backfill', 'hourly')").Scan(&t)
	return fromNull(t), err
}

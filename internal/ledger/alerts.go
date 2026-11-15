package ledger

import (
	"context"
	"time"
)

// AlertCount is an alert, a rule and its labels, and how many times it
// fired in a span, those a quiet window excused aside.
type AlertCount struct {
	Rule    string            `json:"rule"`
	Labels  map[string]string `json:"labels"`
	Firings int               `json:"firings"`
}

// AlertCounts are the alerts that fired most in [from, to), limit of them,
// each with how many times, the most first, as one read of the span's
// firings counts them by alert; and, by rule, all its firings. The weekly
// digest reads them once a week.
func (l *Ledger) AlertCounts(ctx context.Context, from, to time.Time, limit int) ([]AlertCount, map[string]int, error) {
	out, err := l.alertCounts(ctx, `SELECT r.name, s.id, s.labels, x.n FROM (SELECT series_id, count(*) - sum(excused) AS n FROM firings
		WHERE start_at >= ? AND start_at < ? GROUP BY series_id) x JOIN series s ON s.id = x.series_id JOIN rules r ON r.id = s.rule_id
		WHERE x.n > 0 ORDER BY x.n DESC, s.id LIMIT ?`, from.Unix(), to.Unix(), l.d.limit(limit))
	if err != nil {
		return nil, nil, err
	}
	rows, err := l.r.QueryContext(ctx, `SELECT r.name, x.n FROM (SELECT rule_id, count(*) - sum(excused) AS n FROM firings
		WHERE start_at >= ? AND start_at < ? GROUP BY rule_id) x JOIN rules r ON r.id = x.rule_id WHERE x.n > 0`, from.Unix(), to.Unix())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	all := map[string]int{}
	for rows.Next() {
		var rule string
		var n int
		if err := rows.Scan(&rule, &n); err != nil {
			return nil, nil, err
		}
		all[rule] = n
	}
	return out, all, rows.Err()
}

// RuleAlerts are a rule's alerts that caught nothing in [from, to), those
// a quiet window excused aside, that fired most, limit of them, with how
// many times, the most first; and how many such alerts there were.
func (l *Ledger) RuleAlerts(ctx context.Context, rule string, from, to time.Time, limit int) ([]AlertCount, int, error) {
	const black = `FROM firings WHERE rule_id = (SELECT id FROM rules WHERE name = ?) AND start_at >= ? AND start_at < ? AND white = 0 AND excused = 0`
	out, err := l.alertCounts(ctx, `SELECT r.name, s.id, s.labels, x.n FROM (SELECT series_id, count(*) AS n `+black+`
		GROUP BY series_id) x JOIN series s ON s.id = x.series_id JOIN rules r ON r.id = s.rule_id ORDER BY x.n DESC, s.id LIMIT ?`,
		rule, from.Unix(), to.Unix(), l.d.limit(limit))
	if err != nil {
		return nil, 0, err
	}
	var n int
	err = l.r.QueryRowContext(ctx, "SELECT count(DISTINCT series_id) "+black, rule, from.Unix(), to.Unix()).Scan(&n)
	return out, n, err
}

func (l *Ledger) alertCounts(ctx context.Context, q string, args ...any) ([]AlertCount, error) {
	rows, err := l.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertCount
	for rows.Next() {
		var a AlertCount
		var sid int64
		var labels string
		if err := rows.Scan(&a.Rule, &sid, &labels, &a.Firings); err != nil {
			return nil, err
		}
		if a.Labels, err = l.labels(sid, labels); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

package ledger

import (
	"context"
	"database/sql"
	"time"
)

// Refired is an alert, a rule and its labels, and how many of its firings
// re-fired in a span: started within --refire of its last resolving.
type Refired struct {
	Rule    string            `json:"rule"`
	Labels  map[string]string `json:"labels"`
	Refired int               `json:"refired"`
}

// RefiredOf are a rule's alerts that re-fired in [from, to), the most first,
// limit of them.
func (l *Ledger) RefiredOf(ctx context.Context, rule string, from, to time.Time, limit int) ([]Refired, error) {
	return l.refired(ctx, `SELECT r.name, s.id, s.labels, x.n FROM (SELECT series_id, count(*) AS n FROM firings
		WHERE rule_id = (SELECT id FROM rules WHERE name = ?) AND refired = 1 AND excused = 0 AND start_at >= ? AND start_at < ?
		GROUP BY series_id) x JOIN series s ON s.id = x.series_id JOIN rules r ON r.id = s.rule_id
		ORDER BY x.n DESC, s.id LIMIT ?`, rule, from.Unix(), to.Unix(), l.d.limit(limit))
}

// MostRefired are the alerts, of any rule, that re-fired most in [from,
// to), the most first, limit of them; of the rules in only, when given.
func (l *Ledger) MostRefired(ctx context.Context, from, to time.Time, limit int, only func(rule string) bool) ([]Refired, error) {
	n := limit
	if only != nil {
		n = -1 // every one, to keep those of the rules asked for
	}
	all, err := l.refired(ctx, `SELECT r.name, s.id, s.labels, x.n FROM (SELECT series_id, count(*) AS n FROM firings
		WHERE start_at >= ? AND start_at < ? AND refired = 1 AND excused = 0 GROUP BY series_id) x
		JOIN series s ON s.id = x.series_id JOIN rules r ON r.id = s.rule_id ORDER BY x.n DESC, r.name, s.id LIMIT ?`, from.Unix(), to.Unix(), l.d.limit(n))
	if err != nil || only == nil {
		return all, err
	}
	var out []Refired
	for _, x := range all {
		if only(x.Rule) {
			out = append(out, x)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (l *Ledger) refired(ctx context.Context, q string, args ...any) ([]Refired, error) {
	rows, err := l.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Refired
	for rows.Next() {
		var x Refired
		var sid int64
		var labels string
		if err := rows.Scan(&x.Rule, &sid, &labels, &x.Refired); err != nil {
			return nil, err
		}
		if x.Labels, err = l.labels(sid, labels); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// LastNotified is when the latest notification kept came, by the source it
// came from (alertmanager, grafana, sns and the rest).
func (l *Ledger) LastNotified(ctx context.Context) (map[string]time.Time, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT source, max(at) FROM notifications GROUP BY source")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var src string
		var at int64
		if err := rows.Scan(&src, &at); err != nil {
			return nil, err
		}
		out[src] = unix(at)
	}
	return out, rows.Err()
}

// LastDeploy is when the latest deploy kept that started by now started;
// zero for none.
func (l *Ledger) LastDeploy(ctx context.Context, now time.Time) (time.Time, error) {
	var at sql.NullInt64
	err := l.r.QueryRowContext(ctx, "SELECT max(start_at) FROM deploys WHERE start_at <= ?", now.Unix()).Scan(&at)
	return fromNull(at), err
}

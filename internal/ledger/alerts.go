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

// AlertCounts are the alerts that fired in [from, to), each with how many
// times: by rule, the most first. One read of the span's firings, grouped
// by alert, as the weekly digest reads them once a week.
func (l *Ledger) AlertCounts(ctx context.Context, from, to time.Time) ([]AlertCount, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT r.name, s.id, s.labels, x.n FROM (SELECT series_id, count(*) - sum(excused) AS n FROM firings
		WHERE start_at >= ? AND start_at < ? GROUP BY series_id) x JOIN series s ON s.id = x.series_id JOIN rules r ON r.id = s.rule_id
		WHERE x.n > 0 ORDER BY r.name, x.n DESC, s.id`, from.Unix(), to.Unix())
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

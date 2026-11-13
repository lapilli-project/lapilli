package ledger

import (
	"context"
	"database/sql"
	"time"
)

// LabelNames are the label names the alerts of each replayed rule had, by
// the rule's name: of every alert that fired since from, or is firing
// still. They are what a rule's templates can name (docs/design/content.md).
// A rule that fired nothing since is left out.
func (l *Ledger) LabelNames(ctx context.Context, from time.Time) (map[string]map[string]bool, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT r.name, j.key FROM rules r JOIN series s ON s.rule_id = r.id `+l.d.leftKeys("s.labels", "j")+`
		WHERE r.source = '' AND (EXISTS (SELECT 1 FROM firings f WHERE f.series_id = s.id AND f.start_at >= ?)
			OR EXISTS (SELECT 1 FROM firings f WHERE f.series_id = s.id AND f.end_at IS NULL))
		GROUP BY r.name, j.key`, from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]bool{}
	for rows.Next() {
		var rule string
		var name sql.NullString // an alert with no labels
		if err := rows.Scan(&rule, &name); err != nil {
			return nil, err
		}
		if out[rule] == nil {
			out[rule] = map[string]bool{}
		}
		if name.Valid {
			out[rule][name.String] = true
		}
	}
	return out, rows.Err()
}

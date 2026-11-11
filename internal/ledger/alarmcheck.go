package ledger

import (
	"context"
	"encoding/json"
	"time"

	"lapilli.dev/internal/firing"
)

// RecordedFirings are the firings copied in under a source that start in
// [from, to), each under the name the source sent it by, with its labels:
// what a cloud's alarm really fired, set beside its replay
// (docs/design/inputs.md).
func (l *Ledger) RecordedFirings(ctx context.Context, source string, from, to time.Time) ([]firing.Firing, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT r.sent, s.labels, f.start_at, f.end_at FROM firings f
		JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id
		WHERE f.start_at >= ? AND f.start_at < ? AND r.source = ?`, from.Unix(), to.Unix(), source)
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

// Covered reports whether a source's copies cover [from, to) without a
// gap.
func (l *Ledger) Covered(ctx context.Context, source string, from, to time.Time) (bool, error) {
	spans, err := l.spans(ctx, l.r, source)
	if err != nil {
		return false, err
	}
	for _, s := range spans {
		if s[0] <= from.Unix() && s[1] >= to.Unix() {
			return true, nil
		}
	}
	return false, nil
}

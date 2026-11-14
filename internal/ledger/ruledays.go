package ledger

import (
	"context"

	"lapilli.dev/internal/firing"
)

// What a rule's page marks its last days with, as the ledger has it: the
// days its firings came during a deploy, and the quiet windows that could
// excuse one of its firings at all.

// RuleDeploys are, by day, how many of a rule's firings that started on the
// days from first to last came during a deploy of what they are about, as
// each was judged (Deploy.During); the days with none are left out.
func (l *Ledger) RuleDeploys(ctx context.Context, rule string, first, last int64) (map[int64]int, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT d.day, d.deploy FROM days d JOIN rules r ON r.id = d.rule_id
		WHERE r.name = ? AND d.day >= ? AND d.day <= ? AND d.deploy > 0`, rule, first, last)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var day int64
		var n int
		if err := rows.Scan(&day, &n); err != nil {
			return nil, err
		}
		out[day] = n
	}
	return out, rows.Err()
}

// Applies reports whether the window could excuse a firing of rule with
// labels, whenever it started: its pattern, its labels and its team, as
// Excuses has them, when it falls due left out.
func (w Window) Applies(rule string, labels map[string]string) bool {
	return w.matches(firing.Firing{Rule: rule, Labels: labels, Start: w.Start})
}

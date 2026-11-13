package ledger

import (
	"context"
	"strings"
	"testing"
	"time"
)

// plan is how the ledger's database would run a query, as text: SQLite's
// EXPLAIN QUERY PLAN, or Postgres's EXPLAIN with sequential scans set
// aside, so that an index that can be used is (a test's few rows would be
// read whole otherwise).
func plan(t testing.TB, l *Ledger, q string, args ...any) string {
	t.Helper()
	ctx := context.Background()
	if l.d == postgres {
		tx, err := l.r.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
			t.Fatal(err)
		}
		rows, err := tx.QueryContext(ctx, "EXPLAIN "+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, strings.TrimSpace(s))
		}
		return strings.Join(lines, "; ")
	}
	rows, err := l.r.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, detail)
	}
	return strings.Join(lines, "; ")
}

// scansAll says a plan reads every row of a table.
func scansAll(l *Ledger, plan, table string) bool {
	if l.d == postgres {
		return strings.Contains(plan+";", "Seq Scan on "+table+" ") || strings.Contains(plan+";", "Seq Scan on "+table+";")
	}
	return strings.Contains(plan, "SCAN "+table)
}

// An alarm's id is found through series_alarm and notified_alarm, in
// either database, by the conditions the ledger asks: alarmIs, and those of
// alarmIDs.
func TestAlarmIDIndexed(t *testing.T) {
	l := open(t, time.UTC)
	s, d := l.d.text("s.labels", AlarmID), l.d.text("d.labels", AlarmID)
	for _, c := range []struct {
		q, index string
		args     []any
	}{
		{"SELECT s.id FROM series s WHERE " + alarmIs(l.d, s, "?1", "?1"), "series_alarm", []any{"ocid1.alarm.oc1..a"}},
		{"SELECT s.id FROM series s WHERE " + alarmIs(l.d, s, "?1", "?2"), "series_alarm", []any{"projects/p/alertPolicies/9", "alertPolicies/9"}},
		{"SELECT d.rule FROM notified d WHERE " + alarmIs(l.d, d, "?1", "?1") + " AND d.start_at >= ?2", "notified_alarm", []any{"ocid1.alarm.oc1..a", 0}},
		{"SELECT s.id FROM series s WHERE " + l.d.alarmPrefix(s) + " >= 'projects/' AND " + l.d.alarmPrefix(s) + " < 'projects0'", "series_alarm", nil},
		{"SELECT s.id FROM series s WHERE " + s + " IS NOT NULL AND " + alarmIn(l.d, s), "series_alarm", []any{`["ocid1.alarm.oc1..a", "b"]`}},
	} {
		q, index := c.q, c.index
		// In Postgres, searched by the index (Index Cond), not read whole
		// through it, which a partial index can be.
		p := plan(t, l, q, c.args...)
		searched := l.d != postgres
		for _, step := range strings.Split(p, "; ") {
			searched = searched || strings.HasPrefix(step, "Index Cond: ") && strings.Contains(step, "'alarm_id'")
		}
		if !strings.Contains(p, index) || !searched {
			t.Errorf("%s\nplan: %s", q, p)
		}
	}
}

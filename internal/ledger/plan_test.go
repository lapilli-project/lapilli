package ledger

import (
	"context"
	"strings"
	"testing"
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

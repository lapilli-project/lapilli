package ledger

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A notification whose alerts don't say when they started reads their
// starts with one query, bounded, through the group's index and then the
// notification's: not one for each alert, through every notification of
// its rule. It runs while the ledger is written.
func TestNotifyStartsBounded(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	// A noisy rule's year: 100,000 alerts told of, in other groups.
	err := l.write(ctx, func(tx *txn) error {
		for i := range 2000 {
			var id int64
			if err := tx.QueryRowContext(tx.ctx, `INSERT INTO notifications (at, source, receiver, grp, reason, digest) VALUES (?, 'sns', 'r', ?, 'first', ?) RETURNING id`,
				t0.Unix()-int64(i), fmt.Sprint("g-", i), fmt.Sprint("d-", i)).Scan(&id); err != nil {
				return err
			}
			for j := range 50 {
				if _, err := tx.ExecContext(tx.ctx, "INSERT INTO notified (notification_id, rule, labels, start_at, firing) VALUES (?, 'R', ?, ?, 1)",
					id, fmt.Sprintf(`{"i":"%d"}`, j), t0.Unix()-int64(i)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var alerts []Alert
	for j := range MaxAlerts {
		alerts = append(alerts, Alert{Rule: "R", Labels: map[string]string{"i": fmt.Sprint(j)}, Resolved: true})
	}
	began := time.Now()
	if _, err := l.Notify(ctx, Notification{At: t0, Source: "sns", Receiver: "r", Group: "g-0", Alerts: alerts}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 2*time.Second*time.Duration(slow) {
		t.Errorf("a notification of %d alerts with no start took %s", MaxAlerts, took)
	}
	// The plan: the group's index, then the notification's alerts.
	rows, err := l.r.QueryContext(ctx, "EXPLAIN QUERY PLAN "+qStarts, "sns", "r", "g-0", 1, 0, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var a, b, c int
		var d string
		rows.Scan(&a, &b, &c, &d)
		plan = append(plan, d)
	}
	rows.Close()
	if p := strings.Join(plan, "; "); !strings.Contains(p, "notifications_group") || !strings.Contains(p, "notified_of") || strings.Contains(p, "notified_rule") {
		t.Errorf("plan: %s", p)
	}
}

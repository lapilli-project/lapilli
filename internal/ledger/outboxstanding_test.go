package ledger

import (
	"context"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger/ledgertest"
)

// tell makes n notices to a place, each made at made, failed so many
// times; given up on past GiveUpAfter.
func tell(t *testing.T, l *Ledger, key, dest string, n, tries int, made time.Time) {
	t.Helper()
	ctx := context.Background()
	var before int64
	if err := l.w.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM outbox").Scan(&before); err != nil {
		t.Fatal(err)
	}
	told := make([]Told, n)
	for i := range told {
		told[i] = Told{Dest: dest, About: map[string]any{"i": i}}
	}
	if err := l.TellOnce(ctx, key, "1", "test.event", told); err != nil {
		t.Fatal(err)
	}
	next := time.Now().Add(time.Hour).Unix()
	if tries >= GiveUpAfter {
		next = givenUpAt
	}
	errText := ""
	if tries > 0 {
		errText = "down"
	}
	if _, err := l.w.ExecContext(ctx, "UPDATE outbox SET attempts = ?, next_at = ?, error = ?, created = ? WHERE id > ?",
		tries, next, errText, made.Unix(), before); err != nil {
		t.Fatal(err)
	}
}

// TestStandingBounded: a place's waiting notices and its given up are
// each looked through FailedCounted at most, more said apart; the failed
// and the given up made in the window are counted apart, so that a place
// given up on long ago, past the thousand, hides none of the day's
// failures; when the outbox last sent one is read whatever is unsent.
func TestStandingBounded(t *testing.T) {
	ctx := context.Background()
	day := time.Now().Add(-24 * time.Hour)
	l := open(t, time.UTC)
	tell(t, l, "failed", "webhook", FailedCounted+5, 1, time.Now())
	if _, err := l.w.ExecContext(ctx, "UPDATE outbox SET sent_at = ?, error = '' WHERE id = (SELECT min(id) FROM outbox)", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	st, err := l.Standing(ctx, []string{"webhook"}, day)
	if err != nil {
		t.Fatal(err)
	}
	if st.Failed != FailedCounted || !st.More || st.GivenUp != 0 || st.LastSent.IsZero() {
		t.Errorf("past %d failed: %+v, want %d failed, more, a last send", FailedCounted, st, FailedCounted)
	}

	// Given up on long ago, past the thousand, and a few failed today, and a
	// few given up today.
	l = open(t, time.UTC)
	tell(t, l, "old", "webhook", FailedCounted+1, GiveUpAfter, time.Now().Add(-72*time.Hour))
	tell(t, l, "today", "webhook", 2, 3, time.Now())
	tell(t, l, "lost", "webhook", 3, GiveUpAfter, time.Now())
	tell(t, l, "waits", "webhook", 4, 0, time.Now())
	st, err = l.Standing(ctx, []string{"webhook"}, day)
	if err != nil {
		t.Fatal(err)
	}
	if st.Failed != 2 || st.More || st.GivenUp != 3 || st.GivenUpMore {
		t.Errorf("a thousand given up long ago, 2 failed and 3 given up today: %+v", st)
	}
	// Given up today, past the thousand.
	tell(t, l, "lost-more", "slack", FailedCounted+1, GiveUpAfter, time.Now())
	if st, _ := l.Standing(ctx, []string{"webhook", "slack"}, day); st.GivenUp != 3+FailedCounted || !st.GivenUpMore || st.Failed != 2 {
		t.Errorf("past %d given up today: %+v", FailedCounted, st)
	}
}

// TestStandingReadsAnIndexRange: the waiting notices and the given up are
// each read by the outbox's index on their place, when each was sent and
// when it is next tried, a range of it backward from its end, in its order,
// LIMIT rows: no sort of every unsent notice of the place, on SQLite and on
// Postgres (run with LAPILLI_TEST_POSTGRES).
func TestStandingReadsAnIndexRange(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	for _, q := range []string{standingWaiting, standingGivenUp} {
		args := []any{0, "webhook", givenUpAt, FailedCounted + 1}
		var plan []string
		if ledgertest.On() {
			tx, err := l.w.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
				t.Fatal(err)
			}
			rows, err := tx.QueryContext(ctx, "EXPLAIN "+q, args...)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var s string
				rows.Scan(&s)
				plan = append(plan, s)
			}
			rows.Close()
			tx.Rollback()
		} else {
			rows, err := l.r.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, args...)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var id, parent, unused int
				var s string
				rows.Scan(&id, &parent, &unused, &s)
				plan = append(plan, s)
			}
			rows.Close()
		}
		text := strings.Join(plan, "\n")
		ranged := strings.Contains(text, "SEARCH outbox USING INDEX outbox_due") || strings.Contains(text, "Index Scan Backward using outbox_due")
		if !ranged || strings.Contains(text, "TEMP B-TREE") || strings.Contains(text, "Sort") {
			t.Errorf("not a range of outbox_due, in its order:\n%s\n%s", q, text)
		}
	}
}

package ledger

import (
	"context"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger/ledgertest"
)

// TestStandingBounded: a place's unsent notices are looked through
// FailedCounted at most, those past it said to be more, and those of them
// made in the window that failed counted; when the outbox last sent one is
// read whatever is unsent.
func TestStandingBounded(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	told := make([]Told, FailedCounted+5)
	for i := range told {
		told[i] = Told{Dest: []string{"webhook", "slack"}[i%2], About: map[string]any{"i": i}}
	}
	if err := l.TellOnce(ctx, "test-told", "1", "test.event", told); err != nil {
		t.Fatal(err)
	}
	if _, err := l.w.ExecContext(ctx, "UPDATE outbox SET error = 'down', attempts = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.w.ExecContext(ctx, "UPDATE outbox SET sent_at = ?, error = '' WHERE id = (SELECT min(id) FROM outbox)", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	st, err := l.Standing(ctx, []string{"webhook", "slack"}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if st.Failed != (FailedCounted+5)/2+(FailedCounted+5)/2 || st.More || st.LastSent.IsZero() {
		t.Errorf("standing %+v, want %d failed, no more, and a last send", st, FailedCounted+4)
	}
	// One place with more unsent than are looked through.
	more := make([]Told, FailedCounted)
	for i := range more {
		more[i] = Told{Dest: "slack", About: map[string]any{"i": i}}
	}
	if err := l.TellOnce(ctx, "test-told-2", "1", "test.event", more); err != nil {
		t.Fatal(err)
	}
	st, err = l.Standing(ctx, []string{"slack"}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !st.More || st.Failed > FailedCounted {
		t.Errorf("past %d unsent: %+v", FailedCounted, st)
	}
	if st, _ := l.Standing(ctx, []string{"webhook"}, time.Now().Add(time.Hour)); st.Failed != 0 || st.More {
		t.Errorf("none made since: %+v", st)
	}
	// A place given up on long ago, past FailedCounted, and a few failed
	// in the window: those looked through are the given up, the latest to
	// be tried, and none of the window's is counted under them.
	l = open(t, time.UTC)
	old := make([]Told, FailedCounted+1)
	for i := range old {
		old[i] = Told{Dest: "webhook", About: map[string]any{"i": i}}
	}
	if err := l.TellOnce(ctx, "test-old", "1", "test.event", old); err != nil {
		t.Fatal(err)
	}
	if _, err := l.w.ExecContext(ctx, "UPDATE outbox SET error = 'down', attempts = ?, next_at = ?, created = ?", GiveUpAfter, int64(1)<<40, time.Now().Add(-48*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := l.TellOnce(ctx, "test-new", "1", "test.event", []Told{{Dest: "webhook", About: map[string]any{}}, {Dest: "webhook", About: map[string]any{}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.w.ExecContext(ctx, "UPDATE outbox SET error = 'down', attempts = 1 WHERE attempts = 0"); err != nil {
		t.Fatal(err)
	}
	if st, _ := l.Standing(ctx, []string{"webhook"}, time.Now().Add(-time.Hour)); st.Failed != 0 || !st.More {
		t.Errorf("past %d given up, a few failed since: %+v, want none counted and more", FailedCounted, st)
	}
}

// TestStandingReadsAnIndexRange: the unsent notices are read by the
// outbox's index on their place, when each was sent and when it is next
// tried, a range of it from its end, LIMIT rows, not by reading every unsent
// notice of the place and counting them.
func TestStandingReadsAnIndexRange(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	var plan []string
	if ledgertest.On() {
		tx, err := l.w.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
			t.Fatal(err)
		}
		rows, err := tx.QueryContext(ctx, "EXPLAIN "+standingUnsent, 0, "webhook", FailedCounted+1)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			rows.Scan(&s)
			plan = append(plan, s)
		}
		rows.Close()
	} else {
		rows, err := l.r.QueryContext(ctx, "EXPLAIN QUERY PLAN "+standingUnsent, 0, "webhook", FailedCounted+1)
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
	if !strings.Contains(text, "outbox_due") || strings.Contains(text, "SCAN outbox\n") || strings.HasSuffix(text, "SCAN outbox") ||
		strings.Contains(text, "TEMP B-TREE") || strings.Contains(text, "Sort") {
		t.Errorf("the unsent notices aren't read by a range of outbox_due, in its order:\n%s", text)
	}
}

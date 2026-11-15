package ledger

import (
	"context"
	"testing"
	"time"
)

// TestStandingBounded: how many notices failed is counted FailedCounted at
// most, the rest said to be more, over every place asked of; when the
// outbox last sent one is read whatever is unsent.
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
	if st.Failed != FailedCounted || !st.More || st.LastSent.IsZero() {
		t.Errorf("standing %+v, want %d failed, more, and a last send", st, FailedCounted)
	}
	st, err = l.Standing(ctx, []string{"slack"}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if st.Failed != (FailedCounted+5)/2 || st.More {
		t.Errorf("one place: %+v, want %d failed", st, (FailedCounted+5)/2)
	}
	if st, _ := l.Standing(ctx, []string{"webhook"}, time.Now().Add(time.Hour)); st.Failed != 0 || st.More {
		t.Errorf("none made since: %+v", st)
	}
}

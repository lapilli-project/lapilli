package ledger

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// Hours already replayed are noted to replay again from the earliest time
// asked, and their days to check again; hours not replayed yet need
// nothing. A run that replayed from there forgets it, unless an earlier
// time was noted since.
func TestAgain(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	cursor := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	if err := l.write(ctx, func(tx *txn) error {
		if err := setMeta(ctx, tx, "cursor", strconv.FormatInt(cursor.Unix(), 10)); err != nil {
			return err
		}
		return setMeta(ctx, tx, "checked_to", strconv.FormatInt(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC).Unix(), 10))
	}); err != nil {
		t.Fatal(err)
	}
	from := func() time.Time {
		at, err := l.AgainFrom(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	if err := l.Again(ctx, cursor.Add(time.Hour)); err != nil || !from().IsZero() {
		t.Errorf("hours not replayed yet: %v %v", from(), err)
	}
	at := time.Date(2026, 11, 1, 15, 30, 0, 0, time.UTC)
	if err := l.Again(ctx, at); err != nil || !from().Equal(at) {
		t.Errorf("again from %v, want %v: %v", from(), at, err)
	}
	if checked, err := l.CheckedTo(ctx); err != nil || !checked.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("checked to %v, want the start of its day: %v", checked, err)
	}
	if err := l.Again(ctx, at.Add(2*time.Hour)); err != nil || !from().Equal(at) {
		t.Errorf("a later time: again from %v", from())
	}
	if checked, _ := l.CheckedTo(ctx); !checked.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("a later time moved the check to %v", checked)
	}
	if err := l.AgainDone(ctx, at.Add(2*time.Hour)); err != nil || !from().Equal(at) {
		t.Errorf("done with another time: %v", from())
	}
	if err := l.AgainDone(ctx, at); err != nil || !from().IsZero() {
		t.Errorf("done: %v", from())
	}
}

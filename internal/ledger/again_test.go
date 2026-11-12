package ledger

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/verdict"
)

// Hours are noted to replay again from the earliest time asked, and their
// days to check again. A run that replayed from there forgets it, unless a
// version was noted since the run read it: the version the watcher records
// while the worker's run, begun before it, is under way.
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
	from := func() (time.Time, int64) {
		at, seq, err := l.AgainFrom(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return at, seq
	}
	if at, seq := from(); !at.IsZero() || seq != 0 {
		t.Errorf("nothing noted: %v %d", at, seq)
	}
	at := time.Date(2026, 11, 1, 15, 30, 0, 0, time.UTC)
	if err := l.Again(ctx, at); err != nil {
		t.Fatal(err)
	}
	if got, _ := from(); !got.Equal(at) {
		t.Errorf("again from %v, want %v", got, at)
	}
	if checked, err := l.CheckedTo(ctx); err != nil || !checked.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("checked to %v, want the start of its day: %v", checked, err)
	}
	// A run reads it; meanwhile a later version is noted: the earlier time
	// stands, and the run's done forgets nothing.
	begun, seq := from()
	if err := l.Again(ctx, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := from(); !got.Equal(begun) {
		t.Errorf("a later time: again from %v", got)
	}
	if err := l.AgainDone(ctx, seq); err != nil {
		t.Fatal(err)
	}
	if got, _ := from(); !got.Equal(at) {
		t.Errorf("done, with a version noted since the run began: again from %v, want %v kept", got, at)
	}
	// The next run reads it again, and is done with it.
	_, seq = from()
	if err := l.AgainDone(ctx, seq); err != nil {
		t.Fatal(err)
	}
	if got, _ := from(); !got.IsZero() {
		t.Errorf("done: %v", got)
	}
	// Hours not replayed yet are noted too: a run under way may be
	// replaying them with the rules before.
	if err := l.Again(ctx, cursor.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := from(); !got.Equal(cursor.Add(time.Hour)) {
		t.Errorf("hours not replayed yet: %v", got)
	}
}

// When Again sends the check back, what was kept of those days is gone with
// it, in the same write: a rule the new version dropped keeps no day of the
// old one's, and a miss checked again isn't kept twice. A check that began
// before a version was noted keeps nothing.
func TestAgainForgetsChecks(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	day1, day2 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	check := func(rule string, at time.Time) *verdict.SpanCheck {
		return &verdict.SpanCheck{Rules: map[string]*verdict.SpanRule{rule: {Replayed: 1, Real: 2,
			Misses: []verdict.Miss{{Way: "real", Firing: firing.Firing{Rule: rule, Start: at.Add(time.Hour), Labels: map[string]string{"service": "a"}}}}}},
			Elsewhere: map[string]int{"Other": 1}}
	}
	_, seq, _ := l.AgainFrom(ctx)
	for _, d := range []time.Time{day1, day2} {
		if err := l.PutCheck(ctx, d, check("Busy", d), nil, d.Add(24*time.Hour), seq); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.write(ctx, func(tx *txn) error {
		return setMeta(ctx, tx, "cursor", strconv.FormatInt(day2.Add(48*time.Hour).Unix(), 10))
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.Again(ctx, day2.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	cs, err := l.Checks(ctx, day1, day2.Add(24*time.Hour))
	if err != nil || cs["Busy"].Replayed != 1 {
		t.Errorf("checks %+v %v: want day 1's alone", cs, err)
	}
	ms, err := l.CheckMisses(ctx, "Busy")
	if err != nil || len(ms) != 1 || !ms[0].Start.Before(day2) {
		t.Errorf("misses %+v %v: want day 1's alone", ms, err)
	}
	var elsewhere int
	if err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM check_elsewhere WHERE day >= ?", l.day(day2.Unix())).Scan(&elsewhere); err != nil || elsewhere != 0 {
		t.Errorf("check_elsewhere from day 2: %d %v", elsewhere, err)
	}
	// A check begun before (seq) keeps nothing, nor moves on.
	if err := l.PutCheck(ctx, day2, check("Busy", day2), nil, day2.Add(24*time.Hour), seq); !errors.Is(err, ErrAgain) {
		t.Errorf("a check begun before: %v", err)
	}
	if err := l.SkipCheck(ctx, day2.Add(24*time.Hour), seq); !errors.Is(err, ErrAgain) {
		t.Errorf("a skip begun before: %v", err)
	}
	if checked, _ := l.CheckedTo(ctx); !checked.Equal(day2) {
		t.Errorf("checked to %v, want %v", checked, day2)
	}
	_, now, _ := l.AgainFrom(ctx)
	if err := l.PutCheck(ctx, day2, check("Errors", day2), nil, day2.Add(24*time.Hour), now); err != nil {
		t.Errorf("a check begun since: %v", err)
	}
}

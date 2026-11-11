package ledger

import (
	"context"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/verdict"
)

// A schedule's turns are kept as its feed has them: what overlaps or names
// no one who shows, or names them in more bytes than MaxWho, is refused;
// the same again changes nothing; more than MaxTurns from one read are
// refused, and at most MaxTurns are kept, the oldest let go, a read of
// MaxTurns turns kept whole, so the same again changes nothing; --keep
// prunes those over.
func TestTurns(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := func(h int) time.Time {
		return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(h) * time.Hour)
	}
	for name, turns := range map[string][]verdict.RotaTurn{
		"overlapping":       {{From: at(0), To: at(2), Who: "a"}, {From: at(1), To: at(3), Who: "b"}},
		"of no length":      {{From: at(0), To: at(0), Who: "a"}},
		"no one":            {{From: at(0), To: at(1), Who: ""}},
		"a name that hides": {{From: at(0), To: at(1), Who: "Jane\u202eDoe"}},
		"in 1960":           {{From: time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC), To: at(1), Who: "a"}},
		"a name too long":   {{From: at(0), To: at(1), Who: strings.Repeat("\u00e9", MaxWho/2+1)}},
	} {
		if _, err := l.PutTurns(ctx, "s", firstOf(turns), turns); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	turns := []verdict.RotaTurn{{From: at(0), To: at(1), Who: "a"}, {From: at(1), To: at(2), Who: "b"}}
	if changed, err := l.PutTurns(ctx, "s", firstOf(turns), turns); err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	gen := l.Gen()
	if changed, err := l.PutTurns(ctx, "s", firstOf(turns), turns); err != nil || changed || l.Gen() != gen {
		t.Errorf("the same again: changed %v, %v", changed, err)
	}
	if changed, _ := l.PutTurns(ctx, "s", firstOf(nil), nil); changed {
		t.Error("no turns changed something")
	}
	if _, err := l.PutTurns(ctx, "s", at(0), []verdict.RotaTurn{{From: at(0), To: at(1), Who: strings.Repeat("\u00e9", MaxWho/2)}}); err != nil {
		t.Errorf("a name of MaxWho bytes: %v", err)
	}
	var many []verdict.RotaTurn
	for i := range MaxTurns + 1 {
		many = append(many, verdict.RotaTurn{From: at(10 + i), To: at(11 + i), Who: "c"})
	}
	if _, err := l.PutTurns(ctx, "s", firstOf(many), many); err == nil {
		t.Error("more than MaxTurns from one read: taken")
	}
	many = many[:MaxTurns]
	if _, err := l.PutTurns(ctx, "s", firstOf(many), many); err != nil {
		t.Fatal(err)
	}
	if changed, err := l.PutTurns(ctx, "s", firstOf(many), many); err != nil || changed {
		t.Errorf("MaxTurns, the same again: changed %v, %v", changed, err)
	}
	var later []verdict.RotaTurn
	for i := range 5 {
		later = append(later, verdict.RotaTurn{From: at(10 + MaxTurns + i), To: at(11 + MaxTurns + i), Who: "d"})
	}
	if _, err := l.PutTurns(ctx, "s", firstOf(later), later); err != nil {
		t.Fatal(err)
	}
	kept, err := l.Turns(ctx, "s")
	if err != nil || len(kept) != MaxTurns || !kept[len(kept)-1].To.Equal(at(10+MaxTurns+5)) {
		t.Errorf("%d kept, the last %v: %v", len(kept), kept[len(kept)-1], err)
	}
	if err := l.Prune(ctx, at(10+MaxTurns)); err != nil {
		t.Fatal(err)
	}
	if kept, _ := l.Turns(ctx, "s"); len(kept) != 6 {
		t.Errorf("%d kept after pruning", len(kept))
	}
	if other, _ := l.Turns(ctx, "other"); len(other) != 0 {
		t.Errorf("another schedule's: %v", other)
	}
}

// firstOf is when the first of turns starts; a time for none.
func firstOf(turns []verdict.RotaTurn) time.Time {
	if len(turns) == 0 {
		return time.Unix(0, 0)
	}
	return turns[0].From
}

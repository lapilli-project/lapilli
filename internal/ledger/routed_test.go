package ledger

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger/ledgertest"
)

func recordRouted(t *testing.T, l *Ledger) []Firing {
	t.Helper()
	ctx := context.Background()
	at := t0.Add(2 * time.Hour)
	record(t, l, t0, t0.Add(6*time.Hour), []firing.Firing{
		{Rule: "DiskFull", Labels: map[string]string{"instance": "a"}, Start: at, End: at.Add(30 * time.Minute)},
		{Rule: "DiskFull", Labels: map[string]string{"instance": "b"}, Start: at, End: at.Add(45 * time.Minute)},
		{Rule: "Quiet", Labels: map[string]string{"instance": "c"}, Start: at.Add(time.Hour)},
	})
	fs, more, err := l.FiringsOverlapping(ctx, t0, t0.Add(6*time.Hour), t0, 100)
	if err != nil || more || len(fs) != 3 {
		t.Fatalf("firings: %d %v %v", len(fs), more, err)
	}
	return fs
}

// What a run worked out is kept for each firing and each notification, and
// read back; a run that works the same hours out again replaces it; a firing
// replaced takes its row with it; and the span worked out grows.
func TestKeepRouted(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fs := recordRouted(t, l)
	if c, err := l.RoutedCover(ctx); err != nil || c.Any() {
		t.Fatalf("cover before: %v %v", c, err)
	}
	at := t0.Add(2 * time.Hour)
	ns := []RoutedNotification{
		{At: at.Add(30 * time.Second), Receiver: "oncall", Kind: "pagerduty", Page: true, Reason: "first", Group: "g", Firings: 2},
		{At: at.Add(30 * time.Second), Receiver: "oncall", Kind: "slack", Reason: "first", Group: "g", Firings: 2},
		{At: at.Add(6 * time.Minute), Receiver: "oncall", Kind: "pagerduty", Page: true, Reason: "resolved", Group: "g"},
	}
	sentBoth := []RoutedSent{{At: ns[0].At, Receiver: "oncall", Kind: "pagerduty", Page: true}, {At: ns[1].At, Receiver: "oncall", Kind: "slack"}}
	routed := []RoutedFiring{
		{FiringID: fs[0].ID, Grouped: true, Whole: true, Sent: sentBoth},
		{FiringID: fs[1].ID, Grouped: true, Whole: true, Sent: sentBoth,
			Muted: []RoutedMute{{Kind: "inhibition", By: "down", SourceRule: "NodeDown"}}},
		{FiringID: fs[2].ID},
	}
	if err := l.KeepRouted(ctx, t0, t0.Add(6*time.Hour), "", ns, routed); err != nil {
		t.Fatal(err)
	}
	cover, err := l.RoutedCover(ctx)
	if from, ok := cover.First(); err != nil || !ok || !from.Equal(t0) || !cover.Covers(t0, t0.Add(6*time.Hour)) {
		t.Fatalf("cover: %v %v", cover, err)
	}
	got, err := l.RoutedOf(ctx, []int64{fs[0].ID, fs[1].ID, fs[2].ID})
	if err != nil || len(got) != 3 {
		t.Fatalf("read: %v %v", got, err)
	}
	a := got[fs[0].ID]
	if a.Notified != 2 || a.Paged != 1 || !a.Grouped || !a.FirstPage.Equal(ns[0].At) || len(a.PageAts) != 1 || a.PageAts[0].Receiver != "oncall" || len(a.Receivers) != 2 || a.Receivers[0].Kind != "pagerduty" || len(a.Muted) != 0 {
		t.Errorf("firing a: %+v", a)
	}
	if m := got[fs[1].ID].Muted; len(m) != 1 || m[0].SourceRule != "NodeDown" {
		t.Errorf("firing b's mutes: %+v", m)
	}
	if c := got[fs[2].ID]; c.Notified != 0 || !c.FirstPage.IsZero() {
		t.Errorf("firing c: %+v", c)
	}
	by, err := l.RoutedByReceiver(ctx, t0, t0.Add(6*time.Hour))
	if err != nil || by["oncall"] != (RoutedCount{3, 2}) {
		t.Errorf("by receiver: %v %v", by, err)
	}
	pages, more, err := l.RoutedPages(ctx, t0, t0.Add(6*time.Hour), 10)
	if err != nil || more || len(pages) != 2 || pages[0].Rule != "DiskFull" || !pages[0].Black || pages[0].Paged != 1 || len(pages[0].PageAts) != 1 {
		t.Errorf("pages: %+v %v %v", pages, more, err)
	}
	// Worked out again from the same hours: replaced, not added to.
	if err := l.KeepRouted(ctx, t0, t0.Add(8*time.Hour), "", ns[:1], routed[:1]); err != nil {
		t.Fatal(err)
	}
	by, _ = l.RoutedByReceiver(ctx, t0, t0.Add(8*time.Hour))
	if by["oncall"] != (RoutedCount{1, 1}) {
		t.Errorf("after again: %v", by)
	}
	if got, _ := l.RoutedOf(ctx, []int64{fs[0].ID}); len(got[fs[0].ID].Receivers) != 2 {
		t.Errorf("a firing worked out again has one row: %+v", got)
	}
	if cover, _ := l.RoutedCover(ctx); !cover.Covers(t0, t0.Add(8*time.Hour)) || len(cover) != 1 {
		t.Errorf("cover grew to %v", cover)
	}
	// The firings replaced by a run that finds them again: their rows go.
	record(t, l, t0, t0.Add(8*time.Hour), []firing.Firing{{Rule: "DiskFull", Labels: map[string]string{"instance": "a"}, Start: at, End: at.Add(30 * time.Minute)}})
	if got, err := l.RoutedOf(ctx, []int64{fs[0].ID, fs[1].ID, fs[2].ID}); err != nil || len(got) != 0 {
		t.Errorf("rows of replaced firings: %v %v", got, err)
	}
	if err := l.ForgetRouted(ctx); err != nil {
		t.Fatal(err)
	}
	if cover, _ := l.RoutedCover(ctx); cover.Any() || len(cover) != 0 {
		t.Error("cover after forgetting")
	}
}

// What was recorded is counted by receiver, with when the first came.
func TestRecordedByReceiver(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	start := t0.Add(time.Hour)
	record(t, l, t0, t0.Add(6*time.Hour), []firing.Firing{{Rule: "DiskFull", Labels: map[string]string{"instance": "db-1"}, Start: start}})
	for i, recv := range []string{"oncall/pagerduty", "oncall/slack", "oncall/slack"} {
		n := Notification{At: start.Add(time.Duration(i+1) * time.Hour), Source: "alertmanager-events", Receiver: recv, Group: "g", Reason: ReasonFirst,
			Alerts: []Alert{{Rule: "DiskFull", Labels: map[string]string{"alertname": "DiskFull", "instance": "db-1"}, Start: start}}}
		if _, err := l.Notify(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	by, err := l.RecordedByReceiver(ctx, t0, t0.Add(24*time.Hour))
	if err != nil || by["oncall/slack"].Notifications != 2 || by["oncall/pagerduty"].Notifications != 1 || !by["oncall/pagerduty"].First.Equal(start.Add(time.Hour)) {
		t.Errorf("recorded: %v %v", by, err)
	}
	first, err := l.RecordedFirst(ctx)
	if err != nil || len(first) != 2 {
		t.Errorf("first: %v %v", first, err)
	}
}

// A ledger migrated from 034 holds nothing worked out and keeps its firings;
// what is worked out after is kept.
func TestMigrate035(t *testing.T) {
	ctx := context.Background()
	where := place(t)
	l, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	recordRouted(t, l)
	l.Close()
	back := slices.Clone(ledgertest.Undo035)
	if strings.HasPrefix(where, "postgres") {
		c, err := pgx.Connect(ctx, where)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(back, "UPDATE schema_version SET version = 34") {
			if _, err := c.Exec(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		c.Close(ctx)
	} else {
		l, err := Open(ctx, where, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(back, "PRAGMA user_version = 34") {
			if _, err := l.w.ExecContext(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		l.Close()
	}
	l2, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatalf("migrating to 035: %v", err)
	}
	defer l2.Close()
	fs, _, err := l2.FiringsOverlapping(ctx, t0, t0.Add(6*time.Hour), t0, 100)
	if err != nil || len(fs) != 3 {
		t.Fatalf("firings after migrating: %d %v", len(fs), err)
	}
	if got, err := l2.RoutedOf(ctx, []int64{fs[0].ID}); err != nil || len(got) != 0 {
		t.Errorf("worked out after migrating: %v %v", got, err)
	}
	if err := l2.KeepRouted(ctx, t0, t0.Add(6*time.Hour), "", nil, []RoutedFiring{{FiringID: fs[0].ID, Sent: []RoutedSent{{At: t0.Add(3 * time.Hour), Receiver: "oncall", Kind: "slack"}}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := l2.RoutedOf(ctx, []int64{fs[0].ID}); got[fs[0].ID].Notified != 1 {
		t.Errorf("kept after migrating: %v", got)
	}
}

// A gap in what was worked out reads as a gap: the hours tried and not
// worked out, and those not tried, are not covered, with why; a stretch
// worked out over one that wasn't covers it again.
func TestRoutedCover(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	for _, w := range [][2]int{{0, 2}, {2, 4}, {6, 8}} {
		if err := l.KeepRouted(ctx, h(w[0]), h(w[1]), "", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	c, err := l.RoutedCover(ctx)
	if err != nil || len(c) != 2 || !c[0].To.Equal(h(4)) {
		t.Fatalf("cover (adjacent stretches join): %+v %v", c, err)
	}
	if c.Covers(h(0), h(8)) || !c.Covers(h(1), h(4)) || !c.Covers(h(6), h(7)) {
		t.Errorf("covers: %+v", c)
	}
	if g := c.Gaps(h(0), h(9)); len(g) != 2 || !g[0][0].Equal(h(4)) || !g[0][1].Equal(h(6)) || !g[1][0].Equal(h(8)) {
		t.Errorf("gaps: %v", g)
	}
	// Tried and not worked out, over the middle of a stretch worked out.
	why := "more than 300000 firings in the hours worked out: nothing of them is known, and this text is kept whole"
	if err := l.MarkRoutedUnknown(ctx, h(1), h(3), why); err != nil {
		t.Fatal(err)
	}
	c, _ = l.RoutedCover(ctx)
	if c.Covers(h(0), h(4)) || !c.Covers(h(0), h(1)) || !c.Covers(h(3), h(4)) || c.Covers(h(2), h(3)) {
		t.Errorf("cover after unknown: %+v", c)
	}
	if got := c.Unknown(h(0), h(8)); got != why {
		t.Errorf("why (kept whole): %q", got)
	}
	if got := c.Unknown(h(3), h(4)); got != "" {
		t.Errorf("why over a stretch worked out: %q", got)
	}
	// Worked out again: it covers.
	if err := l.KeepRouted(ctx, h(0), h(4), "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if c, _ = l.RoutedCover(ctx); !c.Covers(h(0), h(4)) || c.Unknown(h(0), h(4)) != "" {
		t.Errorf("cover worked out again: %+v", c)
	}
	// A caveat on a stretch worked out is kept with it.
	if err := l.KeepRouted(ctx, h(4), h(6), "the lead-in kept the first firings", nil, nil); err != nil {
		t.Fatal(err)
	}
	if c, _ = l.RoutedCover(ctx); !c.Covers(h(0), h(8)) || c.Unknown(h(5), h(6)) != "the lead-in kept the first firings" {
		t.Errorf("caveat: %+v", c)
	}
}

// KeepRouted replaces the notifications of its hours alone: those after them
// stay.
func TestKeepRoutedBounds(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	n := func(at time.Time) []RoutedNotification {
		return []RoutedNotification{{At: at, Receiver: "r", Kind: "slack", Reason: "first", Group: "g", Firings: 1}}
	}
	if err := l.KeepRouted(ctx, h(0), h(2), "", n(h(1)), nil); err != nil {
		t.Fatal(err)
	}
	if err := l.KeepRouted(ctx, h(4), h(6), "", n(h(5)), nil); err != nil {
		t.Fatal(err)
	}
	// The first hours again: the notification in the later ones isn't theirs.
	if err := l.KeepRouted(ctx, h(0), h(2), "", nil, nil); err != nil {
		t.Fatal(err)
	}
	by, _ := l.RoutedByReceiver(ctx, h(0), h(24))
	if by["r"].Notifications != 1 {
		t.Errorf("after the first hours again: %v", by)
	}
}

// The firings the run's own hours reach come first when there are more than
// the limit: the lead-in is what is left out.
func TestFiringsOverlappingOwnFirst(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	record(t, l, t0, t0.Add(10*time.Hour), []firing.Firing{
		{Rule: "Old", Labels: map[string]string{"i": "1"}, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)},
		{Rule: "Old", Labels: map[string]string{"i": "2"}, Start: t0.Add(90 * time.Minute), End: t0.Add(2 * time.Hour)},
		{Rule: "Own", Labels: map[string]string{"i": "3"}, Start: t0.Add(6 * time.Hour), End: t0.Add(7 * time.Hour)},
		{Rule: "Own", Labels: map[string]string{"i": "4"}, Start: t0.Add(3 * time.Hour)},
	})
	fs, more, err := l.FiringsOverlapping(ctx, t0, t0.Add(8*time.Hour), t0.Add(5*time.Hour), 2)
	if err != nil || !more || len(fs) != 2 || fs[0].Rule != "Own" || fs[1].Rule != "Own" {
		t.Errorf("own first: %+v %v %v", fs, more, err)
	}
}

// Pruning takes the notifications worked out before the time with the
// firings, and the cover with them.
func TestPruneRouted(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	var ns []RoutedNotification
	for i := 0; i < 12; i++ {
		ns = append(ns, RoutedNotification{At: h(i), Receiver: "r", Kind: "slack", Reason: "first", Group: "g", Firings: 1})
	}
	if err := l.KeepRouted(ctx, h(0), h(12), "", ns, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.Prune(ctx, h(6)); err != nil {
		t.Fatal(err)
	}
	by, _ := l.RoutedByReceiver(ctx, h(0), h(24))
	if by["r"].Notifications != 6 {
		t.Errorf("after pruning: %v", by)
	}
	c, _ := l.RoutedCover(ctx)
	if len(c) != 1 || !c[0].From.Equal(h(6)) || !c[0].To.Equal(h(12)) {
		t.Errorf("cover after pruning: %+v", c)
	}
}

// What a firing reached is counted from the notifications of every run that
// saw it: a run of later hours adds to those of the earlier, and a run that
// works some hours out again replaces those and no others; a firing some of
// whose life is in hours that aren't worked out has none.
func TestRoutedFiringAccumulates(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fs := recordRouted(t, l)
	f := fs[2] // Quiet: on from t0+3h with no end
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	sent := func(at time.Time, page bool) RoutedSent {
		return RoutedSent{At: at, Receiver: "ring", Kind: "pushover", Page: page}
	}
	keep := func(from, to int, whole bool, ss ...RoutedSent) {
		t.Helper()
		if err := l.KeepRouted(ctx, h(from), h(to), "", nil, []RoutedFiring{{FiringID: f.ID, Whole: whole, Sent: ss}}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() RoutedFiring {
		t.Helper()
		got, err := l.RoutedOf(ctx, []int64{f.ID})
		if err != nil {
			t.Fatal(err)
		}
		return got[f.ID]
	}
	keep(3, 4, true, sent(h(3), true), sent(h(3).Add(40*time.Minute), true))
	keep(4, 5, false, sent(h(4).Add(20*time.Minute), true))
	if x := read(); x.Notified != 3 || x.Paged != 3 || !x.FirstPage.Equal(h(3)) || len(x.PageAts) != 3 || len(x.Receivers) != 1 || x.Receivers[0].Notified != 3 {
		t.Fatalf("two runs: %+v", x)
	}
	// A run that sees it for the last hour only never takes the first away.
	keep(5, 6, false)
	if x := read(); x.Notified != 3 || !x.FirstPage.Equal(h(3)) {
		t.Errorf("a run that saw none of its notifications: %+v", x)
	}
	// Worked out again, the hour has its own.
	keep(4, 5, false, sent(h(4).Add(50*time.Minute), false))
	if x := read(); x.Notified != 3 || x.Paged != 2 || len(x.PageAts) != 2 {
		t.Errorf("an hour again: %+v", x)
	}
	// An hour that wasn't worked out leaves it not known, and a run that
	// works the others out doesn't make a number of it.
	if err := l.MarkRoutedUnknown(ctx, h(3), h(4), "failed"); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.RoutedOf(ctx, []int64{f.ID}); len(got) != 0 {
		t.Errorf("a firing with an hour not known has a row: %+v", got)
	}
	keep(4, 5, false, sent(h(4).Add(50*time.Minute), false))
	if got, _ := l.RoutedOf(ctx, []int64{f.ID}); len(got) != 0 {
		t.Errorf("a run of the other hours gave a firing with an hour not known a number: %+v", got)
	}
	// Worked out again, it is whole.
	keep(3, 4, false, sent(h(3), true))
	if x := read(); x.Notified != 2 {
		t.Errorf("worked out again: %+v", x)
	}
}

// What a group last notified before a time is read back, the latest for
// each group, receiver and integration kind.
func TestRoutedNotifiedBefore(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	n := func(at time.Time, g, kind string) RoutedNotification {
		return RoutedNotification{At: at, Receiver: "ring", Kind: kind, Reason: "repeat", Group: g, Firings: 1}
	}
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	if err := l.KeepRouted(ctx, h(0), h(8), "", []RoutedNotification{n(h(1), "g1", "pushover"), n(h(3), "g1", "pushover"), n(h(2), "g1", "slack"), n(h(5), "g1", "pushover"), n(h(0).Add(-time.Hour), "g2", "pushover")}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := l.RoutedNotifiedBefore(ctx, h(0), h(4))
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, x := range got {
		if x.Group != "g1" || x.Kind == "pushover" && !x.At.Equal(h(3)) || x.Kind == "slack" && !x.At.Equal(h(2)) {
			t.Errorf("%+v", x)
		}
	}
}

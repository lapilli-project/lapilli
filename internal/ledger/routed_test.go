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
	fs, more, err := l.FiringsOverlapping(ctx, t0, t0.Add(6*time.Hour), 100)
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
	if _, _, ok, err := l.RoutedSpan(ctx); err != nil || ok {
		t.Fatalf("span before: %v %v", ok, err)
	}
	at := t0.Add(2 * time.Hour)
	ns := []RoutedNotification{
		{At: at.Add(30 * time.Second), Receiver: "oncall", Kind: "pagerduty", Page: true, Reason: "first", Group: "g", Firings: 2},
		{At: at.Add(30 * time.Second), Receiver: "oncall", Kind: "slack", Reason: "first", Group: "g", Firings: 2},
		{At: at.Add(6 * time.Minute), Receiver: "oncall", Kind: "pagerduty", Page: true, Reason: "resolved", Group: "g"},
	}
	routed := []RoutedFiring{
		{FiringID: fs[0].ID, Notified: 2, Paged: 1, Grouped: true, FirstPage: ns[0].At, PageAts: []RoutedPageAt{{ns[0].At, "oncall"}},
			Receivers: []RoutedReceiver{{Receiver: "oncall", Kind: "pagerduty", Notified: 1, Paged: 1}, {Receiver: "oncall", Kind: "slack", Notified: 1}}},
		{FiringID: fs[1].ID, Notified: 2, Paged: 1, Grouped: true, FirstPage: ns[0].At, PageAts: []RoutedPageAt{{ns[0].At, "oncall"}},
			Muted: []RoutedMute{{Kind: "inhibition", By: "down", SourceRule: "NodeDown"}}},
		{FiringID: fs[2].ID},
	}
	if err := l.KeepRouted(ctx, t0, t0.Add(6*time.Hour), ns, routed); err != nil {
		t.Fatal(err)
	}
	from, to, ok, err := l.RoutedSpan(ctx)
	if err != nil || !ok || !from.Equal(t0) || !to.Equal(t0.Add(6*time.Hour)) {
		t.Fatalf("span: %v %v %v %v", from, to, ok, err)
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
	if err := l.KeepRouted(ctx, t0, t0.Add(8*time.Hour), ns[:1], routed[:1]); err != nil {
		t.Fatal(err)
	}
	by, _ = l.RoutedByReceiver(ctx, t0, t0.Add(8*time.Hour))
	if by["oncall"] != (RoutedCount{1, 1}) {
		t.Errorf("after again: %v", by)
	}
	if got, _ := l.RoutedOf(ctx, []int64{fs[0].ID}); len(got[fs[0].ID].Receivers) != 2 {
		t.Errorf("a firing worked out again has one row: %+v", got)
	}
	if _, to, _, _ := l.RoutedSpan(ctx); !to.Equal(t0.Add(8 * time.Hour)) {
		t.Errorf("span grew to %v", to)
	}
	// The firings replaced by a run that finds them again: their rows go.
	record(t, l, t0, t0.Add(8*time.Hour), []firing.Firing{{Rule: "DiskFull", Labels: map[string]string{"instance": "a"}, Start: at, End: at.Add(30 * time.Minute)}})
	if got, err := l.RoutedOf(ctx, []int64{fs[0].ID, fs[1].ID, fs[2].ID}); err != nil || len(got) != 0 {
		t.Errorf("rows of replaced firings: %v %v", got, err)
	}
	if err := l.ForgetRouted(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := l.RoutedSpan(ctx); ok {
		t.Error("span after forgetting")
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
	fs, _, err := l2.FiringsOverlapping(ctx, t0, t0.Add(6*time.Hour), 100)
	if err != nil || len(fs) != 3 {
		t.Fatalf("firings after migrating: %d %v", len(fs), err)
	}
	if got, err := l2.RoutedOf(ctx, []int64{fs[0].ID}); err != nil || len(got) != 0 {
		t.Errorf("worked out after migrating: %v %v", got, err)
	}
	if err := l2.KeepRouted(ctx, t0, t0.Add(6*time.Hour), nil, []RoutedFiring{{FiringID: fs[0].ID, Notified: 1}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := l2.RoutedOf(ctx, []int64{fs[0].ID}); got[fs[0].ID].Notified != 1 {
		t.Errorf("kept after migrating: %v", got)
	}
}

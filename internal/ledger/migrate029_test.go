package ledger

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"lapilli.dev/internal/firing"
)

// A ledger at 027, with firings and a notification, in SQLite or, with
// LAPILLI_TEST_POSTGRES, in Postgres, migrates to 029 with them unchanged
// and no mute; then a mute is kept, and mutes the firing it tells of.
func TestMigrate029(t *testing.T) {
	ctx := context.Background()
	where := place(t)
	l, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	from, to := t0, t0.Add(24*time.Hour)
	start := t0.Add(5 * time.Hour)
	record(t, l, from, to, []firing.Firing{
		{Rule: "DiskFull", Labels: map[string]string{"instance": "db-1"}, Start: start, End: start.Add(20 * time.Minute)},
		{Rule: "DiskFull", Labels: map[string]string{"instance": "db-2"}, Start: start, End: start.Add(20 * time.Minute)}})
	n := Notification{At: start.Add(time.Minute), Source: "alertmanager-events", Receiver: "oncall/pagerduty", Group: "g", Reason: ReasonFirst,
		Alerts: []Alert{{Rule: "DiskFull", Labels: map[string]string{"alertname": "DiskFull", "instance": "db-1"}, Start: start}}}
	if _, err := l.Notify(ctx, n); err != nil {
		t.Fatal(err)
	}
	read := func(l *Ledger) ([]Notification, any) {
		t.Helper()
		ns, err := l.Notifications(ctx, from, to)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := l.Report(ctx, from, to, opts, true)
		if err != nil {
			t.Fatal(err)
		}
		return ns, &verdict0{rep.Rules, rep.Incidents, rep.Firings, rep.NoRecord}
	}
	ns, rep := read(l)
	l.Close()

	// Back to 027, as 0.66 left it.
	if strings.HasPrefix(where, "postgres") {
		c, err := pgx.Connect(ctx, where)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(slices.Concat(undo030, undo029, undo028), "UPDATE schema_version SET version = 27") {
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
		for _, q := range append(slices.Concat(undo030, undo029, undo028), "PRAGMA user_version = 27") {
			if _, err := l.w.ExecContext(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		l.Close()
	}

	l2, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatalf("migrating to 029: %v", err)
	}
	defer l2.Close()
	if ok, err := l2.HasMutes(ctx); ok || err != nil {
		t.Errorf("mutes after migrating: %v %v", ok, err)
	}
	ns2, rep2 := read(l2)
	if !reflect.DeepEqual(ns, ns2) || !reflect.DeepEqual(rep, rep2) {
		t.Errorf("after migrating:\n%+v %+v\nnot\n%+v %+v", ns2, rep2, ns, rep)
	}
	m := Mute{At: start.Add(time.Minute), Rule: "DiskFull", Labels: map[string]string{"alertname": "DiskFull", "instance": "db-2"}, Kind: MutedSilence, Ref: "s-1"}
	if added, _, err := l2.KeepMutes(ctx, []Mute{m}); err != nil || added != 1 {
		t.Fatalf("a mute after migrating: %d %v", added, err)
	}
	fs, err := l2.Firings(ctx, FiringQuery{From: from, To: to})
	if err != nil || len(fs) != 2 {
		t.Fatalf("%d firings: %v", len(fs), err)
	}
	of, err := l2.MutedOf(ctx, fs, mutedWithin, to)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if muted := len(of[f.ID]) > 0; muted != (f.Labels["instance"] == "db-2") {
			t.Errorf("%v muted: %v", f.Labels, of[f.ID])
		}
	}
}

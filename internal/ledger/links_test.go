package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// linkedFirings are firings about checkout and cart around INC-7, whose
// tool linked CartErrors's alert about cart to it.
func linkedFirings() ([]firing.Firing, incident.Incident) {
	at := func(m int) time.Time { return t0.Add(2*time.Hour + time.Duration(m)*time.Minute) }
	fs := []firing.Firing{
		{Rule: "CartErrors", Labels: map[string]string{"service": "cart"}, Start: at(1), End: at(20)},
		{Rule: "CartErrors", Labels: map[string]string{"service": "search"}, Start: at(2), End: at(4)},
		{Rule: "CheckoutLatency", Labels: map[string]string{"service": "checkout"}, Start: at(3), End: at(9)},
	}
	in := incident.Incident{ID: "INC-7", Start: at(0), End: at(40), Labels: incident.Labels{"service": {"checkout"}},
		Alerts: []incident.Alert{{Rule: "CartErrors", Labels: map[string]string{"service": "cart"}}}}
	return fs, in
}

// The ledger keeps the alerts an incident lists, and judges by them as
// verdict.Judge does, with --linked-only and without: a catch by one is
// linked; given in another order, the incident changes nothing; past the
// bounds, it is refused.
func TestLinkedAlertsKept(t *testing.T) {
	ctx := context.Background()
	for _, only := range []bool{false, true} {
		l := open(t, time.UTC)
		o := opts
		o.LinkedOnly = only
		fs, in := linkedFirings()
		incs := []incident.Incident{keepAll, in}
		if _, err := l.SyncFile(ctx, incs, o); err != nil {
			t.Fatal(err)
		}
		to := t0.Add(7 * 24 * time.Hour)
		if err := l.Record(ctx, Found{From: t0, To: to, Firings: fs, Run: Run{Kind: "test", Started: to, Finished: to, From: t0, To: to}}, o); err != nil {
			t.Fatal(err)
		}
		o.From, o.To = t0, to
		want := verdict.Judge(nil, slices.Clone(fs), incs, o)
		got, err := l.Report(ctx, o.From, o.To, o, true)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Incidents, want.Incidents) {
			t.Errorf("linked only %v: incidents\n got %+v\nwant %+v", only, got.Incidents, want.Incidents)
		}
		kept, err := l.IncidentByID(ctx, "INC-7")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(kept.Alerts, in.Alerts) {
			t.Errorf("alerts kept %+v, want %+v", kept.Alerts, in.Alerts)
		}
		var linked []string
		for _, c := range kept.CaughtBy {
			if c.Linked {
				linked = append(linked, c.Rule)
			}
		}
		if n := len(kept.CaughtBy); !slices.Equal(linked, []string{"CartErrors"}) || only && n != 1 || !only && n != 2 {
			t.Errorf("linked only %v: caught by %+v, want CartErrors linked and, without it, CheckoutLatency", only, kept.CaughtBy)
		}
	}

	// Copied in by a job, with alerts in another order the next time:
	// nothing changes, and nothing is judged again.
	l := open(t, time.UTC)
	in := incident.Incident{ID: "PD-1", Start: t0, Alerts: []incident.Alert{{Rule: "B"}, {Rule: "A", Labels: map[string]string{"x": "1"}}}}
	if _, err := l.Put(ctx, in, Copier, opts); err != nil {
		t.Fatal(err)
	}
	var spans int
	judging = func(lo, hi int64) { spans++ }
	defer func() { judging = nil }()
	in.Alerts = []incident.Alert{in.Alerts[1], in.Alerts[0], in.Alerts[0]}
	if _, err := l.Put(ctx, in, Copier, opts); err != nil || spans != 0 {
		t.Errorf("written again, its alerts in another order: %v, %d spans judged again", err, spans)
	}
	in.Alerts = in.Alerts[:1]
	if _, err := l.Put(ctx, in, Copier, opts); err != nil || spans == 0 {
		t.Errorf("written with an alert fewer: %v, nothing judged again", err)
	}
	if got, _ := l.IncidentByID(ctx, "PD-1"); len(got.Alerts) != 1 || got.Alerts[0].Rule != "A" {
		t.Errorf("alerts %+v", got.Alerts)
	}
	many := make([]incident.Alert, incident.MaxAlerts+1)
	for i := range many {
		many[i] = incident.Alert{Rule: fmt.Sprint("R", i)}
	}
	for _, bad := range [][]incident.Alert{many, {{Rule: ""}}, {{Rule: "R", Labels: map[string]string{"k": strings.Repeat("v", 300)}}}} {
		in.Alerts = bad
		if _, err := l.Put(ctx, in, Copier, opts); !invalidErr(err) {
			t.Errorf("%.60v: %v, want refused as invalid", bad, err)
		}
	}
}

// A ledger at 027 migrates to 028 with every incident listing no alerts and
// every catch not linked, its stones as they were.
func TestMigrate028(t *testing.T) {
	ctx := context.Background()
	where := place(t)
	l, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	fs, in := linkedFirings()
	in.Alerts = nil
	if _, err := l.SyncFile(ctx, []incident.Incident{keepAll, in}, opts); err != nil {
		t.Fatal(err)
	}
	record(t, l, t0, t0.Add(7*24*time.Hour), fs)
	o := opts
	o.From, o.To = t0, t0.Add(7*24*time.Hour)
	before, err := l.Report(ctx, o.From, o.To, o, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(where, "postgres") {
		l.Close()
		c, err := pgx.Connect(ctx, where)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(slices.Concat(undo032, undo031, undo030, undo029, undo028), "UPDATE schema_version SET version = 27") {
			if _, err := c.Exec(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		c.Close(ctx)
	} else {
		for _, q := range append(slices.Concat(undo032, undo031, undo030, undo029, undo028), "PRAGMA user_version = 27") {
			if _, err := l.w.ExecContext(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		l.Close()
	}
	l2, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatalf("migrating to 028: %v", err)
	}
	defer l2.Close()
	after, err := l2.Report(ctx, o.From, o.To, o, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Incidents, after.Incidents) || !reflect.DeepEqual(before.Rules, after.Rules) {
		t.Errorf("after migrating:\n%+v\nnot\n%+v", after.Incidents, before.Incidents)
	}
	var alerts string
	var linked sql.NullInt64
	if err := l2.r.QueryRowContext(ctx, "SELECT i.alerts, max(c.linked) FROM incidents i JOIN catches c ON c.incident_id = i.id WHERE i.key = 'INC-7' GROUP BY i.alerts").Scan(&alerts, &linked); err != nil || alerts != "" || linked.Int64 != 0 {
		t.Errorf("INC-7 after migrating: alerts %q, linked %v: %v", alerts, linked, err)
	}
}

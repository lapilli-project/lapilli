package ledger

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger/ledgertest"
	"lapilli.dev/internal/response"
	"lapilli.dev/internal/verdict"
)

// respKept is what firings.response, routine and useful hold for each firing, by
// the pod or deployment label that names it.
type respKept struct{ response, routine, useful int }

func respKeptOf(t *testing.T, l *Ledger) map[string]respKept {
	t.Helper()
	rows, err := l.r.QueryContext(context.Background(), `SELECT s.labels, f.response, f.routine, f.useful FROM firings f JOIN series s ON s.id = f.series_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]respKept{}
	for rows.Next() {
		var labels string
		var k respKept
		if err := rows.Scan(&labels, &k.response, &k.routine, &k.useful); err != nil {
			t.Fatal(err)
		}
		m, _ := l.labels(0, labels)
		_ = m
		name := labels[strings.Index(labels, `"name":"`)+8:]
		out[name[:strings.Index(name, `"`)]] = k
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func page(id, rule, name string, sent, ack, res time.Time, by string) response.Page {
	return response.Page{ID: id, Alerts: []incident.Alert{{Rule: rule, Labels: map[string]string{"name": name}}}, Sent: sent, Acknowledged: ack, Resolved: res, ResolvedBy: by}
}

func firingOf(name string, ns string, extra map[string]string, start time.Time, mins int) firing.Firing {
	l := map[string]string{"name": name, "namespace": ns}
	for k, v := range extra {
		l[k] = v
	}
	return firing.Firing{Rule: "R", Labels: l, Start: start, End: start.Add(time.Duration(mins) * time.Minute)}
}

// A firing's response and usefulness are worked out from what the ledger
// keeps: a person's answer first, a change on its target, the page's
// acknowledgement or the alert manager's resolve, a routine change falling
// through to the page; and again when the pages are written again, the
// changes counted by day and week.
func TestRespond(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	o.Within = func(string) time.Duration { return 4 * time.Minute }
	at := t0.Add(24 * time.Hour)
	// The workloads' changes: shop/cart rolled out once at +10m; shop/pay
	// rolled out every hour, so that one's change is routine.
	var changes []response.Change
	changes = append(changes, response.Change{Kind: response.Rollout, At: at.Add(10 * time.Minute), Namespace: "shop", WorkloadKind: "Deployment", Workload: "cart", Revision: "cart-1"})
	for m := 0; m < 24*6; m++ {
		changes = append(changes, response.Change{Kind: response.Rollout, At: at.Add(time.Duration(m)*10*time.Minute + 5*time.Minute), Namespace: "shop", WorkloadKind: "Deployment", Workload: "pay", Revision: fmt.Sprint("pay-", m)})
	}
	span := []response.Span{{From: t0, To: t0.Add(7 * 24 * time.Hour)}}
	rolls := &response.Rollouts{
		Changes: changes,
		Coverage: map[response.Workload][]response.Span{
			{Namespace: "shop", Kind: "Deployment", Name: "cart"}: span,
			{Namespace: "shop", Kind: "Deployment", Name: "pay"}:  span,
		},
		Owners: map[response.Pod]response.Workload{{Namespace: "shop", Name: "web-7f9c"}: {Namespace: "shop", Kind: "Deployment", Name: "web"}},
	}
	fs := []firing.Firing{
		firingOf("rolled", "shop", map[string]string{"deployment": "cart"}, at, 30),                       // changed
		firingOf("routine-unanswered", "shop", map[string]string{"deployment": "pay"}, at, 120),           // routine change, then its page
		firingOf("unanswered", "shop", nil, at, 20),                                                       // page nobody answered
		firingOf("acked-known", "shop", map[string]string{"deployment": "cart"}, at.Add(2*time.Hour), 20), // acknowledged, nothing changed there
		firingOf("acked-unknown", "shop", nil, at, 20),                                                    // acknowledged, target unknown
		firingOf("said-yes", "shop", nil, at, 20),                                                         // a person's yes over an unanswered page
		firingOf("said-no", "shop", map[string]string{"deployment": "cart"}, at, 30),                      // a person's no over a change
		firingOf("by-pod", "shop", map[string]string{"pod": "web-7f9c"}, at, 30),                          // pod of a Deployment with no change
		firingOf("none", "shop", nil, at, 20),                                                             // nothing known
	}
	found := Found{From: t0, To: t0.Add(7 * 24 * time.Hour), Firings: fs, Rollouts: rolls,
		Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: t0.Add(7 * 24 * time.Hour)}}
	if err := l.Record(ctx, found, o); err != nil {
		t.Fatal(err)
	}
	pages := []response.Page{
		page("P1", "R", "routine-unanswered", at.Add(time.Minute), time.Time{}, at.Add(2*time.Minute), response.ByAlert),
		page("P2", "R", "unanswered", at.Add(time.Minute), time.Time{}, at.Add(3*time.Minute), response.ByAlert),
		page("P3", "R", "acked-known", at.Add(2*time.Hour), at.Add(2*time.Hour+time.Minute), time.Time{}, ""),
		page("P4", "R", "acked-unknown", at.Add(time.Minute), at.Add(2*time.Minute), time.Time{}, ""),
		page("P5", "R", "said-yes", at.Add(time.Minute), time.Time{}, at.Add(2*time.Minute), response.ByAlert),
	}
	if got, err := l.SyncResponses(ctx, ResponsesFile, "sum", pages, o); err != nil || got.Added != 5 {
		t.Fatalf("SyncResponses: %+v %v", got, err)
	}
	// People's words, by the labels that name the firing.
	got, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(7 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	say := func(name, action string) {
		t.Helper()
		for _, f := range got {
			if f.Labels["name"] == name {
				if _, err := l.SetVerdict(ctx, keyOfFiring(f), VerdictChange{Action: &action}, VerdictBy{By: "ana", Admin: true, Now: time.Now()}, o); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
		t.Fatal("no firing", name)
	}
	say("said-yes", "yes")
	say("said-no", "no")
	want := map[string]respKept{
		"rolled":             {int(response.Changed), 0, 1},
		"routine-unanswered": {int(response.Unanswered), 1, 0},
		"unanswered":         {int(response.Unanswered), 0, 0},
		"acked-known":        {int(response.Acknowledged), 0, 0},
		"acked-unknown":      {int(response.Acknowledged), 0, -1},
		"said-yes":           {int(response.Unanswered), 0, 1},
		"said-no":            {int(response.Changed), 0, 0},
		"by-pod":             {int(response.None), 0, -1},
		"none":               {int(response.None), 0, -1},
	}
	if g := respKeptOf(t, l); !equalKept(g, want) {
		t.Errorf("responses kept:\n%v\nwant\n%v", g, want)
	}
	// Counted by week: those with each response, useful and not useful.
	weeks, err := l.Weeks(ctx, Week(l.Day(at)), Week(l.Day(at)))
	if err != nil || len(weeks) != 1 {
		t.Fatalf("weeks: %v %v", weeks, err)
	}
	w := weeks[0]
	if w.Firings != 9 || w.RespChanged != 2 || w.RespAcknowledged != 2 || w.RespUnanswered != 3 || w.RespRoutine != 0 || w.Useful != 2 || w.NotUseful != 4 {
		t.Errorf("the week: %+v", w)
	}

	// Written again, a page that the alert manager no longer resolved: the
	// firing it answers is acknowledged, and judged again.
	more := slices.Clone(pages)
	more[1] = page("P2", "R", "unanswered", at.Add(time.Minute), at.Add(2*time.Minute), at.Add(3*time.Minute), response.ByPerson)
	got2, err := l.PutResponses(ctx, "aaa-pd", at, at.Add(time.Hour), more[1:2], o)
	if err != nil || got2.Added != 1 {
		t.Fatalf("PutResponses: %+v %v", got2, err)
	}
	// Its own source: the file's page of the same id stays.
	if k := respKeptOf(t, l)["unanswered"]; k.response != int(response.Unanswered) {
		t.Errorf("a page of another source with the same id changed the file's: %+v", k)
	}
	if _, err := l.SyncResponses(ctx, ResponsesFile, "sum2", more, o); err != nil {
		t.Fatal(err)
	}
	if k := respKeptOf(t, l)["unanswered"]; k.response != int(response.Acknowledged) || k.useful != -1 {
		t.Errorf("after the page changed: %+v", k)
	}
	// A page sent outside its period is refused.
	if _, err := l.PutResponses(ctx, "aaa-pd", at, at.Add(time.Minute), more[2:3], o); err == nil {
		t.Error("a page sent outside the period was taken")
	}
	// Replaced by span: the source's page in the span that isn't sent again goes.
	got3, err := l.PutResponses(ctx, "aaa-pd", at, at.Add(time.Hour), nil, o)
	if err != nil || got3.Removed != 1 {
		t.Errorf("replacing the span with none: %+v %v", got3, err)
	}
	// Judging every firing again finds the same.
	before := respKeptOf(t, l)
	if err := l.JudgeAll(ctx, o, nil); err != nil {
		t.Fatal(err)
	}
	if after := respKeptOf(t, l); !equalKept(after, before) {
		t.Errorf("judged again:\n%v\nwas\n%v", after, before)
	}
	// The report counts them, a rule's actionable share, and says each
	// firing's page.
	rep, err := l.Report(ctx, t0, t0.Add(7*24*time.Hour), o, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Responses == nil || rep.Responses.Pages != 5 || !rep.Responses.Rolled {
		t.Errorf("responses read: %+v", rep.Responses)
	}
	var rr *verdict.RuleResult
	for i := range rep.Rules {
		if rep.Rules[i].Rule == "R" {
			rr = &rep.Rules[i]
		}
	}
	if rr == nil || rr.Responses == nil || rr.Responses.Changed != 2 || rr.Responses.Actionable == nil {
		t.Fatalf("the rule's responses: %+v", rr)
	}
	for _, j := range rep.Firings {
		if j.Response == nil {
			t.Fatalf("a firing with no response: %+v", j)
		}
		if j.Labels["name"] == "rolled" && (j.Response.Kind != verdict.ResponseChanged || j.Response.Change == nil || j.Response.Useful == nil || !*j.Response.Useful) {
			t.Errorf("rolled: %+v", j.Response)
		}
		if j.Labels["name"] == "acked-known" && (j.Response.Page == nil || j.Response.Page.ID != "P3") {
			t.Errorf("acked-known's page: %+v", j.Response)
		}
	}
}

func equalKept(a, b map[string]respKept) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A black firing judged by its record whose use no response tells is not
// useful, in the week the record judges it: unknown use is counted by what
// kept the record, as the stones' are.
func TestRespondUnknownJudged(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	at := t0.Add(24 * time.Hour)
	if _, err := l.SyncFile(ctx, []incident.Incident{keepAll}, o); err != nil {
		t.Fatal(err)
	}
	svc := map[string]string{"service": "shop"}
	fs := []firing.Firing{firingOf("a", "shop", svc, at, 20), firingOf("b", "shop", svc, at.Add(time.Hour), 20)}
	if err := l.Record(ctx, Found{From: t0, To: t0.Add(7 * 24 * time.Hour), Firings: fs, Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: t0.Add(7 * 24 * time.Hour)}}, o); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SyncResponses(ctx, ResponsesFile, "", []response.Page{page("P", "R", "a", at.Add(time.Minute), at.Add(2*time.Minute), at.Add(3*time.Minute), response.ByPerson)}, o); err != nil {
		t.Fatal(err)
	}
	rep, err := l.Report(ctx, t0, t0.Add(7*24*time.Hour), o, false)
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Rules[0].Responses
	// Both are black and the week's record is kept: not useful; the first
	// was acknowledged, the second has no response.
	if c == nil || c.Acknowledged != 1 || c.None != 1 || c.Useful != 0 || c.NotUseful != 2 || c.Actionable == nil || *c.Actionable != 0 {
		t.Errorf("responses: %+v", c)
	}
	// By useful, it is noisy once it has the firings for it.
	o.NoisyBy, o.NoisyFirings = verdict.NoisyByUseful, 2
	rep, err = l.Report(ctx, t0, t0.Add(7*24*time.Hour), o, false)
	if err != nil || !rep.Rules[0].Noisy {
		t.Errorf("noisy by useful: %+v %v", rep.Rules[0], err)
	}
}

// Changes read by a run are kept whole: a workload's spans that meet are one,
// a change read again is kept once, and a change new that fell in a firing
// that started before the run has that firing judged again.
func TestKeepRollouts(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	at := t0.Add(24 * time.Hour)
	w := response.Workload{Namespace: "shop", Kind: "Deployment", Name: "cart"}
	f := firingOf("late", "shop", map[string]string{"deployment": "cart"}, at, 180)
	if err := l.Record(ctx, Found{From: t0, To: at.Add(time.Hour), Firings: []firing.Firing{f}, Rollouts: &response.Rollouts{
		Coverage: map[response.Workload][]response.Span{w: {{From: t0, To: at.Add(time.Hour)}}}},
		Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: at.Add(time.Hour)}}, o); err != nil {
		t.Fatal(err)
	}
	if k := respKeptOf(t, l)["late"]; k.response != int(response.None) {
		t.Fatalf("before: %+v", k)
	}
	// The next run starts after the firing did, and sees the rollout made two
	// hours in.
	next := Found{From: at.Add(time.Hour), To: at.Add(3 * time.Hour), Rollouts: &response.Rollouts{
		Changes:  []response.Change{{Kind: response.Rollout, At: at.Add(2 * time.Hour), Namespace: "shop", WorkloadKind: "Deployment", Workload: "cart"}},
		Coverage: map[response.Workload][]response.Span{w: {{From: at.Add(time.Hour), To: at.Add(3 * time.Hour)}}}},
		Run: Run{Kind: "test", Started: t0, Finished: t0, From: at.Add(time.Hour), To: at.Add(3 * time.Hour)}}
	if err := l.Record(ctx, next, o); err != nil {
		t.Fatal(err)
	}
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	if k := respKeptOf(t, l)["late"]; k.response != int(response.Changed) || k.useful != 1 {
		t.Errorf("after the rollout: %+v", k)
	}
	var n int
	if err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM rollout_spans").Scan(&n); err != nil || n != 1 {
		t.Errorf("a workload's spans that meet are one: %d %v", n, err)
	}
	if err := l.KeepRollouts(ctx, next.Rollouts, at.Add(3*time.Hour), o); err != nil {
		t.Fatal(err)
	}
	if err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM rollouts").Scan(&n); err != nil || n != 1 {
		t.Errorf("a change read again: %d %v", n, err)
	}
}

// A ledger migrated from 033 holds no response, and keeps its firings'
// stones; a page written after is kept.
func TestMigrate034(t *testing.T) {
	ctx := context.Background()
	where := place(t)
	l, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	at := t0.Add(24 * time.Hour)
	if err := l.Record(ctx, Found{From: t0, To: t0.Add(24 * time.Hour * 7), Firings: []firing.Firing{firingOf("a", "shop", nil, at, 10)},
		Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: t0.Add(24 * time.Hour * 7)}}, opts); err != nil {
		t.Fatal(err)
	}
	l.Close()
	back := slices.Clone(ledgertest.Undo034)
	if strings.HasPrefix(where, "postgres") {
		c, err := pgx.Connect(ctx, where)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(back, "UPDATE schema_version SET version = 33") {
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
		for _, q := range append(back, "PRAGMA user_version = 33") {
			if _, err := l.w.ExecContext(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		l.Close()
	}
	l2, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatalf("migrating to 034: %v", err)
	}
	defer l2.Close()
	if k := respKeptOf(t, l2)["a"]; k != (respKept{0, 0, -1}) {
		t.Errorf("a firing from before: %+v", k)
	}
	if in, err := l2.ResponseInputs(ctx); err != nil || in.Any() {
		t.Errorf("inputs after migrating: %+v %v", in, err)
	}
	if got, err := l2.SyncResponses(ctx, ResponsesFile, "", []response.Page{page("P", "R", "a", at.Add(time.Minute), time.Time{}, at.Add(2*time.Minute), response.ByAlert)}, opts); err != nil || got.Added != 1 {
		t.Fatalf("a page after migrating: %+v %v", got, err)
	}
	if k := respKeptOf(t, l2)["a"]; k.response != int(response.Unanswered) {
		t.Errorf("after the page: %+v", k)
	}
}

// Responses show when the ledger keeps pages or changes read from the
// history, or rules are found noisy by what was done about their firings:
// deploys alone, serve's before responses, show none beside --noisy-by
// caught.
func TestResponseInputsShown(t *testing.T) {
	useful := verdict.Options{NoisyBy: verdict.NoisyByUseful}
	for _, c := range []struct {
		in           ResponseInputs
		caught, used bool
	}{
		{ResponseInputs{}, false, true},
		{ResponseInputs{Deploys: true}, false, true},
		{ResponseInputs{Pages: true}, true, true},
		{ResponseInputs{Rollouts: true}, true, true},
	} {
		if got := c.in.Shown(verdict.Options{}); got != c.caught {
			t.Errorf("%+v, caught: %v", c.in, got)
		}
		if got := c.in.Shown(useful); got != c.used {
			t.Errorf("%+v, useful: %v", c.in, got)
		}
	}
}

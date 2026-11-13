package ledger

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

func twoMinutes(string) time.Duration { return 2 * time.Minute }

func alert(rule, service string, start time.Time) Alert {
	return Alert{Rule: rule, Start: start, Labels: map[string]string{"alertname": rule, "service": service, "cluster": "eu-1"}}
}

func notify(t *testing.T, l *Ledger, n Notification) bool {
	t.Helper()
	kept, err := l.Notify(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	return kept
}

func reasons(t *testing.T, l *Ledger) []string {
	t.Helper()
	ns, err := l.Notifications(context.Background(), t0.Add(-time.Hour), t0.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range ns {
		out = append(out, n.Reason)
	}
	return out
}

// A notification from the second replica of an Alertmanager, within a
// minute, is the first one again; so is a message whose id is kept already.
// The same alerts later are a repeat.
func TestNotifyDuplicates(t *testing.T) {
	l := open(t, time.UTC)
	start := t0.Add(time.Hour)
	n := Notification{At: start.Add(time.Minute), Source: "alertmanager", Receiver: "oncall", Group: "{}:{alertname=\"A\"}",
		Alerts: []Alert{alert("A", "checkout", start)}}
	if !notify(t, l, n) {
		t.Fatal("the first wasn't kept")
	}
	twice := n
	twice.At = n.At.Add(40 * time.Second)
	if notify(t, l, twice) {
		t.Error("the same from another replica, 40 s later, was kept")
	}
	later := n
	later.At = n.At.Add(4 * time.Hour)
	if !notify(t, l, later) {
		t.Error("the same four hours later wasn't kept")
	}
	sns := Notification{At: start.Add(time.Hour), Source: "sns", Receiver: "oncall", Group: "g", Msg: "m-1",
		Alerts: []Alert{alert("A", "search", start)}}
	if !notify(t, l, sns) {
		t.Fatal("an SNS message wasn't kept")
	}
	sns.At = sns.At.Add(3 * time.Hour)
	if notify(t, l, sns) {
		t.Error("an SNS message sent again three hours later was kept")
	}
	if got, want := reasons(t, l), []string{ReasonFirst, ReasonFirst, ReasonRepeat}; !reflect.DeepEqual(got, want) {
		t.Errorf("reasons %v, want %v", got, want)
	}
}

// Without a reason from the source, it is worked out from the one before in
// the group; one the source gives is kept.
func TestNotifyReason(t *testing.T) {
	l := open(t, time.UTC)
	s := t0.Add(time.Hour)
	at := s
	send := func(reason string, alerts ...Alert) {
		t.Helper()
		at = at.Add(10 * time.Minute)
		if !notify(t, l, Notification{At: at, Source: "grafana", Receiver: "r", Group: "g", Reason: reason, Alerts: alerts}) {
			t.Fatal("not kept")
		}
	}
	a, b := alert("A", "checkout", s), alert("A", "search", s.Add(5*time.Minute))
	resolved := func(x Alert) Alert { x.Resolved = true; return x }
	send("", a)                         // first
	send("", a)                         // repeat
	send("unknown", a, b)               // new
	send("", a, resolved(b))            // resolved
	send("", resolved(a))               // resolved
	send("", a)                         // first again: all had resolved
	send(ReasonRepeat, a, b)            // as the source said
	send("", alert("B", "checkout", s)) // new: B wasn't in it
	want := []string{ReasonFirst, ReasonRepeat, ReasonNew, ReasonResolved, ReasonResolved, ReasonFirst, ReasonRepeat, ReasonNew}
	if got := reasons(t, l); !reflect.DeepEqual(got, want) {
		t.Errorf("reasons %v, want %v", got, want)
	}
}

func TestNotifyRefuses(t *testing.T) {
	l := open(t, time.UTC)
	ctx := context.Background()
	if _, err := l.Notify(ctx, Notification{At: t0, Source: "alertmanager"}); err == nil {
		t.Error("a notification without alerts was kept")
	}
	var many []Alert
	for i := 0; i <= MaxAlerts; i++ {
		many = append(many, alert("A", fmt.Sprint(i), t0))
	}
	if _, err := l.Notify(ctx, Notification{At: t0, Source: "alertmanager", Alerts: many}); err == nil {
		t.Error("1,001 alerts in one notification were kept")
	}
	if _, err := l.Notify(ctx, Notification{At: t0, Source: "alertmanager", Alerts: []Alert{{Rule: "A", Start: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}}}); err == nil {
		t.Error("an alert whose start isn't a time was kept")
	}
	if _, err := l.Notify(ctx, Notification{At: t0, Source: "sns", Copied: "Cloud", Alerts: []Alert{alert("A", "x", t0)}}); err == nil {
		t.Error("a notification naming a source no source could be named was kept")
	}
}

// An alert tells of its rule's firing whose labels are all among its own,
// started within two evaluations, the nearest; one that tells of none is
// untied. A notification is white when a firing it tells of caught
// something, black when each caught nothing. The stones don't change.
func TestPagesTie(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	s := t0.Add(2 * time.Hour)
	fs := []firing.Firing{
		{Rule: "A", Labels: map[string]string{"service": "checkout"}, Start: s, End: s.Add(20 * time.Minute)},
		{Rule: "A", Labels: map[string]string{"service": "checkout"}, Start: s.Add(3 * time.Minute), End: s.Add(25 * time.Minute)},
		{Rule: "A", Labels: map[string]string{"service": "search"}, Start: s.Add(time.Hour), End: s.Add(70 * time.Minute)},
	}
	record(t, l, t0, t0.Add(24*time.Hour), fs)
	if _, err := l.SyncFile(ctx, []incident.Incident{{ID: "INC-1", Title: "checkout down", Start: s.Add(-5 * time.Minute),
		End: s.Add(30 * time.Minute), Labels: incident.Labels{"service": {"checkout"}}}}, opts); err != nil {
		t.Fatal(err)
	}
	before, err := l.Report(ctx, t0, t0.Add(24*time.Hour), opts, true)
	if err != nil {
		t.Fatal(err)
	}
	at := s
	send := func(alerts ...Alert) {
		t.Helper()
		at = at.Add(time.Hour)
		if !notify(t, l, Notification{At: at, Source: "alertmanager", Receiver: "oncall", Group: fmt.Sprint(at.Unix()), Alerts: alerts}) {
			t.Fatal("not kept")
		}
	}
	send(alert("A", "checkout", s.Add(40*time.Second)))  // the first firing: nearer than the second
	send(alert("A", "checkout", s.Add(150*time.Second))) // the second: 30 s from it, 150 s from the first
	send(alert("A", "search", s.Add(time.Hour+time.Minute)))
	send(alert("A", "search", s.Add(time.Hour+5*time.Minute)))                    // too far from its firing
	send(Alert{Rule: "A", Start: s, Labels: map[string]string{"alertname": "A"}}) // lacks the firing's service
	send(alert("Gone", "checkout", s))                                            // a rule the ledger lacks
	send(alert("A", "checkout", s), alert("A", "search", s.Add(time.Hour)))       // white beside black is white

	ps, err := l.Pages(ctx, t0, t0.Add(30*24*time.Hour), twoMinutes)
	if err != nil {
		t.Fatal(err)
	}
	all, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	id := map[string]int64{}
	for _, f := range all {
		id[f.Labels["service"]+" "+f.Start.Sub(s).String()] = f.ID
	}
	want := []struct {
		stone   string
		firings []int64
	}{
		{PageWhite, []int64{id["checkout 0s"]}},
		{PageWhite, []int64{id["checkout 3m0s"]}},
		{PageBlack, []int64{id["search 1h0m0s"]}},
		{PageUntied, nil},
		{PageUntied, nil},
		{PageUntied, nil},
		{PageWhite, []int64{id["checkout 0s"], id["search 1h0m0s"]}},
	}
	if len(ps) != len(want) {
		t.Fatalf("%d pages, want %d: %+v", len(ps), len(want), ps)
	}
	for i, w := range want {
		if ps[i].Stone != w.stone || !reflect.DeepEqual(ps[i].Firings, w.firings) {
			t.Errorf("page %d: %s %v, want %s %v", i, ps[i].Stone, ps[i].Firings, w.stone, w.firings)
		}
	}

	of, err := l.NotifiedOf(ctx, all, twoMinutes)
	if err != nil {
		t.Fatal(err)
	}
	if got := map[int64]int{id["checkout 0s"]: 2, id["checkout 3m0s"]: 1, id["search 1h0m0s"]: 2}; !reflect.DeepEqual(of, got) {
		t.Errorf("notified of %v, want %v", of, got)
	}
	since, err := l.NotifiedSince(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// From when the first notification came, an hour after the earliest
	// alert started: the firings before the port was connected aren't
	// taken as unpaged.
	if !since["A"].Equal(s.Add(time.Hour)) || !since["Gone"].Equal(s.Add(time.Hour)) {
		t.Errorf("notified since %v", since)
	}

	after, err := l.Report(ctx, t0, t0.Add(24*time.Hour), opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("notifications changed the stones")
	}
}

// A run replaces its firings, and their ids: a notification ties to the
// firing found anew.
func TestPagesAfterRun(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	s := t0.Add(2 * time.Hour)
	if !notify(t, l, Notification{At: s.Add(time.Minute), Source: "alertmanager", Group: "g", Alerts: []Alert{alert("A", "checkout", s)}}) {
		t.Fatal("not kept")
	}
	ps, err := l.Pages(ctx, t0, t0.Add(24*time.Hour), twoMinutes)
	if err != nil || len(ps) != 1 || ps[0].Stone != PageUntied {
		t.Fatalf("before the run: %+v, %v", ps, err)
	}
	f := firing.Firing{Rule: "A", Labels: map[string]string{"service": "checkout"}, Start: s.Add(time.Minute)}
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{f})
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{f})
	ps, err = l.Pages(ctx, t0, t0.Add(24*time.Hour), twoMinutes)
	if err != nil || len(ps) != 1 || ps[0].Stone != PageBlack || len(ps[0].Firings) != 1 {
		t.Fatalf("after two runs: %+v, %v", ps, err)
	}
}

func TestPruneNotifications(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	for i := range 3 {
		at := t0.Add(time.Duration(i) * 24 * time.Hour)
		notify(t, l, Notification{At: at, Source: "alertmanager", Group: fmt.Sprint(i), Alerts: []Alert{alert("A", "checkout", at), alert("B", "x", at)}})
	}
	if err := l.Prune(ctx, t0.Add(36*time.Hour)); err != nil {
		t.Fatal(err)
	}
	ns, err := l.Notifications(ctx, time.Unix(0, 0), t0.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 || !ns[0].At.Equal(t0.Add(48*time.Hour)) {
		t.Errorf("kept %+v, want the last", ns)
	}
	var left int
	if err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM notified").Scan(&left); err != nil || left != 2 {
		t.Errorf("%d alerts left, want 2 (%v)", left, err)
	}
}

// TestNotificationsScale holds reading twelve weeks of notifications, tied
// to their firings, to a budget: 400 rules, about 100,000 firings, 20,000
// notifications, a third of them repeats.
func TestNotificationsScale(t *testing.T) {
	if testing.Short() || race {
		t.Skip("short, or under the race detector")
	}
	ctx := context.Background()
	l := open(t, time.UTC)
	start := t0.Add(-12 * 7 * 24 * time.Hour)
	var fs []firing.Firing
	for h := 0; h < 12*7*24; h++ {
		for i := range 100 {
			at := start.Add(time.Duration(h)*time.Hour + time.Duration(i*30)*time.Second)
			fs = append(fs, firing.Firing{Rule: fmt.Sprintf("Rule%d", (h*100+i)%400), Labels: map[string]string{"service": fmt.Sprint(i % 7)},
				Start: at, End: at.Add(10 * time.Minute)})
		}
	}
	for d := range 12 * 7 {
		from := start.Add(time.Duration(d) * 24 * time.Hour)
		var day []firing.Firing
		for _, f := range fs {
			if !f.Start.Before(from) && f.Start.Before(from.Add(24*time.Hour)) {
				day = append(day, f)
			}
		}
		record(t, l, from, from.Add(24*time.Hour), day)
	}
	// Kept a thousand to a write, as a source's are copied in: what is
	// held to a budget is reading them.
	began := time.Now()
	var ns []Notification
	for i := range 20000 {
		f := fs[(i*5)%len(fs)]
		at := f.Start.Add(30 * time.Second)
		if i%3 == 2 {
			f = fs[((i-1)*5)%len(fs)]
			at = f.Start.Add(4 * time.Hour)
		}
		a := Alert{Rule: f.Rule, Start: f.Start, Labels: map[string]string{"alertname": f.Rule, "service": f.Labels["service"], "cluster": "eu-1"}}
		ns = append(ns, Notification{At: at, Source: "alertmanager", Receiver: "oncall", Group: f.Rule + f.Labels["service"], Alerts: []Alert{a}})
		if len(ns) == 1000 || i == 19999 {
			kept, errs, err := l.NotifyAll(ctx, ns)
			if err != nil {
				t.Fatal(err)
			}
			for j := range ns {
				if errs[j] != nil || !kept[j] {
					t.Fatalf("notification %d: %v %v", i-len(ns)+1+j, kept[j], errs[j])
				}
			}
			ns = ns[:0]
		}
	}
	t.Logf("20,000 notifications kept in %v", time.Since(began))
	began = time.Now()
	ps, err := l.Pages(ctx, start, t0, twoMinutes)
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(began)
	untied := 0
	for _, p := range ps {
		if p.Stone == PageUntied {
			untied++
		}
	}
	t.Logf("%d pages over twelve weeks, %d untied, read in %v", len(ps), untied, took)
	if len(ps) != 20000 || untied != 0 {
		t.Errorf("%d pages, %d untied: want 20,000, none untied", len(ps), untied)
	}
	if budget := time.Duration(float64(3*time.Second) * slow); took > budget {
		t.Errorf("twelve weeks of notifications read in %v; budget %v", took, budget)
	}
}

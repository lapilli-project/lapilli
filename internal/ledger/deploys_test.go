package ledger

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// A firing is during a deploy that started from --deploy-before before it
// to --deploy-after after its end, and agrees with it on every label both
// have, one at least: a label the firing hasn't stands for any value. A
// deploy naming no label is everything's.
func TestDeployDuring(t *testing.T) {
	at := t0.Add(10 * time.Hour)
	d := Deploy{Start: at, End: at.Add(5 * time.Minute), Labels: map[string]string{"service": "checkout", "namespace": "shop"}}
	f := func(min int, labels map[string]string) firing.Firing {
		return firing.Firing{Rule: "R", Labels: labels, Start: at.Add(time.Duration(min) * time.Minute)}
	}
	for _, c := range []struct {
		name   string
		f      firing.Firing
		before time.Duration
		want   bool
	}{
		{"during, the service", f(2, map[string]string{"service": "checkout"}), 0, true},
		{"the service and the namespace", f(2, map[string]string{"service": "checkout", "namespace": "shop", "pod": "x"}), 0, true},
		{"after, within --deploy-after", f(15, map[string]string{"service": "checkout"}), 0, true},
		{"too long after", f(16, map[string]string{"service": "checkout"}), 0, false},
		{"before it", f(-1, map[string]string{"service": "checkout"}), 0, false},
		{"before it, within --deploy-before", f(-2, map[string]string{"service": "checkout"}), 2 * time.Minute, true},
		{"too long before", f(-3, map[string]string{"service": "checkout"}), 2 * time.Minute, false},
		{"the namespace's own alert", f(3, map[string]string{"namespace": "shop"}), 0, true},
		{"the namespace, another service", f(3, map[string]string{"namespace": "shop", "service": "cart"}), 0, false},
		{"another service and namespace", f(3, map[string]string{"namespace": "search", "service": "cart"}), 0, false},
		{"no label it names", f(3, map[string]string{"instance": "node-1"}), 0, false},
	} {
		if got := d.During(c.f, c.before, 10*time.Minute); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if !(Deploy{Start: at, End: at}).During(f(3, map[string]string{"instance": "node-1"}), 0, 10*time.Minute) {
		t.Error("a deploy naming no label should be everything's")
	}
}

// Deploys are kept as the file has them: a firing that came during one is
// marked, beside its stones, and counted by day and week, with those that
// caught something; read again, a deploy taken away unmarks its firings,
// and --deploy-after changed marks them by the new.
func TestSyncDeploys(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	o.DeployAfter = 10 * time.Minute
	at := t0.Add(10 * time.Hour)
	in := incident.Incident{ID: "INC-1", Start: at.Add(time.Minute), End: at.Add(10 * time.Minute), Labels: incident.Labels{"service": {"checkout"}}}
	if _, err := l.SyncFile(ctx, []incident.Incident{in}, o); err != nil {
		t.Fatal(err)
	}
	fs := []firing.Firing{
		// During, and catching the incident.
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at.Add(2 * time.Minute), End: at.Add(4 * time.Minute)},
		// During, the namespace's own, catching nothing.
		{Rule: "Latency", Labels: map[string]string{"namespace": "shop"}, Start: at.Add(12 * time.Minute), End: at.Add(20 * time.Minute)},
		// Another service in the namespace: not during the checkout deploy.
		{Rule: "Latency", Labels: map[string]string{"service": "cart", "namespace": "shop"}, Start: at.Add(13 * time.Minute), End: at.Add(20 * time.Minute)},
		// Five minutes after the search deploy.
		{Rule: "Latency", Labels: map[string]string{"service": "search"}, Start: at.Add(25 * time.Minute), End: at.Add(26 * time.Minute)},
		// Long before.
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at.Add(-3 * time.Hour), End: at.Add(-170 * time.Minute)},
	}
	f := Found{From: t0, To: t0.Add(24 * time.Hour), Firings: fs, Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: t0.Add(24 * time.Hour)}}
	if err := l.Record(ctx, f, o); err != nil {
		t.Fatal(err)
	}
	ds := []Deploy{
		{Start: at, End: at.Add(5 * time.Minute), Labels: map[string]string{"service": "checkout", "namespace": "shop"}, Revision: "4f2a9c1", Source: "checkout.txt"},
		{Start: at.Add(20 * time.Minute), Labels: map[string]string{"service": "search"}, Revision: "b7"},
	}
	changed, err := l.SyncDeploys(ctx, ds, o, DeploysRead{})
	if err != nil || !changed {
		t.Fatalf("SyncDeploys: %v %v", changed, err)
	}
	marks := func() map[string]bool {
		t.Helper()
		got, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, f := range got {
			out[f.Rule+" "+f.Labels["service"]+" "+f.Start.Format("15:04")] = f.Deploy
		}
		return out
	}
	want := map[string]bool{"Errors checkout 10:02": true, "Latency  10:12": true, "Latency cart 10:13": false, "Latency search 10:25": true, "Errors checkout 07:00": false}
	if got := marks(); !equalMarks(got, want) {
		t.Errorf("marks: %v, want %v", got, want)
	}
	// The same file again changes nothing.
	if changed, err := l.SyncDeploys(ctx, ds, o, DeploysRead{}); err != nil || changed {
		t.Errorf("the same deploys again: changed %v, %v", changed, err)
	}
	// Counted by day and week, those that caught something apart; and
	// in a report.
	days, err := l.Days(ctx, l.Day(t0), l.Day(t0))
	if err != nil {
		t.Fatal(err)
	}
	var dep, depWhite, white int
	for _, d := range days {
		dep, depWhite, white = dep+d.Deploy, depWhite+d.DeployWhite, white+d.White
	}
	if dep != 3 || depWhite != 1 || white != 1 {
		t.Errorf("days: %d during deploys, %d of them caught something (%d caught in all); want 3, 1 (1)", dep, depWhite, white)
	}
	weeks, err := l.Weeks(ctx, Week(l.Day(t0)), Week(l.Day(t0)))
	if err != nil || len(weeks) == 0 {
		t.Fatal(err)
	}
	dep = 0
	for _, w := range weeks {
		dep += w.Deploy
	}
	if dep != 3 {
		t.Errorf("weeks: %d during deploys, want 3", dep)
	}
	rep, err := l.Report(ctx, t0, t0.Add(24*time.Hour), o, false)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string][2]int{}
	for _, r := range rep.Rules {
		by[r.Rule] = [2]int{r.DuringDeploys, r.DeploysCaught}
	}
	if by["Errors"] != [2]int{1, 1} || by["Latency"] != [2]int{2, 0} {
		t.Errorf("report: %v", by)
	}
	// A report over part of a day reads the firings, not the days.
	rep, err = l.Report(ctx, at, at.Add(time.Hour), o, false)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rep.Rules {
		n += r.DuringDeploys
	}
	if n != 3 {
		t.Errorf("a report over an hour: %d during deploys, want 3", n)
	}
	// Taken away from the file: the search deploy's firing is no longer
	// during one.
	if _, err := l.SyncDeploys(ctx, ds[:1], o, DeploysRead{}); err != nil {
		t.Fatal(err)
	}
	want["Latency search 10:25"] = false
	if got := marks(); !equalMarks(got, want) {
		t.Errorf("after a deploy was taken away: %v, want %v", got, want)
	}
	// Put back, and --deploy-after shortened to 4m: the firings seven
	// minutes after the checkout deploy and five after the search one are
	// no longer during them.
	if _, err := l.SyncDeploys(ctx, ds, o, DeploysRead{}); err != nil {
		t.Fatal(err)
	}
	want["Latency search 10:25"] = true
	if got := marks(); !equalMarks(got, want) {
		t.Errorf("with it back: %v, want %v", got, want)
	}
	o.DeployAfter = 4 * time.Minute
	if changed, err := l.SyncDeploys(ctx, ds, o, DeploysRead{}); err != nil || !changed {
		t.Fatalf("a shorter --deploy-after: changed %v, %v", changed, err)
	}
	want["Latency search 10:25"], want["Latency  10:12"] = false, false
	if got := marks(); !equalMarks(got, want) {
		t.Errorf("with --deploy-after 4m: %v, want %v", got, want)
	}
	// And a run recorded over them again marks them the same way.
	if err := l.Record(ctx, f, o); err != nil {
		t.Fatal(err)
	}
	if got := marks(); !equalMarks(got, want) {
		t.Errorf("after a run again: %v, want %v", got, want)
	}
	if got, err := l.Deploys(ctx, t0, t0.Add(24*time.Hour)); err != nil || len(got) != 2 || got[0].Revision != "4f2a9c1" || got[0].Source != "checkout.txt" {
		t.Errorf("Deploys: %+v %v", got, err)
	}
}

func equalMarks(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// A quiet window's excused firing isn't counted during a deploy: it isn't
// judged at all.
func TestDeployExcused(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	o.DeployAfter = 10 * time.Minute
	at := t0.Add(10 * time.Hour)
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at.Add(time.Minute), End: at.Add(2 * time.Minute)}})
	if _, err := l.SyncDeploys(ctx, []Deploy{{Start: at, Labels: map[string]string{"service": "checkout"}}}, o, DeploysRead{}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutWindow(ctx, Window{Title: "deploy", Start: at, End: at.Add(time.Hour)}, "ana", o); err != nil {
		t.Fatal(err)
	}
	days, err := l.Days(ctx, l.Day(t0), l.Day(t0))
	if err != nil || len(days) != 1 {
		t.Fatal(days, err)
	}
	if days[0].Excused != 1 || days[0].Deploy != 0 {
		t.Errorf("an excused firing: %+v", days[0])
	}
}

// What the file holds is refused whole when a deploy in it is wrong, and
// the ledger keeps what it had.
func TestSyncDeploysRefused(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	o.DeployAfter = 10 * time.Minute
	good := Deploy{Start: t0, Labels: map[string]string{"service": "checkout"}}
	if _, err := l.SyncDeploys(ctx, []Deploy{good}, o, DeploysRead{}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []Deploy{
		{},
		{Start: t0, End: t0.Add(-time.Second)},
		{Start: t0, End: t0.Add(25 * time.Hour)},
		{Start: t0, Labels: map[string]string{"service": ""}},
		{Start: t0, Revision: string(make([]byte, 201))},
	} {
		if _, err := l.SyncDeploys(ctx, []Deploy{good, d}, o, DeploysRead{}); err == nil {
			t.Errorf("%+v taken", d)
		}
	}
	o.DeployAfter = 25 * time.Hour
	if _, err := l.SyncDeploys(ctx, []Deploy{good}, o, DeploysRead{}); err == nil {
		t.Error("a --deploy-after over a day taken")
	}
	if got, err := l.Deploys(ctx, t0.Add(-time.Hour), t0.Add(time.Hour)); err != nil || len(got) != 1 {
		t.Errorf("after refusals: %+v %v", got, err)
	}
}

// A rule's alerts that re-fired most, and any rule's, over a span.
func TestRefired(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	var fs []firing.Firing
	flap := func(rule, svc string, n int, from time.Time) {
		for i := 0; i < n; i++ {
			at := from.Add(time.Duration(i) * 30 * time.Minute)
			fs = append(fs, firing.Firing{Rule: rule, Labels: map[string]string{"service": svc}, Start: at, End: at.Add(10 * time.Minute)})
		}
	}
	flap("Errors", "checkout", 6, t0.Add(time.Hour))   // re-fires 5 times
	flap("Errors", "cart", 3, t0.Add(time.Hour))       // twice
	flap("Latency", "search", 4, t0.Add(10*time.Hour)) // three times
	record(t, l, t0, t0.Add(24*time.Hour), fs)
	got, err := l.RefiredOf(ctx, "Errors", t0, t0.Add(24*time.Hour), 5)
	if err != nil || len(got) != 2 || got[0].Labels["service"] != "checkout" || got[0].Refired != 5 || got[1].Refired != 2 {
		t.Errorf("RefiredOf: %+v %v", got, err)
	}
	most, err := l.MostRefired(ctx, t0, t0.Add(24*time.Hour), 2, nil)
	if err != nil || len(most) != 2 || most[0].Rule != "Errors" || most[1].Rule != "Latency" || most[1].Refired != 3 {
		t.Errorf("MostRefired: %+v %v", most, err)
	}
	only, err := l.MostRefired(ctx, t0, t0.Add(24*time.Hour), 1, func(r string) bool { return r == "Latency" })
	if err != nil || len(only) != 1 || only[0].Rule != "Latency" {
		t.Errorf("MostRefired of Latency: %+v %v", only, err)
	}
	// Over a span before them, none.
	if none, err := l.MostRefired(ctx, t0.Add(-24*time.Hour), t0, 5, nil); err != nil || len(none) != 0 {
		t.Errorf("before: %+v %v", none, err)
	}
}

// Deploys a few hours apart are judged as one span, a week at a time; a
// day apart, each on its own.
func TestNear(t *testing.T) {
	h := int64(3600)
	got := near([][2]int64{{30 * h, 31 * h}, {0, h}, {4 * h, 5 * h}, {9 * h, 10 * h}, {60 * h, 61 * h}})
	want := [][2]int64{{0, 10 * h}, {30 * h, 31 * h}, {60 * h, 61 * h}}
	if len(got) != len(want) {
		t.Fatalf("%v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%v, want %v", got, want)
		}
	}
}

// A deploy starting more than an hour after the file is read is refused;
// so are labels over 8 MB in all, counted for each deploy, though each
// deploy's are under 4 KB.
func TestDeploysBounded(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	o.DeployAfter = 10 * time.Minute
	read := DeploysRead{At: t0}
	if _, err := l.SyncDeploys(ctx, []Deploy{{Start: t0.Add(59 * time.Minute)}}, o, read); err != nil {
		t.Errorf("a deploy within the hour: %v", err)
	}
	if _, err := l.SyncDeploys(ctx, []Deploy{{Start: t0.Add(61 * time.Minute)}}, o, read); err == nil {
		t.Error("a deploy starting after the hour taken")
	}
	big := map[string]string{"service": string(make([]byte, 0, 1))}
	for i := 0; i < 15; i++ {
		big[fmt.Sprintf("l%02d", i)] = strings.Repeat("v", 250)
	}
	var ds []Deploy
	for i := 0; i < 3000; i++ { // 3,000 × about 3.8 KB: over 8 MB
		ds = append(ds, Deploy{Start: t0.Add(-time.Duration(i) * time.Minute), Labels: big})
	}
	big["service"] = "a"
	if _, err := l.SyncDeploys(ctx, ds, o, read); err == nil || !strings.Contains(err.Error(), "MB in all") {
		t.Errorf("8 MB of labels and more: %v", err)
	}
	if _, err := l.SyncDeploys(ctx, ds[:2000], o, read); err != nil {
		t.Errorf("under 8 MB: %v", err)
	}
	// A history's lines share their labels: kept once.
	var n int
	l.r.QueryRowContext(ctx, "SELECT count(*) FROM deploy_labels").Scan(&n)
	if n != 1 {
		t.Errorf("%d label sets kept for 2,000 deploys of one", n)
	}
}

// A read whose digest is the last one's, with the same times before and
// after, changes nothing and reads nothing: what it holds isn't looked at.
// Another digest is read; and so is the same one with another time after.
func TestDeploysUnchanged(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	o.DeployAfter = 10 * time.Minute
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "R", Labels: map[string]string{"service": "a"}, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}})
	ds := []Deploy{{Start: t0.Add(55 * time.Minute), Labels: map[string]string{"service": "a"}}}
	if changed, err := l.SyncDeploys(ctx, ds, o, DeploysRead{Sum: "s1"}); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if changed, err := l.SyncDeploys(ctx, nil, o, DeploysRead{Sum: "s1"}); err != nil || changed {
		t.Errorf("the same digest: changed %v, %v", changed, err)
	}
	if got, _ := l.Deploys(ctx, t0, t0.Add(24*time.Hour)); len(got) != 1 {
		t.Errorf("the same digest took the deploys away: %+v", got)
	}
	o.DeployAfter = 20 * time.Minute
	if changed, err := l.SyncDeploys(ctx, ds, o, DeploysRead{Sum: "s1"}); err != nil || !changed {
		t.Errorf("the same digest, another --deploy-after: changed %v, %v", changed, err)
	}
	if changed, err := l.SyncDeploys(ctx, nil, o, DeploysRead{Sum: "s2"}); err != nil || !changed {
		t.Errorf("another digest: changed %v, %v", changed, err)
	}
	if got, _ := l.Deploys(ctx, t0, t0.Add(24*time.Hour)); len(got) != 0 {
		t.Errorf("another digest, with none: %+v", got)
	}
}

// Deploys after the newest firing pend nothing: a run judges its own
// period. Many short spans are judged a few hundred to a write, the list
// written back once each, not once a span. The latest deploy is of those
// started by now.
func TestDeploysPending(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	o.DeployAfter = 10 * time.Minute
	read := DeploysRead{At: t0.Add(400 * 24 * time.Hour)}
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "R", Labels: map[string]string{"service": "a"}, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}})
	var later []Deploy
	for i := 0; i < 50; i++ {
		later = append(later, Deploy{Start: t0.Add(48*time.Hour + time.Duration(i)*7*time.Hour), Labels: map[string]string{"service": "a"}})
	}
	newest := t0.Add(time.Hour).Unix()
	past := 0
	judging = func(lo, hi int64) {
		if lo > newest {
			past++
		}
	}
	defer func() { judging = nil }()
	if _, err := l.SyncDeploys(ctx, later, o, read); err != nil {
		t.Fatal(err)
	}
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	if past > 0 {
		t.Errorf("deploys after the newest firing judged %d spans", past)
	}
	judging = nil
	// Firings every seven hours for 300 days: 1,000 short spans.
	var fs []firing.Firing
	var ds []Deploy
	for i := 0; i < 1000; i++ {
		at := t0.Add(-300*24*time.Hour + time.Duration(i)*7*time.Hour)
		fs = append(fs, firing.Firing{Rule: "R", Labels: map[string]string{"service": "a"}, Start: at.Add(time.Minute), End: at.Add(2 * time.Minute)})
		ds = append(ds, Deploy{Start: at, Labels: map[string]string{"service": "a"}})
	}
	record(t, l, t0.Add(-301*24*time.Hour), t0.Add(-time.Hour), fs)
	gen := l.Gen()
	if _, err := l.SyncDeploys(ctx, append(ds, later...), o, read); err != nil {
		t.Fatal(err)
	}
	if err := l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	if writes := l.Gen() - gen; writes > 10 {
		t.Errorf("1,000 spans judged in %d writes", writes)
	}
	var marked int
	l.r.QueryRowContext(ctx, "SELECT count(*) FROM firings WHERE deploy = 1").Scan(&marked)
	if marked != 1000 {
		t.Errorf("%d of 1,000 firings marked", marked)
	}
	last, err := l.LastDeploy(ctx, t0.Add(60*time.Hour))
	if err != nil || !last.Equal(t0.Add(48*time.Hour+7*time.Hour)) {
		t.Errorf("the latest deploy by then: %s %v", last, err)
	}
}

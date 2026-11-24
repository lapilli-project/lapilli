package verdict

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
)

// A span's record is kept by an incident on at some time from two weeks
// before it to its end, one with no end taken to last as long as a firing
// catches it for (UnknownLen); or by an impact signal read over all of it.
// A firing's, by one on in the two weeks before it.
func TestRecordKept(t *testing.T) {
	day := 24 * time.Hour
	week := [2]time.Time{t0, t0.Add(7 * day)}
	in := func(start, end time.Time) []incident.Incident {
		return []incident.Incident{{ID: "I", Start: start, End: end}}
	}
	for _, c := range []struct {
		name string
		span [2]time.Time
		incs []incident.Incident
		want bool
	}{
		{"in the week", week, in(t0.Add(day), t0.Add(day+time.Hour)), true},
		{"in the week before", week, in(t0.Add(-6*day), t0.Add(-6*day+time.Hour)), true},
		{"two weeks before it, just", week, in(t0.Add(-14*day), t0.Add(-14*day+time.Hour)), true},
		{"ended before that", week, in(t0.Add(-15*day), t0.Add(-14*day-time.Second)), false},
		{"started before that, on into it", week, in(t0.Add(-20*day), t0.Add(-13*day)), true},
		{"started 20 days before, with no end", week, in(t0.Add(-20*day), time.Time{}), false},
		{"started 15 days before, with no end", week, in(t0.Add(-15*day), time.Time{}), true},
		{"started 15 days and 2 hours before, with no end", week, in(t0.Add(-15*day-2*time.Hour), time.Time{}), false},
		{"at its end", week, in(t0.Add(7*day), t0.Add(7*day+time.Hour)), false},
		{"none", week, nil, false},
	} {
		if got := RecordOf(c.incs, Options{UnknownLen: day}).Kept(c.span[0], c.span[1]); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	// An impact signal read from two weeks before keeps all of it, found or
	// not; read from later, only what it covers.
	if r := RecordOf(nil, Options{ImpactFrom: RecordFrom(week[0], week[1])}); !r.Kept(week[0], week[1]) || !r.At(t0.Add(day)) {
		t.Error("impact read over it keeps it")
	}
	if r := RecordOf(nil, Options{ImpactFrom: t0.Add(3 * day)}); r.Kept(week[0], week[1]) || r.At(t0.Add(day)) || !r.At(t0.Add(4*day)) {
		t.Error("impact read from midweek keeps what it covers")
	}
	// Judging each firing by its own time, read from midweek on, some of
	// the week's firings are judged; from its end, or not read, none; by
	// week, none of a week it wasn't read all of.
	for _, c := range []struct {
		name string
		o    Options
		want bool
	}{
		{"from midweek", Options{ImpactFrom: t0.Add(3 * day)}, true},
		{"from its end", Options{ImpactFrom: week[1]}, false},
		{"not read", Options{}, false},
		{"from midweek, by week", Options{ImpactFrom: t0.Add(3 * day), RecordWeek: func(time.Time) (time.Time, time.Time) { return week[0], week[1] }}, false},
	} {
		if got := RecordOf(nil, c.o).JudgesAny(week[0], week[1]); got != c.want {
			t.Errorf("judges any, %s: %v", c.name, got)
		}
	}
	// A firing: one on in the two weeks before it, not after it.
	r := RecordOf(in(t0.Add(10*day), t0.Add(10*day+time.Hour)), Options{})
	for at, want := range map[time.Duration]bool{9 * day: false, 10 * day: true, 20 * day: true, 25 * day: false} {
		if r.At(t0.Add(at)) != want {
			t.Errorf("firing at day %d: %v", at/day, !want)
		}
	}
}

// With no incident to judge them by, firings are counted, not judged: a
// rule that fires as often as a noisy one isn't noisy, has no precision,
// and says why; the text says it once.
func TestNoRecord(t *testing.T) {
	var fs []firing.Firing
	for i := range 30 {
		fs = append(fs, firing.Firing{Rule: "Restarts", Labels: map[string]string{"service": "a"}, Start: m(60 * i), End: m(60*i + 10)})
	}
	known := []rules.Rule{{Name: "Restarts", Expr: "x"}, {Name: "Quiet", Expr: "y"}}
	rep := Judge(known, fs, nil, opts())
	if !rep.NoRecord {
		t.Fatal("no incident: no record")
	}
	var r RuleResult
	for _, x := range rep.Rules {
		if x.Rule == "Restarts" {
			r = x
		}
		if x.Rule == "Quiet" && x.NoRecord {
			t.Error("a rule that never fired has nothing unjudged")
		}
	}
	if r.Noisy || !r.NoRecord || r.Firings != 30 || r.Black != 30 {
		t.Errorf("Restarts: %+v", r)
	}
	if _, ok := r.Precision(); ok {
		t.Error("no precision without a record")
	}
	var b strings.Builder
	if err := Text(&b, rep, time.UTC); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Count(out, NoRecordText) != 1 || strings.Contains(out, "0%") {
		t.Errorf("the text should say it once, and give no precision:\n%s", out)
	}

	// One written down a week before keeps a week's record: judged again.
	before := []incident.Incident{{ID: "INC-1", Start: t0.Add(-3 * 24 * time.Hour), End: t0.Add(-3*24*time.Hour + time.Hour)}}
	rep = Judge(known, fs, before, opts())
	if rep.NoRecord || !rep.Rules[0].Noisy || rep.Rules[0].NoRecord {
		t.Errorf("an incident in the two weeks before: judged, and noisy: %+v %+v", rep.NoRecord, rep.Rules[0])
	}
	b.Reset()
	if err := Text(&b, rep, time.UTC); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), NoRecordText) {
		t.Errorf("a record kept says nothing of it:\n%s", b.String())
	}
}

// A comparison given incidents, none of which keeps the period's record,
// counts the firings and doesn't judge them, and says why.
func TestCompareNoRecord(t *testing.T) {
	fs := []firing.Firing{{Rule: "A", Start: m(1), End: m(2)}}
	before := Judge([]rules.Rule{{Name: "A", Expr: "x"}}, fs, nil, opts())
	after := Judge([]rules.Rule{{Name: "A", Expr: "x > 1"}}, nil, nil, opts())
	c := Compare(before, after, []string{"A"}, 0)
	c.Summarize(true)
	if c.judged() || !c.Summary.NoRecord || !c.Summary.IncidentsGiven {
		t.Errorf("no record: not judged, and the summary says why: %+v", c.Summary)
	}
	var b strings.Builder
	if err := CompareText(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), NoRecordCompared) || strings.Contains(b.String(), "PRECISION") {
		t.Errorf("text:\n%s", b.String())
	}
	b.Reset()
	if err := CompareMarkdown(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), NoRecordCompared) || strings.Contains(b.String(), "No incidents were given") {
		t.Errorf("markdown:\n%s", b.String())
	}
}

// A rule's alerts are named by the labels that tell them apart, the most
// firings first, five at most; one alone, by its own. Of a condition that
// counts events, every firing happened; of another, how many stayed well
// past the bound says so, and none past it doesn't say they flickered.
func TestTopAlerts(t *testing.T) {
	var all []AlertCount
	for i := range 7 {
		all = append(all, AlertCount{Labels: map[string]string{"alertname": "R", "team": "infra", "pod": fmt.Sprintf("p%d", i), "ns": "a"}, Firings: i + 1})
	}
	top := TopAlerts(all)
	if len(top) != TopShown || top[0].Firings != 7 || !reflect.DeepEqual(top[0].Labels, map[string]string{"pod": "p6"}) {
		t.Errorf("%+v", top)
	}
	top = TopAlerts(all[:1])
	if !reflect.DeepEqual(top[0].Labels, map[string]string{"pod": "p0", "ns": "a", "team": "infra"}) {
		t.Errorf("one: %+v", top)
	}
	u := Unrecorded{Rule: "R", Firings: 12, Alerts: 1, Top: top}
	if got := u.Text(); got != "Fired 12 times with no incident written down to judge them by. All came from one alert: ns=a pod=p0 team=infra. Write down whether they mattered, or fix what fires them." {
		t.Errorf("%q", got)
	}
	if got := u.Prose(); strings.Contains(got, "p0") || !strings.Contains(got, "They came from 1 alerts.") {
		t.Errorf("prose %q", got)
	}
	zero, twelve := 0, 12
	u.Clear, u.Bound = &zero, "> 0.5"
	if got := u.Text(); !strings.Contains(got, "None stayed more than a fifth past its bound (> 0.5) for all of its for.") || strings.Contains(got, "would hide") {
		t.Errorf("%q", got)
	}
	u.Clear = &twelve
	if got := u.Text(); !strings.Contains(got, "All 12 went well past its bound (> 0.5), not a flicker at its edge: these happened.") || !strings.Contains(got, "would hide") {
		t.Errorf("%q", got)
	}
	u.Events, u.Bound = true, ""
	if got := u.Text(); !strings.Contains(got, "All 12 are events its condition counts (a count that went up): these happened.") || !strings.Contains(got, "would hide") {
		t.Errorf("events: %q", got)
	}
}

// TopAlerts is one pass over the alerts, the most read TopRead: 40,000
// alerts with labels of their own each, a few dozen allocations for each
// of the TopRead read (their labels' text) at most, not for each of the
// 40,000. Counted (testing.AllocsPerRun), not timed, so that a slow
// runner fails none; the time is logged.
func TestTopAlertsBounded(t *testing.T) {
	all := make([]AlertCount, 40000)
	for i := range all {
		all[i] = AlertCount{Labels: map[string]string{fmt.Sprintf("l%d", i): "v", "pod": fmt.Sprint(i)}, Firings: i % 97}
	}
	start := time.Now()
	top := TopAlerts(all)
	if len(top) != TopShown {
		t.Errorf("%d shown, want %d", len(top), TopShown)
	}
	// Some 9,000 (11,000 under the race detector); for each of the 40,000,
	// some 330,000.
	n := testing.AllocsPerRun(3, func() { TopAlerts(all) })
	t.Logf("40,000 alerts: %s, %.0f allocations", time.Since(start), n)
	if n > 40*TopRead {
		t.Errorf("40,000 alerts: %.0f allocations, over %d: each alert's labels read, not TopRead's?", n, 40*TopRead)
	}
}

// Over 28 days with one incident written down, on day 20, only the firings
// from it on are judged: those before it are counted, not judged, and in no
// precision; the text says how many. An early incident doesn't judge what
// came weeks after it, either.
func TestJudgeEachFiring(t *testing.T) {
	day := 24 * time.Hour
	o := opts()
	o.To = t0.Add(28 * day)
	var fs []firing.Firing
	for d := range 28 {
		at := t0.Add(time.Duration(d)*day + 12*time.Hour)
		fs = append(fs, firing.Firing{Rule: "Daily", Labels: map[string]string{"service": "a"}, Start: at, End: at.Add(time.Minute)})
	}
	incs := []incident.Incident{{ID: "INC-1", Start: t0.Add(20 * day), End: t0.Add(20*day + time.Hour), Labels: incident.Labels{"service": {"b"}}}}
	rep := Judge(nil, fs, incs, o)
	r := rep.Rules[0]
	if rep.NoRecord || rep.Unjudged != 20 || r.Unjudged != 20 || r.Judged() != 8 || r.Firings != 28 || r.NoRecord {
		t.Fatalf("%v %d %+v", rep.NoRecord, rep.Unjudged, r)
	}
	if p, ok := r.Precision(); !ok || p != 0 {
		t.Errorf("precision of the 8 judged: %v %v", p, ok)
	}
	if r.Noisy {
		t.Error("8 judged firings, under ten: not noisy")
	}
	for _, j := range rep.Firings {
		if want := j.Start.Before(t0.Add(20 * day)); j.NoRecord != want {
			t.Errorf("%s: no record %v", j.Start, j.NoRecord)
		}
	}
	var b strings.Builder
	if err := Text(&b, rep, time.UTC); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "20 of the 28 firings came with no incident written down in the two weeks before them, and caught none") ||
		strings.Contains(b.String(), NoRecordText) {
		t.Errorf("text:\n%s", b.String())
	}
	// One on day 0 judges two weeks of firings, and not the two after.
	incs[0].Start, incs[0].End = t0.Add(-time.Hour), t0
	rep = Judge(nil, fs, incs, o)
	if r := rep.Rules[0]; r.Unjudged != 14 || r.Judged() != 14 || !r.Noisy {
		t.Errorf("an incident before day 0: %+v", r)
	}
	// With no end, it lasts UnknownLen (an hour), for its record as for
	// what catches it: not on for all the 28 days.
	incs[0].End = time.Time{}
	rep = Judge(nil, fs, incs, o)
	if r := rep.Rules[0]; r.Unjudged != 14 || r.Judged() != 14 {
		t.Errorf("an incident with no end: %+v", r)
	}
}

// Judged by week (RecordWeek), as lapilli serve keeps its stones: an
// incident on in a week or the two before it judges all of the week's
// firings, one on Monday's too, and none of the next week's past two weeks
// from it; a period with no week kept has no record.
func TestJudgeByWeek(t *testing.T) {
	day := 24 * time.Hour
	week := func(at time.Time) (time.Time, time.Time) {
		n := at.Sub(t0) / (7 * day)
		if at.Before(t0) {
			n--
		}
		from := t0.Add(n * 7 * day)
		return from, from.Add(7 * day)
	}
	o := opts()
	o.To, o.RecordWeek = t0.Add(28*day), week
	var fs []firing.Firing
	for d := range 28 {
		at := t0.Add(time.Duration(d)*day + 12*time.Hour)
		fs = append(fs, firing.Firing{Rule: "Daily", Labels: map[string]string{"service": "a"}, Start: at, End: at.Add(time.Minute)})
	}
	// On day 20, Saturday of the third week: by firing, days 20 to 27 are
	// judged; by week, the third and fourth weeks, days 14 to 27.
	incs := []incident.Incident{{ID: "INC-1", Start: t0.Add(20 * day), End: t0.Add(20*day + time.Hour), Labels: incident.Labels{"service": {"b"}}}}
	rep := Judge(nil, fs, incs, o)
	if r := rep.Rules[0]; rep.NoRecord || r.Unjudged != 14 || r.Judged() != 14 || !r.Noisy {
		t.Errorf("by week: %v %+v", rep.NoRecord, r)
	}
	for _, j := range rep.Firings {
		if want := j.Start.Before(t0.Add(14 * day)); j.NoRecord != want {
			t.Errorf("%s: no record %v", j.Start, j.NoRecord)
		}
	}
	// None kept: no record, though the period has some of a week.
	o.From, o.To = t0.Add(2*day), t0.Add(5*day)
	if rep := Judge(nil, fs[2:5], nil, o); !rep.NoRecord || rep.Unjudged != 3 {
		t.Errorf("none: %v %d", rep.NoRecord, rep.Unjudged)
	}
	// One in the period's week, after the period's end: the week is kept.
	after := []incident.Incident{{ID: "INC-2", Start: t0.Add(6 * day), End: t0.Add(6*day + time.Hour)}}
	if rep := Judge(nil, fs[2:5], after, o); rep.NoRecord || rep.Unjudged != 0 || len(rep.Incidents) != 0 {
		t.Errorf("one later in the week: %v %d %+v", rep.NoRecord, rep.Unjudged, rep.Incidents)
	}
}

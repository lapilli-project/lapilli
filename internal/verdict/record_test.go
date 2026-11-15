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

// A period's record is kept by an incident in it, or in the two weeks
// before its end; a period longer than two weeks, by one in it.
func TestRecordKept(t *testing.T) {
	day := 24 * time.Hour
	week := [2]time.Time{t0, t0.Add(7 * day)}
	for _, c := range []struct {
		name  string
		span  [2]time.Time
		start time.Time
		want  bool
	}{
		{"in the week", week, t0.Add(day), true},
		{"in the week before", week, t0.Add(-6 * day), true},
		{"two weeks before its end, just", week, t0.Add(-7 * day), true},
		{"before that", week, t0.Add(-7*day - time.Second), false},
		{"at its end", week, t0.Add(7 * day), false},
		{"in a long period", [2]time.Time{t0, t0.Add(28 * day)}, t0.Add(day), true},
		{"before a long period", [2]time.Time{t0, t0.Add(28 * day)}, t0.Add(-time.Second), false},
	} {
		if got := RecordKept(c.span[0], c.span[1], []time.Time{c.start}); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	if RecordKept(t0, t0.Add(7*day), nil) {
		t.Error("no incident keeps nothing")
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
	if c.judged() {
		t.Error("no record: not judged")
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
// firings first, five at most; one alone, by its own.
func TestTopAlerts(t *testing.T) {
	var all []AlertCount
	for i := range 7 {
		all = append(all, AlertCount{Labels: map[string]string{"alertname": "R", "team": "infra", "pod": fmt.Sprintf("p%d", i), "ns": "a"}, Firings: i + 1})
	}
	n, top := TopAlerts(all)
	if n != 7 || len(top) != TopShown || top[0].Firings != 7 || !reflect.DeepEqual(top[0].Labels, map[string]string{"pod": "p6"}) {
		t.Errorf("%d %+v", n, top)
	}
	n, top = TopAlerts(all[:1])
	if n != 1 || !reflect.DeepEqual(top[0].Labels, map[string]string{"pod": "p0", "ns": "a", "team": "infra"}) {
		t.Errorf("one: %d %+v", n, top)
	}
	u := Unrecorded{Rule: "R", Firings: 12, Alerts: 1, Top: top}
	if got := u.Text(); got != "Fired 12 times with no incident written down to judge them by. All came from one alert: ns=a pod=p0 team=infra. Write down whether they mattered, or fix what fires them." {
		t.Errorf("%q", got)
	}
	zero, all7 := 0, 12
	u.Clear, u.Bound = &zero, "> 0.5"
	if got := u.Text(); !strings.Contains(got, "None went well past its bound (> 0.5): each came within a fifth of it.") || strings.Contains(got, "would hide") {
		t.Errorf("%q", got)
	}
	u.Clear = &all7
	if got := u.Text(); !strings.Contains(got, "All 12 went well past its bound (> 0.5), not a flicker at its edge: these happened.") || !strings.Contains(got, "would hide") {
		t.Errorf("%q", got)
	}
}

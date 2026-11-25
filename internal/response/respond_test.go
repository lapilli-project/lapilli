package response

import (
	"math"
	"testing"
	"time"

	"lapilli.dev/internal/deploys"
	"lapilli.dev/internal/incident"
)

var (
	t0     = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	period = 10 * time.Hour
	web    = Workload{"shop", KindDeployment, "web"}
)

func rollout(w Workload, at time.Time) Change {
	return Change{Kind: Rollout, At: at, Namespace: w.Namespace, WorkloadKind: w.Kind, Workload: w.Name}
}

// changes is a set over the period holding the rollouts given, each
// workload's covered from the period's start.
func changes(cs ...Change) *ChangeSet {
	set := NewChangeSet(t0, t0.Add(period))
	r := &Rollouts{Changes: cs, Coverage: map[Workload][]Span{}, Owners: map[Pod]Workload{}}
	for _, c := range cs {
		r.Coverage[Workload{c.Namespace, c.WorkloadKind, c.Workload}] = []Span{{t0, t0.Add(period)}}
	}
	set.AddRollouts(r)
	return set
}

func TestRespondBoundaries(t *testing.T) {
	start, end := t0.Add(2*time.Hour), t0.Add(3*time.Hour)
	f := Firing{Rule: "Err", Labels: map[string]string{"namespace": "shop", "deployment": "web"}, Start: start, End: end}
	now := t0.Add(9 * time.Hour)
	for _, c := range []struct {
		name string
		at   time.Time
		on   bool
		want Type
	}{
		{"exactly at the start does not count", start, false, None},
		{"a second after the start counts", start.Add(time.Second), false, Changed},
		{"before the start does not", start.Add(-time.Second), false, None},
		{"exactly at the end counts", end, false, Changed},
		{"a second after the end does not", end.Add(time.Second), false, None},
		{"for a firing still on, now is its end", now, true, Changed},
		{"for a firing still on, after now doesn't count", now.Add(time.Second), true, None},
		{"a firing still on ignores the End it carries", end.Add(time.Hour), true, Changed},
	} {
		g := f
		g.On = c.on
		e := Evidence{Changes: changes(rollout(web, c.at)), Routine: DefaultRoutine}
		r := e.Respond(g, 4*time.Minute, now)
		if r.Type != c.want {
			t.Errorf("%s: %v, want %v", c.name, r.Type, c.want)
		}
		if c.want == Changed && (r.Change == nil || !r.Change.At.Equal(c.at)) {
			t.Errorf("%s: change %+v", c.name, r.Change)
		}
	}
}

func TestRespondMatching(t *testing.T) {
	start, end := t0.Add(time.Hour), t0.Add(2*time.Hour)
	mid := t0.Add(90 * time.Minute)
	db := Workload{"shop", KindStatefulSet, "db"}
	agent := Workload{"shop", KindDaemonSet, "agent"}
	cs := NewChangeSet(t0, t0.Add(period))
	cs.AddRollouts(&Rollouts{
		Changes:  []Change{rollout(web, mid), rollout(db, mid), rollout(agent, mid), rollout(Workload{"shop", KindDeployment, "checkout"}, mid)},
		Coverage: map[Workload][]Span{web: {{t0, t0.Add(period)}}},
		Owners: map[Pod]Workload{
			{"shop", "web-7d4-abc"}: web,
			{"shop", "db-0"}:        db,
		},
	})
	cs.AddDeploys([]deploys.Deploy{
		{Start: mid, End: mid, Labels: map[string]string{"service": "payments", "namespace": "pay"}, Revision: "abc1234"},
	})
	for _, c := range []struct {
		name   string
		labels map[string]string
		match  []string
		want   string // the workload or revision changed, "" for none
	}{
		{"direct deployment label", map[string]string{"namespace": "shop", "deployment": "web"}, nil, "web"},
		{"direct statefulset label", map[string]string{"namespace": "shop", "statefulset": "db"}, nil, "db"},
		{"direct daemonset label", map[string]string{"namespace": "shop", "daemonset": "agent"}, nil, "agent"},
		{"a name of another kind is not the workload", map[string]string{"namespace": "shop", "statefulset": "web"}, nil, ""},
		{"the namespace is required", map[string]string{"deployment": "web"}, nil, ""},
		{"another namespace: no workload of shop (the deploy of pay ties by its namespace)", map[string]string{"namespace": "pay", "deployment": "web"}, nil, "abc1234"},
		{"a pod a Deployment owns", map[string]string{"namespace": "shop", "pod": "web-7d4-abc"}, nil, "web"},
		{"a pod a StatefulSet owns", map[string]string{"namespace": "shop", "pod": "db-0"}, nil, "db"},
		{"a pod nobody known owns", map[string]string{"namespace": "shop", "pod": "x-1"}, nil, ""},
		{"a pod in another namespace is not shop's (the deploy of pay ties)", map[string]string{"namespace": "pay", "pod": "web-7d4-abc"}, nil, "abc1234"},
		{"service is not a workload unless named", map[string]string{"namespace": "shop", "service": "checkout"}, nil, ""},
		{"--rollout-match names a label", map[string]string{"namespace": "shop", "service": "checkout"}, []string{"service"}, "checkout"},
		{"--rollout-match: the label's value must be the name", map[string]string{"namespace": "shop", "service": "cart"}, []string{"service"}, ""},
		{"--rollout-match needs the namespace too", map[string]string{"service": "checkout"}, []string{"service"}, ""},
		{"a deploy shares a label", map[string]string{"service": "payments"}, nil, "abc1234"},
		{"a deploy shares all it has of both", map[string]string{"service": "payments", "namespace": "pay"}, nil, "abc1234"},
		{"a deploy disagrees on a shared label", map[string]string{"service": "payments", "namespace": "shop"}, nil, ""},
		{"a deploy shares none", map[string]string{"team": "x"}, nil, ""},
	} {
		e := Evidence{Changes: cs, Match: c.match, Routine: DefaultRoutine}
		r := e.Respond(Firing{Rule: "R", Labels: c.labels, Start: start, End: end}, 0, end)
		got := ""
		if r.Change != nil {
			got = r.Change.Workload
			if r.Change.Kind == Deploy {
				got = r.Change.Revision
			}
		}
		if got != c.want {
			t.Errorf("%s: changed %q, want %q (%v)", c.name, got, c.want, r.Type)
		}
	}
	// A deploy that names no label is everything's.
	all := NewChangeSet(t0, t0.Add(period))
	all.AddDeploys([]deploys.Deploy{{Start: mid, End: mid}})
	r := Evidence{Changes: all, Routine: DefaultRoutine}.Respond(Firing{Rule: "R", Labels: map[string]string{"a": "b"}, Start: start, End: end}, 0, end)
	if r.Type != Changed || r.Change.Kind != Deploy {
		t.Errorf("a deploy with no labels: %+v", r)
	}
}

func TestRoutineMaths(t *testing.T) {
	T := 10 * time.Hour
	h := time.Hour
	chance := func(n int, s time.Duration) float64 { return 1 - math.Exp(-float64(n-1)*s.Seconds()/T.Seconds()) }
	for _, c := range []struct {
		name      string
		n         int
		s         time.Duration
		threshold float64
		want      bool
	}{
		{"n of 1 is never routine", 1, 9 * h, 0.01, false},
		{"n of 0", 0, h, 0.01, false},
		{"eleven changes, an hour: 63%", 11, h, 0.5, true},
		{"three changes, an hour: 18%", 3, h, 0.5, false},
		{"at the threshold exactly", 6, 2 * h, chance(6, 2*h), true},
		{"just over the threshold", 6, 2 * h, math.Nextafter(chance(6, 2*h), 1), false},
		{"a long firing makes any repeat routine", 2, 100 * h, 0.5, true},
		{"a firing no longer than a moment", 50, 0, 0.5, false},
	} {
		if got := Routine(c.n, c.s, T, c.threshold); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if Routine(5, h, 0, 0.5) {
		t.Error("no period was routine")
	}

	// Through a response: eleven rollouts of a target in the period, a
	// two-hour firing meeting one; and the same target changing twice.
	start := t0.Add(4 * time.Hour)
	f := Firing{Rule: "R", Labels: map[string]string{"namespace": "shop", "deployment": "web"}, Start: start, End: start.Add(h)}
	var many []Change
	for i := 0; i < 11; i++ {
		many = append(many, rollout(web, t0.Add(time.Duration(i)*time.Hour).Add(30*time.Minute)))
	}
	r := Evidence{Changes: changes(many...), Routine: 0.5}.Respond(f, 0, t0.Add(period))
	if r.Type != ChangedRoutine || r.Change == nil || !r.Change.At.Equal(t0.Add(4*time.Hour+30*time.Minute)) {
		t.Errorf("eleven changes: %v %+v", r.Type, r.Change)
	}
	// A routine change is chance, not evidence: the page decides, and the
	// routine change stays on the response.
	pages := NewIndex([]Page{{ID: "p", Alerts: []incident.Alert{alert("R")}, Sent: start, Acknowledged: start.Add(time.Minute)}})
	r = Evidence{Changes: changes(many...), Pages: pages, Routine: 0.5}.Respond(f, 0, t0.Add(period))
	if r.Type != Acknowledged || r.Page == nil || r.Page.ID != "p" || r.Change == nil || !r.Change.At.Equal(t0.Add(4*time.Hour+30*time.Minute)) {
		t.Errorf("routine with an acknowledged page: %v %+v %+v", r.Type, r.Page, r.Change)
	}
	if u := Useful(false, false, NoAnswer, r, true); u != NotUseful {
		t.Errorf("acknowledged with a routine change attached, changes known: %v", u)
	}
	un := NewIndex([]Page{{ID: "q", Alerts: []incident.Alert{alert("R")}, Sent: start, Resolved: start.Add(time.Minute), ResolvedBy: ByAlert}})
	r = Evidence{Changes: changes(many...), Pages: un, Routine: 0.5}.Respond(f, 0, t0.Add(period))
	if r.Type != Unanswered || r.Change == nil || Useful(false, false, NoAnswer, r, false) != NotUseful {
		t.Errorf("routine with an unanswered page: %v %+v", r.Type, r.Change)
	}
	open := NewIndex([]Page{{ID: "o", Alerts: []incident.Alert{alert("R")}, Sent: start}})
	if r = (Evidence{Changes: changes(many...), Pages: open, Routine: 0.5}).Respond(f, 0, t0.Add(period)); r.Type != None || r.Page == nil {
		t.Errorf("routine with an open page is none, routine is for no page at all: %v", r.Type)
	}
	if r = (Evidence{Changes: changes(many...), Routine: 0.5}).Respond(f, 0, t0.Add(period)); r.Type != ChangedRoutine {
		t.Errorf("routine with no page: %v", r.Type)
	}
	two := []Change{rollout(web, start.Add(10*time.Minute)), rollout(web, t0)}
	if r := (Evidence{Changes: changes(two...), Routine: 0.5}).Respond(f, 0, t0.Add(period)); r.Type != Changed {
		t.Errorf("two changes: %v", r.Type)
	}
	// Of a routine workload's change and a deploy that isn't, the deploy is
	// what the firing rests on.
	cs := changes(many...)
	cs.AddDeploys([]deploys.Deploy{{Start: start.Add(20 * time.Minute), End: start.Add(20 * time.Minute), Labels: map[string]string{"namespace": "shop"}, Revision: "r9"}})
	r = Evidence{Changes: cs, Routine: 0.5}.Respond(f, 0, t0.Add(period))
	if r.Type != Changed || r.Change.Kind != Deploy {
		t.Errorf("a routine rollout and a deploy: %v %+v", r.Type, r.Change)
	}
	// A change outside the period isn't counted toward n.
	out := append([]Change{}, many[:5]...)
	for i := 0; i < 6; i++ {
		out = append(out, rollout(web, t0.Add(-time.Duration(i+1)*time.Hour)))
	}
	out = append(out, rollout(web, start.Add(10*time.Minute)))
	r = Evidence{Changes: changes(out...), Routine: 0.5}.Respond(f, 0, t0.Add(period))
	if r.Type != Changed {
		t.Errorf("changes before the period were counted: %v", r.Type)
	}
}

func TestRespondPages(t *testing.T) {
	start, end := at(10), at(20)
	f := Firing{Rule: "Disk", Labels: map[string]string{"ns": "shop"}, Start: start, End: end}
	within := 4 * time.Minute
	pg := func(sent int, ack, res int, by string) Page {
		p := Page{ID: "p", Alerts: []incident.Alert{alert("Disk", "ns", "shop")}, Sent: at(sent)}
		if ack > 0 {
			p.Acknowledged = at(ack)
		}
		if res > 0 {
			p.Resolved, p.ResolvedBy = at(res), by
		}
		return p
	}
	for _, c := range []struct {
		name string
		page Page
		want Type
		has  bool
	}{
		{"acknowledged", pg(11, 12, 0, ""), Acknowledged, true},
		{"acknowledged and then resolved by the alert", pg(11, 12, 15, ByAlert), Acknowledged, true},
		{"resolved by a person without acknowledging", pg(11, 0, 15, ByPerson), Acknowledged, true},
		{"not acknowledged, resolved by the alert manager", pg(11, 0, 15, ByAlert), Unanswered, true},
		{"still open and unacknowledged", pg(11, 0, 0, ""), None, true},
		{"sent at the start", pg(10, 0, 15, ByAlert), Unanswered, true},
		{"sent before the start", pg(9, 0, 15, ByAlert), None, false},
		{"sent at the end plus the window", pg(24, 0, 25, ByAlert), Unanswered, true},
		{"sent after the end plus the window", pg(25, 0, 26, ByAlert), None, false},
	} {
		r := Evidence{Pages: NewIndex([]Page{c.page}), Routine: 0.5}.Respond(f, within, end)
		if r.Type != c.want || (r.Page != nil) != c.has {
			t.Errorf("%s: %v page %v, want %v %v", c.name, r.Type, r.Page != nil, c.want, c.has)
		}
	}
	// A page still on when the firing is: its end is now.
	g := f
	g.On, g.End = true, time.Time{}
	if r := (Evidence{Pages: NewIndex([]Page{pg(40, 0, 45, ByAlert)})}).Respond(g, within, at(30)); r.Page != nil {
		t.Errorf("a page sent after now: %+v", r.Page)
	}
	if r := (Evidence{Pages: NewIndex([]Page{pg(33, 0, 45, ByAlert)})}).Respond(g, within, at(30)); r.Type != Unanswered {
		t.Errorf("a page sent in now's window: %v", r.Type)
	}
	// A change and a page: changed, the page still there.
	cs := changes(rollout(web, at(15)))
	cs.From, cs.To = at(0), at(60)
	r := Evidence{Pages: NewIndex([]Page{pg(11, 0, 15, ByAlert)}), Changes: cs}.Respond(
		Firing{Rule: "Disk", Labels: map[string]string{"ns": "shop", "namespace": "shop", "deployment": "web"}, Start: start, End: end}, within, end)
	if r.Type != Changed || r.Page == nil {
		t.Errorf("a change and a page: %v %+v", r.Type, r.Page)
	}
	// Nothing at all.
	if r := (Evidence{}).Respond(f, within, end); r.Type != None || r.Page != nil || r.Change != nil || r.Known {
		t.Errorf("nothing: %+v", r)
	}
}

func TestKnown(t *testing.T) {
	late := Workload{"shop", KindDeployment, "late"}
	cs := NewChangeSet(t0, t0.Add(period))
	cs.AddRollouts(&Rollouts{Coverage: map[Workload][]Span{late: {{t0.Add(4 * time.Hour), t0.Add(period)}}}})
	cs.AddDeploys([]deploys.Deploy{
		{Start: t0.Add(2 * time.Hour), Labels: map[string]string{"service": "pay"}},
		{Start: t0.Add(time.Hour), Labels: map[string]string{"service": "pay"}},
	})
	l := map[string]string{"namespace": "shop", "deployment": "late"}
	for _, c := range []struct {
		name   string
		labels map[string]string
		at     time.Time
		want   bool
	}{
		{"before its series begin", l, t0.Add(4*time.Hour - time.Second), false},
		{"from where they begin", l, t0.Add(4 * time.Hour), true},
		{"to the period's end", l, t0.Add(period), true},
		{"after the period", l, t0.Add(period + time.Second), false},
		{"a workload not read", map[string]string{"namespace": "shop", "deployment": "other"}, t0.Add(5 * time.Hour), false},
		{"a namespace not read", map[string]string{"namespace": "x", "deployment": "late"}, t0.Add(5 * time.Hour), false},
		{"from the earliest deploy listed", map[string]string{"service": "pay"}, t0.Add(time.Hour), true},
		{"before it", map[string]string{"service": "pay"}, t0.Add(time.Hour - time.Second), false},
		{"a target the file doesn't list", map[string]string{"service": "other"}, t0.Add(5 * time.Hour), false},
	} {
		if got := cs.Known(c.labels, nil, c.at, c.at); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// Known over the whole of [from, to]: one merged span holds it all.
	span := NewChangeSet(t0, t0.Add(period))
	span.AddRollouts(&Rollouts{Coverage: map[Workload][]Span{late: {{t0, t0.Add(time.Hour)}, {t0.Add(time.Hour), t0.Add(2 * time.Hour)}, {t0.Add(3 * time.Hour), t0.Add(4 * time.Hour)}}}})
	for _, c := range []struct {
		name     string
		from, to time.Time
		want     bool
	}{
		{"inside one span", t0, t0.Add(30 * time.Minute), true},
		{"across touching spans", t0.Add(30 * time.Minute), t0.Add(90 * time.Minute), true},
		{"out of the span at its end", t0.Add(90 * time.Minute), t0.Add(150 * time.Minute), false},
		{"across a gap", t0.Add(time.Hour), t0.Add(210 * time.Minute), false},
		{"begun after the span", t0.Add(3 * time.Hour), t0.Add(5 * time.Hour), false},
	} {
		if got := span.Known(l, nil, c.from, c.to); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// A deploy covers a firing that carries every label it lists, equal; one
	// that lists none covers nothing.
	dc := NewChangeSet(t0, t0.Add(period))
	dc.AddDeployCoverage(map[string]string{"service": "pay", "env": "prod"}, t0)
	dc.AddDeployCoverage(map[string]string{"service": "pay", "env": "prod"}, t0.Add(-time.Hour))
	dc.AddDeployCoverage(nil, t0)
	dc.AddDeployCoverage(map[string]string{}, t0)
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"all its labels", map[string]string{"service": "pay", "env": "prod", "pod": "x"}, true},
		{"one of its two labels", map[string]string{"service": "pay"}, false},
		{"one value differs", map[string]string{"service": "pay", "env": "stage"}, false},
		{"no label shared", map[string]string{"pod": "x"}, false},
	} {
		if got := dc.Known(c.labels, nil, t0, t0.Add(time.Hour)); got != c.want {
			t.Errorf("deploy cover, %s: %v, want %v", c.name, got, c.want)
		}
	}
	if dc.Known(map[string]string{"service": "pay", "env": "prod"}, nil, t0.Add(-2*time.Hour), t0) {
		t.Error("known before the earliest deploy")
	}
	if n := len(dc.deployCover); n != 1 {
		t.Errorf("%d covers, want 1: equal label sets are one", n)
	}
	var none *ChangeSet
	if none.Known(l, nil, t0, t0) || none.Matching(l, nil, t0, t0.Add(time.Hour)) != nil {
		t.Error("a nil set knew something")
	}
}

func TestUseful(t *testing.T) {
	R := func(t Type) Response { return Response{Type: t} }
	for _, c := range []struct {
		name          string
		white, judged bool
		answer        Answer
		r             Response
		known         bool
		want          Usefulness
	}{
		{"white", true, true, NoAnswer, R(None), false, IsUseful},
		{"white, unanswered page", true, true, NoAnswer, R(Unanswered), false, IsUseful},
		{"white, acknowledged, changes known", true, true, NoAnswer, R(Acknowledged), true, IsUseful},
		{"changed, black and judged", false, true, NoAnswer, R(Changed), true, IsUseful},
		{"changed, not judged", false, false, NoAnswer, R(Changed), false, IsUseful},
		{"a person said yes, nothing else", false, false, AnswerYes, R(None), false, IsUseful},
		{"a person said yes, unanswered", false, true, AnswerYes, R(Unanswered), true, IsUseful},
		{"a person said no, black", false, true, AnswerNo, R(None), false, NotUseful},
		{"a person said no, changed", false, true, AnswerNo, R(Changed), true, NotUseful},
		{"a person said no, white", true, true, AnswerNo, R(None), false, NotUseful},
		{"unanswered, not judged", false, false, NoAnswer, R(Unanswered), false, NotUseful},
		{"unanswered, black", false, true, NoAnswer, R(Unanswered), false, NotUseful},
		{"acknowledged, changes known, not judged", false, false, NoAnswer, R(Acknowledged), true, NotUseful},
		{"acknowledged, changes known, black", false, true, NoAnswer, R(Acknowledged), true, NotUseful},
		{"acknowledged, changes unknown, not judged", false, false, NoAnswer, R(Acknowledged), false, Unknown},
		{"acknowledged, changes unknown, black and judged", false, true, NoAnswer, R(Acknowledged), false, NotUseful},
		{"black and judged, nothing known", false, true, NoAnswer, R(None), false, NotUseful},
		{"black and judged, a routine change", false, true, NoAnswer, R(ChangedRoutine), true, NotUseful},
		{"routine change, no stone to judge it", false, false, NoAnswer, R(ChangedRoutine), true, Unknown},
		{"nothing judged and nothing known", false, false, NoAnswer, R(None), false, Unknown},
		{"nothing judged, changes known, no response", false, false, NoAnswer, R(None), true, Unknown},
	} {
		if got := Useful(c.white, c.judged, c.answer, c.r, c.known); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

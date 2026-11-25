package verdict

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/graph"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
)

// graphAt is a graph with each edge ("a>b") held in the hour t0 is in plus
// the hours after it given in hours (0 for t0's own).
func graphAt(t *testing.T, hours []int, edges ...string) *graph.Graph {
	t.Helper()
	b := graph.NewBuilder()
	for _, h := range hours {
		for _, e := range edges {
			from, to, _ := strings.Cut(e, ">")
			b.Add(t0.Add(time.Duration(h)*time.Hour), from, to)
		}
	}
	return b.Graph()
}

func withGraph(o Options, g *graph.Graph, depth int, direction string) Options {
	o.Depends = &Dependencies{Graph: g, Label: "service", Depth: depth, Direction: direction}
	return o
}

func svcFiring(rule, service string, start, end int, extra ...string) firing.Firing {
	l := map[string]string{"service": service}
	for i := 0; i+1 < len(extra); i += 2 {
		l[extra[i]] = extra[i+1]
	}
	return firing.Firing{Rule: rule, Labels: l, Start: m(start), End: m(end)}
}

func checkoutIncident() incident.Incident {
	return incident.Incident{ID: "INC-1", Start: m(100), End: m(140), Labels: incident.Labels{"service": {"checkout"}}}
}

// The payments database's alert catches checkout's incident when checkout
// called it: by dependency, with its path, as any catch; the same alert
// without the graph, or about a service checkout never called, doesn't.
func TestCatchByDependency(t *testing.T) {
	fs := []firing.Firing{
		svcFiring("PaymentsDBConnections", "payments-db", 95, 130),
		svcFiring("SearchDown", "search", 95, 130),
	}
	in := []incident.Incident{checkoutIncident()}
	g := graphAt(t, []int{0}, "checkout>payments-db", "cart>search")

	without := Judge(nil, fs, in, opts())
	if len(without.Incidents[0].CaughtBy) != 0 {
		t.Fatalf("no graph: caught by %+v", without.Incidents[0].CaughtBy)
	}
	r := Judge(nil, fs, in, withGraph(opts(), g, 1, ""))
	got := catchesOf(r.Incidents[0])
	c, ok := got["PaymentsDBConnections"]
	if !ok || !slices.Equal(c.Dependency, []string{"checkout", "payments-db"}) || c.Delay != -5*time.Minute {
		t.Errorf("PaymentsDBConnections: %+v %v, want by dependency checkout → payments-db, -5m", c, ok)
	}
	if _, ok := got["SearchDown"]; ok || len(got) != 1 {
		t.Errorf("caught by %+v, want PaymentsDBConnections alone", r.Incidents[0].CaughtBy)
	}
	// A catch is a catch: the firing is white, and counts toward precision.
	for _, f := range r.Firings {
		if f.Rule == "PaymentsDBConnections" && !f.White {
			t.Error("the firing by dependency is not white")
		}
		if f.Rule == "SearchDown" && f.White {
			t.Error("a firing no call reaches is white")
		}
	}
	if rr := ruleOf(r, "PaymentsDBConnections"); rr.White != 1 || rr.Black != 0 {
		t.Errorf("PaymentsDBConnections: %+v", rr)
	}
	if r.Settings.Depends == nil || r.Settings.Depends.Edges != 2 || r.Settings.Depends.Depth != 1 || r.Settings.Depends.Direction != "down" || r.Settings.Depends.Label != "service" {
		t.Errorf("settings: %+v", r.Settings.Depends)
	}

	var b bytes.Buffer
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "PaymentsDBConnections -5m (via checkout → payments-db)") {
		t.Errorf("the table doesn't mark the catch:\n%s", out)
	}
	want := "! 1 catch was by dependency, a rule on a service the incident's depends on (the service graph had 2 edges): PaymentsDBConnections. It made 1 firing white that no labels or link caught.\n"
	if !strings.Contains(out, want) {
		t.Errorf("no note after the table, want %q:\n%s", want, out)
	}
	b.Reset()
	if err := JSON(&b, r); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Settings  struct{ Depends map[string]any } `json:"settings"`
		Incidents []struct {
			CaughtBy []struct {
				Rule       string   `json:"rule"`
				Dependency []string `json:"dependency"`
			} `json:"caught_by"`
		} `json:"incidents"`
	}
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if cb := doc.Incidents[0].CaughtBy; len(cb) != 1 || !slices.Equal(cb[0].Dependency, []string{"checkout", "payments-db"}) || doc.Settings.Depends["edges"] != float64(2) {
		t.Errorf("--json: %+v %v", cb, doc.Settings.Depends)
	}
}

// With no firing caught by dependency, the note says the graph's size and
// that none was.
func TestDependsNoteWithNone(t *testing.T) {
	r := Judge(nil, []firing.Firing{svcFiring("SearchDown", "search", 95, 130)}, []incident.Incident{checkoutIncident()},
		withGraph(opts(), graphAt(t, []int{0}, "checkout>payments-db"), 1, ""))
	if got := DependsNote(r); got != "  --depends: the service graph had 1 edge; no catch was by dependency.\n" {
		t.Errorf("note: %q", got)
	}
	if got := DependsNote(Judge(nil, nil, nil, opts())); got != "" {
		t.Errorf("without a graph: %q", got)
	}
}

// Depth: a firing two calls away catches with depth 2, not 1; three with 3;
// the path is the shortest.
func TestDependsDepth(t *testing.T) {
	g := graphAt(t, []int{0}, "checkout>payments", "payments>ledger", "ledger>ledger-db", "checkout>ledger-db2")
	in := []incident.Incident{checkoutIncident()}
	for _, c := range []struct {
		service string
		depth   int
		want    []string
	}{
		{"payments", 1, []string{"checkout", "payments"}},
		{"ledger", 1, nil},
		{"ledger", 2, []string{"checkout", "payments", "ledger"}},
		{"ledger-db", 2, nil},
		{"ledger-db", 3, []string{"checkout", "payments", "ledger", "ledger-db"}},
		{"ledger-db", 9, []string{"checkout", "payments", "ledger", "ledger-db"}}, // 3 at most
		{"unknown", 3, nil},
	} {
		r := Judge(nil, []firing.Firing{svcFiring("R", c.service, 95, 130)}, in, withGraph(opts(), g, c.depth, ""))
		got := catchesOf(r.Incidents[0])["R"]
		if !slices.Equal(got.Dependency, c.want) || (c.want == nil) == (len(r.Incidents[0].CaughtBy) > 0) {
			t.Errorf("%s at depth %d: %+v, want %v", c.service, c.depth, r.Incidents[0].CaughtBy, c.want)
		}
	}
	// Four calls away is out of reach at any depth.
	g4 := graphAt(t, []int{0}, "checkout>a", "a>b", "b>c", "c>d")
	r := Judge(nil, []firing.Firing{svcFiring("R", "d", 95, 130)}, in, withGraph(opts(), g4, 3, ""))
	if len(r.Incidents[0].CaughtBy) != 0 {
		t.Errorf("four calls away caught: %+v", r.Incidents[0].CaughtBy)
	}
}

// Direction: down never lets a caller's alert catch what it calls; both
// does, and its path is from the caller (the firing's service) to the
// callee (the incident's).
func TestDependsDirection(t *testing.T) {
	// The incident is in the payments database; checkout calls it.
	in := []incident.Incident{{ID: "INC-2", Start: m(100), End: m(140), Labels: incident.Labels{"service": {"payments-db"}}}}
	fs := []firing.Firing{svcFiring("CheckoutErrors", "checkout", 101, 130)}
	g := graphAt(t, []int{0}, "checkout>payments-db")
	if r := Judge(nil, fs, in, withGraph(opts(), g, 1, "down")); len(r.Incidents[0].CaughtBy) != 0 {
		t.Errorf("down: %+v", r.Incidents[0].CaughtBy)
	}
	r := Judge(nil, fs, in, withGraph(opts(), g, 1, "both"))
	if c := catchesOf(r.Incidents[0])["CheckoutErrors"]; !slices.Equal(c.Dependency, []string{"checkout", "payments-db"}) {
		t.Errorf("both: %+v", r.Incidents[0].CaughtBy)
	}
	// Down still works with both.
	r = Judge(nil, []firing.Firing{svcFiring("DB", "payments-db", 95, 130)}, []incident.Incident{checkoutIncident()}, withGraph(opts(), g, 1, "both"))
	if c := catchesOf(r.Incidents[0])["DB"]; !slices.Equal(c.Dependency, []string{"checkout", "payments-db"}) {
		t.Errorf("both, the callee's alert: %+v", r.Incidents[0].CaughtBy)
	}
}

// An incident of several services reaches a firing through any of them.
func TestDependsIncidentWithSeveralValues(t *testing.T) {
	in := []incident.Incident{{ID: "INC-3", Start: m(100), End: m(140), Labels: incident.Labels{"service": {"cart", "checkout"}}}}
	g := graphAt(t, []int{0}, "checkout>payments-db")
	r := Judge(nil, []firing.Firing{svcFiring("DB", "payments-db", 95, 130)}, in, withGraph(opts(), g, 1, ""))
	if c := catchesOf(r.Incidents[0])["DB"]; !slices.Equal(c.Dependency, []string{"checkout", "payments-db"}) {
		t.Errorf("%+v", r.Incidents[0].CaughtBy)
	}
}

// A firing agrees on every other label of the match both carry, or it
// doesn't catch by dependency; one that lacks it does, loose.
func TestDependsOtherMatchLabels(t *testing.T) {
	g := graphAt(t, []int{0}, "checkout>payments-db")
	in := []incident.Incident{{ID: "INC-4", Start: m(100), End: m(140), Labels: incident.Labels{"service": {"checkout"}, "cluster": {"eu"}}}}
	o := opts()
	o.Match = []string{"service", "cluster"}
	o = withGraph(o, g, 1, "")
	for _, c := range []struct {
		name  string
		f     firing.Firing
		catch bool
		loose bool
	}{
		{"same cluster", svcFiring("A", "payments-db", 95, 130, "cluster", "eu"), true, false},
		{"another cluster", svcFiring("B", "payments-db", 95, 130, "cluster", "us"), false, false},
		{"no cluster", svcFiring("C", "payments-db", 95, 130), true, true},
	} {
		r := Judge(nil, []firing.Firing{c.f}, in, o)
		got, ok := catchesOf(r.Incidents[0])[c.f.Rule]
		if ok != c.catch || got.Loose != c.loose {
			t.Errorf("%s: %+v %v, want caught %v loose %v", c.name, got, ok, c.catch, c.loose)
		}
	}
	o.MatchStrict = true
	if r := Judge(nil, []firing.Firing{svcFiring("C", "payments-db", 95, 130)}, in, o); len(r.Incidents[0].CaughtBy) != 0 {
		t.Errorf("--match-strict: %+v", r.Incidents[0].CaughtBy)
	}
}

// Timing is as any catch's: not before the lead, not after the end, not
// one already on before the lead.
func TestDependsTiming(t *testing.T) {
	g := graphAt(t, []int{0}, "checkout>payments-db")
	o := withGraph(opts(), g, 1, "")
	in := []incident.Incident{checkoutIncident()}
	for _, c := range []struct {
		name       string
		start, end int
		catch      bool
	}{
		{"at the lead", 85, 90, true},
		{"before the lead", 84, 90, false},
		{"at the end", 140, 150, true},
		{"after the end", 141, 150, false},
	} {
		r := Judge(nil, []firing.Firing{svcFiring("R", "payments-db", c.start, c.end)}, in, o)
		if got := len(r.Incidents[0].CaughtBy) > 0; got != c.catch {
			t.Errorf("%s: caught %v, want %v", c.name, got, c.catch)
		}
	}
	// After the end, with the labels agreeing, is "after its end"; with the
	// graph, so is a firing the graph reaches.
	late := svcFiring("R", "payments-db", 141, 150)
	late.Held = m(120) // its condition held in the incident
	r := Judge(nil, []firing.Firing{late}, in, o)
	if len(r.Incidents[0].FiredAfter) != 1 {
		t.Errorf("fired after its end: %+v", r.Incidents[0].FiredAfter)
	}
	if r := Judge(nil, []firing.Firing{late}, in, opts()); len(r.Incidents[0].FiredAfter) != 0 {
		t.Errorf("no graph, fired after its end: %+v", r.Incidents[0].FiredAfter)
	}
}

// The edges taken are those that held from an hour before the incident
// started to its end: not last week's, not those after.
func TestDependsEdgesOutsideTheWindow(t *testing.T) {
	// The incident is at 01:40 to 02:20 on t0's day (t0 is midnight).
	in := []incident.Incident{{ID: "INC-5", Start: m(100), End: m(140), Labels: incident.Labels{"service": {"checkout"}}}}
	fs := []firing.Firing{svcFiring("R", "payments-db", 95, 130)}
	for _, c := range []struct {
		name  string
		hours []int
		want  bool
	}{
		{"the hour of the start", []int{1}, true},
		{"the hour before the start", []int{0}, true}, // 00:40 is in the hour of 00:00
		{"two hours before", []int{-1}, false},
		{"the hour of the end", []int{2}, true},
		{"after the end", []int{3}, false},
		{"last week", []int{-168}, false},
	} {
		g := graphAt(t, c.hours, "checkout>payments-db")
		r := Judge(nil, fs, in, withGraph(opts(), g, 1, ""))
		if got := len(r.Incidents[0].CaughtBy) > 0; got != c.want {
			t.Errorf("%s: caught %v, want %v", c.name, got, c.want)
		}
	}
}

// Precedence: a catch by a link or by labels is never by dependency, and a
// rule that caught both ways is marked by the way that comes first.
func TestDependsPrecedence(t *testing.T) {
	g := graphAt(t, []int{0}, "checkout>payments-db")
	in := checkoutIncident()
	in.Alerts = []incident.Alert{{Rule: "Linked", Labels: map[string]string{"service": "payments-db"}}}
	fs := []firing.Firing{
		svcFiring("Linked", "payments-db", 101, 130),                                                          // linked and reached
		svcFiring("Labels", "checkout", 101, 130),                                                             // labels agree
		svcFiring("Labels", "payments-db", 102, 130),                                                          // and reached
		svcFiring("Dependency", "payments-db", 103, 130),                                                      // reached only
		svcFiring("Dependency", "payments-db", 104, 130),                                                      // twice
		svcFiring("BothLoose", "payments-db", 105, 130),                                                       // see below
		firing.Firing{Rule: "BothLoose", Labels: map[string]string{"node": "n1"}, Start: m(106), End: m(130)}, // loose by labels
	}
	r := Judge(nil, fs, []incident.Incident{in}, withGraph(opts(), g, 1, ""))
	got := catchesOf(r.Incidents[0])
	if c := got["Linked"]; !c.Linked || c.Dependency != nil {
		t.Errorf("Linked: %+v", c)
	}
	if c := got["Labels"]; c.Linked || c.Dependency != nil {
		t.Errorf("Labels: %+v", c)
	}
	if c := got["Dependency"]; !slices.Equal(c.Dependency, []string{"checkout", "payments-db"}) || c.Delay != 3*time.Minute {
		t.Errorf("Dependency: %+v", c)
	}
	// A rule that also fired with no service (loose by its labels) is a
	// catch by labels, and as loose as that firing is: the graph's firing
	// makes none of its numbers, direct firings give them.
	if c := got["BothLoose"]; c.Dependency != nil || !c.Loose || c.Delay != 6*time.Minute {
		t.Errorf("BothLoose: %+v", c)
	}
	// Linked is checked before the graph is asked: LinkedOnly with links
	// keeps the graph out too.
	o := withGraph(opts(), g, 1, "")
	o.LinkedOnly = true
	r = Judge(nil, fs, []incident.Incident{in}, o)
	for _, c := range r.Incidents[0].CaughtBy {
		if c.Rule != "Linked" {
			t.Errorf("--linked-only: caught by %+v", r.Incidents[0].CaughtBy)
		}
	}
}

// A graph that kept none (too many edges) judges nothing by dependency and
// the report says so.
func TestDependsTooMany(t *testing.T) {
	b := graph.NewBuilder()
	for i := 0; i <= graph.MaxEdgesPerHour; i++ {
		b.Add(t0, "a"+itoa(i), "b")
	}
	b.Add(t0, "checkout", "payments-db")
	g := b.Graph()
	if !g.TooMany() || g.Edges() != 0 {
		t.Fatalf("graph: too many %v, edges %d", g.TooMany(), g.Edges())
	}
	r := Judge(nil, []firing.Firing{svcFiring("R", "payments-db", 95, 130)}, []incident.Incident{checkoutIncident()}, withGraph(opts(), g, 1, ""))
	if len(r.Incidents[0].CaughtBy) != 0 || !r.Settings.Depends.TooMany {
		t.Errorf("%+v %+v", r.Incidents[0].CaughtBy, r.Settings.Depends)
	}
	if got := DependsNote(r); !strings.HasPrefix(got, "! --depends: the service graph had more than 20000 edges in an hour: it kept none") {
		t.Errorf("note: %q", got)
	}
}

func itoa(i int) string {
	var b []byte
	if i == 0 {
		return "0"
	}
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// Near misses are looked for by dependency: About is true of a firing the
// graph reaches, so a rule that fired but too late for the incident is
// "after its end", and a rule held only briefly is looked at.
func TestAboutByDependency(t *testing.T) {
	g := graphAt(t, []int{0}, "checkout>payments-db")
	in := checkoutIncident()
	l := map[string]string{"service": "payments-db"}
	if !withGraph(opts(), g, 1, "").About("R", l, in) || opts().About("R", l, in) {
		t.Error("About: the graph alone makes a firing on payments-db about checkout's incident")
	}
	n := Nearly
	nm, ok := n(in, []firing.Firing{{Rule: "R", Labels: l, Start: m(102), End: m(105)}}, 5*time.Minute, time.Minute, withGraph(opts(), g, 1, ""))
	if !ok || nm.NewFor != 2*time.Minute {
		t.Errorf("Nearly by dependency: %v %+v", ok, nm)
	}
	if _, ok := n(in, []firing.Firing{{Rule: "R", Labels: l, Start: m(102), End: m(105)}}, 5*time.Minute, time.Minute, opts()); ok {
		t.Error("Nearly without the graph found it")
	}
}

// A graph that says everything calls everything: the note shows how many
// catches came by it.
func TestDependsRedact(t *testing.T) {
	g := graphAt(t, []int{0}, "checkout>payments-db")
	r := Judge(nil, []firing.Firing{svcFiring("PaymentsDB", "payments-db", 95, 130)}, []incident.Incident{checkoutIncident()}, withGraph(opts(), g, 1, ""))
	red := NewRedactor(nil).Report(r)
	var b bytes.Buffer
	if err := Text(&b, red, time.UTC); err != nil {
		t.Fatal(err)
	}
	if err := JSON(&b, red); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, name := range []string{"checkout", "payments-db", "PaymentsDB"} {
		if strings.Contains(out, name) {
			t.Errorf("%s is in the redacted report:\n%s", name, out)
		}
	}
	c := red.Incidents[0].CaughtBy[0]
	if len(c.Dependency) != 2 || c.Dependency[0] != red.Incidents[0].Labels["service"][0] {
		t.Errorf("path %v, incident's service %v: the first should be the incident's alias", c.Dependency, red.Incidents[0].Labels)
	}
	if !strings.Contains(out, "(via "+c.Dependency[0]+" → "+c.Dependency[1]+")") {
		t.Errorf("redacted text lacks the path:\n%s", out)
	}
}

// --compare: a change in how a catch came is a change.
func TestCompareSeesDependency(t *testing.T) {
	g := graphAt(t, []int{0}, "checkout>payments-db")
	in := []incident.Incident{checkoutIncident()}
	fs := []firing.Firing{svcFiring("DB", "payments-db", 95, 130)}
	known := []rules.Rule{{Name: "DB", Expr: "x"}}
	before := Judge(known, fs, in, opts())
	after := Judge(known, fs, in, withGraph(opts(), g, 1, ""))
	c := Compare(before, after, []string{"DB"}, 0)
	if len(c.Incidents) != 1 {
		t.Fatalf("incidents changed: %+v", c.Incidents)
	}
	var b bytes.Buffer
	if err := CompareText(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "(via checkout → payments-db)") {
		t.Errorf("compare text:\n%s", b.String())
	}
	b.Reset()
	if err := CompareMarkdown(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "(via checkout → payments-db)") {
		t.Errorf("compare markdown:\n%s", b.String())
	}
}

// A rule with a direct firing has its delay, looseness and precision from
// it: the graph's firings of the same rule change none of them, and the
// note counts the firings white by the graph alone.
func TestDependsDirectNumbers(t *testing.T) {
	g := graphAt(t, []int{0}, "checkout>payments-db", "checkout>cart")
	in := checkoutIncident()
	fs := []firing.Firing{
		svcFiring("Latency", "checkout", 110, 130),   // direct, +10m
		svcFiring("Latency", "payments-db", 90, 130), // by dependency, -10m
		svcFiring("Latency", "cart", 120, 135),       // by dependency, +20m
		svcFiring("Cache", "cart", 95, 130),          // by dependency alone
	}
	r := Judge(nil, fs, []incident.Incident{in}, withGraph(opts(), g, 1, ""))
	got := catchesOf(r.Incidents[0])
	if c := got["Latency"]; c.Delay != 10*time.Minute || c.Dependency != nil || c.Loose {
		t.Errorf("Latency: %+v", c)
	}
	if c := got["Cache"]; c.Delay != -5*time.Minute || !slices.Equal(c.Dependency, []string{"checkout", "cart"}) {
		t.Errorf("Cache: %+v", c)
	}
	by := map[string]RuleResult{}
	for _, ru := range r.Rules {
		by[ru.Rule] = ru
	}
	if l := by["Latency"]; l.White != 3 || l.WhiteByDependency != 2 {
		t.Errorf("Latency: white %d, by dependency %d", l.White, l.WhiteByDependency)
	}
	if c := by["Cache"]; c.WhiteByDependency != 1 {
		t.Errorf("Cache: by dependency %d", c.WhiteByDependency)
	}
	for _, j := range r.Firings {
		if want := j.Labels["service"] != "checkout"; j.ByDependency != want {
			t.Errorf("%s on %s: by dependency %v", j.Rule, j.Labels["service"], j.ByDependency)
		}
	}
	if note := DependsNote(r); !strings.Contains(note, "It made 3 firings white that no labels or link caught.") {
		t.Errorf("note: %q", note)
	}
	// Without a direct firing the graph's earliest one gives the delay.
	r = Judge(nil, []firing.Firing{svcFiring("Latency", "payments-db", 90, 130), svcFiring("Latency", "cart", 120, 135)}, []incident.Incident{in}, withGraph(opts(), g, 1, ""))
	if c := catchesOf(r.Incidents[0])["Latency"]; c.Delay != -10*time.Minute || c.Dependency == nil {
		t.Errorf("by dependency alone: %+v", c)
	}
}

// A service's name is whatever a label said: in a Markdown cell it is a
// code span as a rule's name is, so it makes no table, mention, link or tag.
func TestDependencyMarkMarkdown(t *testing.T) {
	evil := []string{"checkout", "a|b @team [x](http://e.test) <img src=x onerror=1>"}
	got := mdCatches([]Catch{{Rule: "R", Delay: time.Minute, Dependency: evil}}, []string{"service"})
	want := "`R` +1m (via `checkout` → `a\\|b @team [x](http://e.test) <img src=x onerror=1>`)"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if cell := (Catch{Dependency: evil}).marks(nil); !strings.Contains(cell, "via checkout → ") {
		t.Errorf("text: %q", cell)
	}
}

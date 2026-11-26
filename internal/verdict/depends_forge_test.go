package verdict

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/graph"
	"lapilli.dev/internal/incident"
)

// With many incidents and firings, the graph is walked once for each
// incident, not once for each firing and incident: the count of walks is
// the incidents', whatever the firings.
func TestJudgeWalksOncePerIncident(t *testing.T) {
	b := graph.NewBuilder()
	b.Add(t0, "checkout", "payments-db")
	g := b.Graph()
	const incidents, perIncident = 1500, 10 // more incidents than any window cache held
	var ins []incident.Incident
	var fs []firing.Firing
	for i := 0; i < incidents; i++ {
		at := 100 + i*70 // an hour and ten minutes apart: each has its own window
		ins = append(ins, incident.Incident{ID: fmt.Sprint("INC-", i), Start: m(at), End: m(at + 30),
			Labels: incident.Labels{"service": {"checkout"}}})
		for j := 0; j < perIncident; j++ {
			fs = append(fs, svcFiring("DB", "payments-db", at-5+j, at+20))
		}
	}
	o := withGraph(opts(), g, 1, "")
	o.To = t0.Add(120 * 24 * time.Hour)
	r := Judge(nil, fs, ins, o)
	if len(r.Incidents) != incidents {
		t.Fatalf("%d incidents", len(r.Incidents))
	}
	// Each firing is looked up in the walk made for its incident, and none
	// walks again.
	if w := g.Walks(); w > incidents {
		t.Errorf("%d walks for %d incidents and %d firings: want at most one for each incident", w, incidents, len(fs))
	}
	if w := g.Walks(); w == 0 {
		t.Error("no walk was made")
	}
}

func forgedFirings() (fs []firing.Firing, ins []incident.Incident) {
	ins = []incident.Incident{checkoutIncident()}
	// The real alert, by the incident's own label, and another rule's on a
	// service the graph says checkout calls.
	fs = []firing.Firing{svcFiring("CheckoutErrors", "checkout", 102, 130), svcFiring("ForgedDB", "payments-db", 95, 130)}
	return fs, ins
}

// A change that deletes an incident's only real alert leaves another
// rule's catch by dependency: that isn't counted as caught, so the incident
// is missed (and fails --fail-missed), with the reason said, unless
// --depends-count-catches counts it.
func TestHoldMissedDependencyOnly(t *testing.T) {
	g := graphAt(t, []int{0}, "checkout>payments-db")
	for _, count := range []bool{false, true} {
		fs, ins := forgedFirings()
		o := withGraph(opts(), g, 1, "")
		o.Depends.CountCatches = count
		after := Judge(nil, []firing.Firing{fs[1]}, ins, o)
		before := Judge(nil, slices.Clone(fs), ins, o)
		c := Compare(before, after, []string{"CheckoutErrors"}, 0)
		c.Summarize(true)
		c.HoldMissed(nil)
		switch {
		case !count && (len(c.Missed) != 1 || len(c.MissedByDependency) != 1 || c.Summary.IncidentsMissed != 1):
			t.Errorf("not counted: missed %d, by dependency only %d, summary %d: want 1, 1 and 1", len(c.Missed), len(c.MissedByDependency), c.Summary.IncidentsMissed)
		case count && (len(c.Missed) != 0 || len(c.MissedByDependency) != 0):
			t.Errorf("counted: missed %d, by dependency only %d: want none", len(c.Missed), len(c.MissedByDependency))
		}
		var b strings.Builder
		if err := MissedText(&b, c, time.UTC); err != nil {
			t.Fatal(err)
		}
		if said := strings.Contains(b.String(), "only by dependency"); said == count || strings.Contains(b.String(), "The run fails") == count {
			t.Errorf("count %v, said %v: %q", count, said, b.String())
		}
	}
	// With the real alert and the forged one both gone, it is missed.
	fs, ins := forgedFirings()
	o := withGraph(opts(), graphAt(t, []int{0}, "checkout>payments-db"), 1, "")
	c := Compare(Judge(nil, slices.Clone(fs), ins, o), Judge(nil, nil, ins, o), []string{"CheckoutErrors"}, 0)
	c.Summarize(true)
	c.HoldMissed(nil)
	if len(c.Missed) != 1 {
		t.Errorf("missed %d: want 1", len(c.Missed))
	}
}

// A rule isn't called spare for a catch made only by dependency, nor
// does that catch take a rule's FIRST: unless counted.
func TestSpareIgnoresDependencyCatch(t *testing.T) {
	g := graphAt(t, []int{0, 10, 11, 12}, "checkout>payments-db")
	var fs []firing.Firing
	var ins []incident.Incident
	for i := 0; i < 2; i++ {
		ins = append(ins, incident.Incident{ID: fmt.Sprint("INC-", i), Start: m(100 + i*600), End: m(140 + i*600), Labels: incident.Labels{"service": {"checkout"}}})
		fs = append(fs, svcFiring("CheckoutErrors", "checkout", 102+i*600, 130+i*600, "severity", "page"), svcFiring("ForgedDB", "payments-db", 95+i*600, 130+i*600, "severity", "page"))
	}
	for _, count := range []bool{false, true} {
		o := withGraph(opts(), g, 1, "")
		o.Depends.CountCatches = count
		r := Judge(nil, fs, ins, o)
		r.Credit(nil)
		real := ruleOf(r, "CheckoutErrors")
		if real.Spare == !count || (real.Alone == 2) == count {
			t.Errorf("count %v: the real rule is spare %v, alone %d", count, real.Spare, real.Alone)
		}
	}
}

// A path's text can't be forged: a name with an arrow or a comma is escaped
// if an older record holds one, and the graph refuses them.
func TestDependencyMarkEscapes(t *testing.T) {
	got := DependencyMark([]string{"a → b", "c,d", "e‮f"})
	if strings.Count(got, " → ") != 2 || strings.Contains(got, ",") || strings.Contains(got, "‮") {
		t.Errorf("mark %q: want two arrows, the hops' own escaped", got)
	}
	if want := `via a \u{2192} b → c\u{2c}d → e\u{202e}f`; got != want {
		t.Errorf("mark %q, want %q", got, want)
	}
}

func TestDependsHops(t *testing.T) {
	got := DependsHops([]string{"shop/checkout", `<b>"x"&`, "e‮f"})
	want := "<bdi>shop/checkout</bdi> → <bdi>&lt;b&gt;&#34;x&#34;&amp;</bdi> → <bdi>e‮f</bdi>"
	if got != want {
		t.Errorf("hops %q, want %q", got, want)
	}
	if strings.Contains(got, "<b>") {
		t.Error("a name wrote a tag")
	}
}

// An incident a rule left alone catches directly isn't missed nor said to
// be caught only by dependency, whatever the changed rule's catch was.
func TestHoldMissedLeftAloneBeforeDependency(t *testing.T) {
	g := graphAt(t, []int{0}, "checkout>payments-db")
	fs, ins := forgedFirings()
	o := withGraph(opts(), g, 1, "")
	c := Compare(Judge(nil, slices.Clone(fs), ins, o), Judge(nil, []firing.Firing{fs[1]}, ins, o), []string{"CheckoutErrors"}, 0)
	c.Summarize(true)
	c.HoldMissedBy(func(incident.Incident) (bool, bool) { return true, true })
	if len(c.Missed) != 0 || len(c.MissedByDependency) != 0 {
		t.Errorf("missed %d, by dependency %d: a rule left alone catches it, want none", len(c.Missed), len(c.MissedByDependency))
	}
}

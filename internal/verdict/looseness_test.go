package verdict

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// looseWorld is an incident in checkout, and the firings around it: of
// DiskFull, with no service label, which agrees with any; of
// CheckoutLatency, about checkout; of CartErrors, about cart, which
// disagrees; of NodeDown, once with no label and once with it.
func looseWorld() ([]firing.Firing, []incident.Incident) {
	fs := []firing.Firing{
		{Rule: "DiskFull", Labels: map[string]string{"node": "n1"}, Start: m(105), End: m(130)},
		{Rule: "CheckoutLatency", Labels: map[string]string{"service": "checkout"}, Start: m(103), End: m(110)},
		{Rule: "CartErrors", Labels: map[string]string{"service": "cart"}, Start: m(102), End: m(104)},
		{Rule: "NodeDown", Labels: map[string]string{"node": "n2"}, Start: m(101), End: m(102)},
		{Rule: "NodeDown", Labels: map[string]string{"service": "checkout"}, Start: m(110), End: m(112)},
	}
	incs := []incident.Incident{
		{ID: "INC-7", Start: m(100), End: m(140), Labels: incident.Labels{"service": {"checkout"}}},
	}
	return fs, incs
}

// A rule whose firings lack the --match label the incident has catches it,
// as documented, marked loose; one that also fired with the label caught
// it as any other. The text says how many catches were so, and which rules.
func TestLooseCatchMarked(t *testing.T) {
	fs, incs := looseWorld()
	o := opts()
	o.Match = []string{"service"}
	r := Judge(nil, fs, incs, o)
	got := catchesOf(r.Incidents[0])
	if len(got) != 3 || !got["DiskFull"].Loose || got["CheckoutLatency"].Loose || got["NodeDown"].Loose {
		t.Errorf("caught by %+v, want DiskFull alone loose, of the three", r.Incidents[0].CaughtBy)
	}
	var b bytes.Buffer
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "DiskFull +5m (no service)") || strings.Contains(out, "CheckoutLatency +3m (no service)") {
		t.Errorf("the table doesn't mark the loose catch alone:\n%s", out)
	}
	want := "! 1 catch was by a rule with no service label, which agrees with any incident's: DiskFull (--match-strict counts it as not caught).\n"
	if !strings.Contains(out, want) {
		t.Errorf("no note after the table, want %q:\n%s", want, out)
	}
	// With none loose, or no --match, there is no note.
	o.Match = nil
	b.Reset()
	if err := Text(&b, Judge(nil, fs, incs, o), time.UTC); err != nil || strings.Contains(b.String(), "no service") {
		t.Errorf("without --match: %v\n%s", err, b.String())
	}
}

// With --match-strict a firing without the label doesn't catch the
// incident, so a rule that fired only so is no longer one that caught it,
// and its precision is what it was.
func TestMatchStrictDropsLoose(t *testing.T) {
	fs, incs := looseWorld()
	o := opts()
	o.Match = []string{"service"}
	loose := Judge(nil, fs, incs, o)
	o.MatchStrict = true
	r := Judge(nil, fs, incs, o)
	got := catchesOf(r.Incidents[0])
	if _, ok := got["DiskFull"]; ok || len(got) != 2 || got["NodeDown"].Loose {
		t.Errorf("--match-strict: caught by %+v, want CheckoutLatency and NodeDown, by the labelled firing", r.Incidents[0].CaughtBy)
	}
	if !r.Settings.MatchStrict {
		t.Error("settings don't say --match-strict")
	}
	white := func(r *Report, rule string) (int, int) {
		for _, rr := range r.Rules {
			if rr.Rule == rule {
				return rr.White, rr.Firings
			}
		}
		return -1, -1
	}
	if w, n := white(loose, "DiskFull"); w != 1 || n != 1 {
		t.Errorf("DiskFull by default: %d of %d caught, want 1 of 1", w, n)
	}
	if w, n := white(r, "DiskFull"); w != 0 || n != 1 {
		t.Errorf("DiskFull strict: %d of %d caught, want 0 of 1", w, n)
	}
	// NodeDown's firing with no label no longer counts: 1 of 2.
	if w, n := white(r, "NodeDown"); w != 1 || n != 2 {
		t.Errorf("NodeDown strict: %d of %d caught, want 1 of 2", w, n)
	}
	var b bytes.Buffer
	if err := Text(&b, r, time.UTC); err != nil || strings.Contains(b.String(), "no service") {
		t.Errorf("strict text: %v\n%s", err, b.String())
	}
}

// The mark and the note name the labels the firing lacked, of the match in
// effect: an incident that matches on team, under a global --match service,
// is caught loose by a firing with no team, and says team; with several,
// they come in one list.
func TestLooseCatchNamesTheLabelsLacked(t *testing.T) {
	fs := []firing.Firing{{Rule: "DiskFull", Labels: map[string]string{"service": "checkout"}, Start: m(105), End: m(130)}}
	incs := []incident.Incident{{ID: "INC-7", Start: m(100), End: m(140), Match: []string{"team"},
		Labels: incident.Labels{"service": {"checkout"}, "team": {"pay"}}}}
	o := opts()
	o.Match = []string{"service"}
	r := Judge(nil, fs, incs, o)
	c := catchesOf(r.Incidents[0])["DiskFull"]
	if !c.Loose || !slices.Equal(c.Lacked, []string{"team"}) {
		t.Fatalf("caught by %+v, want loose, lacking team", r.Incidents[0].CaughtBy)
	}
	var b bytes.Buffer
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "DiskFull +5m (no team)") || !strings.Contains(out, "with no team label") || strings.Contains(out, "no service") {
		t.Errorf("the text names the wrong label:\n%s", out)
	}

	incs[0].Match = []string{"service", "team"}
	fs[0].Labels = map[string]string{"node": "n1"}
	r = Judge(nil, fs, incs, o)
	if c := catchesOf(r.Incidents[0])["DiskFull"]; !slices.Equal(c.Lacked, []string{"service", "team"}) {
		t.Fatalf("caught by %+v, want lacking service and team", r.Incidents[0].CaughtBy)
	}
	b.Reset()
	if err := Text(&b, r, time.UTC); err != nil || !strings.Contains(b.String(), "DiskFull +5m (no service, team)") || !strings.Contains(b.String(), "with no service, team label") {
		t.Errorf("several: %v\n%s", err, b.String())
	}
}

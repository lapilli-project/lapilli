package verdict

import (
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
)

// TestLasted: the incidents say how long each lasted. Of those nothing
// caught that --impact found over several series, each series' own window
// is written: none of them lasted as long as the incident. And when
// --impact-for counts impact shorter than rules can fire on, it is said.
func TestLasted(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	r := &Report{
		From: t0.Add(-time.Hour), To: t0.Add(time.Hour),
		Incidents: []IncidentResult{
			{Incident: incident.Incident{ID: "INC-1", Title: "down", Start: at(0), End: at(30)},
				CaughtBy: []Catch{{Rule: "Errors", Delay: 5 * time.Minute}}},
			{Incident: incident.Incident{ID: "INC-2", Title: "open", Start: at(40)}},
			{Incident: incident.Incident{ID: "impact-1", Title: "user impact (--impact)", Start: at(6), End: at(16), Candidate: true,
				Spans: []incident.Span{
					{Labels: map[string]string{"service": "cart", "env": "prod"}, Start: at(6), End: at(11)},
					{Labels: map[string]string{"service": "batch", "env": "prod"}, Start: at(6), End: at(16)},
				}}},
		},
		ImpactShort: &ImpactShort{For: 5 * time.Minute, Rules: 3, Judged: 10, Need: 6 * time.Minute, Most: 2},
	}
	var b strings.Builder
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"INCIDENT                         START             LASTED  CAUGHT BY",
		"INC-1 down                       2026-11-02 09:00  30m     Errors +5m",
		"INC-2 open                       2026-11-02 09:40  –",
		"impact-1 user impact (--impact)  2026-11-02 09:06  10m",
		"✗ 2 of 3 incidents had no alert.\n" +
			"  Of those --impact found over several series, each series' own window:\n" +
			"  impact-1: service=cart 09:06–09:11 (5m); service=batch 09:06–09:16 (10m)\n",
		"! --impact-for 5m counts impact that lasted 5m as an incident, but 3 of the 10 rules judged fire only once\n" +
			"  their condition held longer, for their \"for\" and one evaluation (6m for 2 of them): impact shorter\n" +
			"  than that they can't catch. --impact-for 6m counts only what they can.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	// With no incident found by --impact, nothing is said of it.
	r.Incidents = r.Incidents[:2]
	b.Reset()
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "--impact-for") || strings.Contains(b.String(), "each series' own window") {
		t.Errorf("no incident found by --impact:\n%s", b.String())
	}
}

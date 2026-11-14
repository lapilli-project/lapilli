package serve

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// TestIncidentTimeline: an incident's page draws its span, from its lead
// before it to its lead after, and a row for each rule that caught it,
// nearly caught it, or was on around it, those first: twenty rules at
// most, fifty firings of each; and the same as a table.
func TestIncidentTimeline(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	o := s.cfg.Options
	at := func(h, m int) time.Time { return time.Date(2026, 11, 1, h, m, 0, 0, time.UTC) }
	var fs []firing.Firing
	add := func(rule, svc string, start, end time.Time) {
		fs = append(fs, firing.Firing{Rule: rule, Start: start, End: end, Labels: map[string]string{"service": svc, "n": fmt.Sprint(len(fs))}})
	}
	add("PayErrors", "pay", at(9, 55), at(10, 20))   // catches it, 5m before
	add("PayLatency", "pay", at(10, 40), at(10, 41)) // after it ended
	add("Noise", "search", at(9, 50), at(9, 52))
	add("Quiet", "search", at(9, 47), at(9, 48))   // excused
	add("LongOn", "search", at(8, 0), time.Time{}) // on since before, and still
	add("Before", "search", at(8, 0), at(8, 10))   // over before the span
	add("After", "search", at(10, 45), at(10, 50)) // after it
	add("Yesterday", "search", at(9, 0).Add(-24*time.Hour), at(9, 1).Add(-24*time.Hour))
	for k := 0; k < 60; k++ {
		add("Busy", "search", at(9, 45).Add(time.Duration(k)*time.Minute), at(9, 45).Add(time.Duration(k)*time.Minute+20*time.Second))
	}
	for k := 0; k < 25; k++ {
		add(fmt.Sprintf("R%02d", k), "search", at(10, 1).Add(time.Duration(k)*time.Minute), at(10, 2).Add(time.Duration(k)*time.Minute))
	}
	keep(t, s, at(0, 0).Add(-48*time.Hour), at(23, 0), fs)
	if _, err := s.l.PutWindow(ctx, ledger.Window{Title: "w", Start: at(9, 46), End: at(9, 49), Rules: "^Quiet$"}, "ana", o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.Put(ctx, incident.Incident{ID: "I-1", Title: "pay down", Start: at(10, 0), End: at(10, 30), Labels: incident.Labels{"service": {"pay"}}}, "ana", o); err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, o); err != nil { // the window's span
		t.Fatal(err)
	}
	week := s.l.Midnight(ledger.Week(s.l.Day(at(10, 0))))
	if err := s.l.ReplaceResult(ctx, "near_misses", now, week, week.Add(7*24*time.Hour), []verdict.NearMiss{{Incident: "I-1", Rule: "SlowPay",
		For: 5 * time.Minute, NewFor: 2 * time.Minute, Delay: 2 * time.Minute}}); err != nil {
		t.Fatal(err)
	}
	in, err := s.l.IncidentByID(ctx, "I-1")
	if err != nil {
		t.Fatal(err)
	}
	tl, err := s.tallies(ctx, weeks)
	if err != nil {
		t.Fatal(err)
	}
	near, err := s.nearMisses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.timeline(ctx, "en", in, tl, near["I-1"])
	if err != nil {
		t.Fatal(err)
	}
	if !v.From.Equal(at(9, 45)) || !v.To.Equal(at(10, 45)) || v.SpanX != 101 || v.SpanW != 198 {
		t.Fatalf("from %s to %s, the span at %.1f, %.1f wide", v.From, v.To, v.SpanX, v.SpanW)
	}
	rows := map[string]timelineRow{}
	var order []string
	for _, r := range v.Rows {
		rows[r.Rule] = r
		order = append(order, r.Rule)
	}
	// Caught, then near, then by when they were first on: 32 rules on, or
	// nearly, around it; twenty drawn.
	if len(v.Rows) != timelineRows || v.More != 32-timelineRows {
		t.Errorf("%d rows, %d more: %v", len(v.Rows), v.More, order)
	}
	if want := "PayErrors SlowPay LongOn Busy Quiet Noise R00"; strings.Join(order[:7], " ") != want {
		t.Errorf("in order %v, not %s", order, want)
	}
	for _, r := range []string{"Before", "After", "Yesterday"} {
		if _, ok := rows[r]; ok {
			t.Errorf("%s, not on around it, has a row", r)
		}
	}
	pe, sp, lo, busy, q := rows["PayErrors"], rows["SlowPay"], rows["LongOn"], rows["Busy"], rows["Quiet"]
	if pe.Kind != "caught" || pe.Did != "caught it, 5m before" || pe.White != 1 || len(pe.Marks) != 1 || pe.Marks[0].Kind != "caught" || pe.Marks[0].X != 68 {
		t.Errorf("PayErrors: %+v", pe)
	}
	if sp.Kind != "near" || len(sp.Marks) != 1 || sp.Marks[0].Kind != "near" || sp.Marks[0].X != 114.2 || !strings.HasPrefix(sp.Did, `nearly caught it: with "for" 2m, it would have fired 2m after`) {
		t.Errorf("SlowPay: %+v", sp)
	}
	if lo.Kind != "fired" || len(lo.Marks) != 1 || lo.Marks[0].X != 2 || lo.Marks[0].To != 398 || !lo.Marks[0].Ongoing || lo.Black != 1 {
		t.Errorf("LongOn: %+v", lo)
	}
	if busy.Firings != 60 || len(busy.Marks) != timelineFirings || busy.More != 60-timelineFirings || !strings.HasPrefix(busy.Starts, "09:45 ●, 09:46 ●") {
		t.Errorf("Busy: %d firings, %d drawn, %d more, %q", busy.Firings, len(busy.Marks), busy.More, busy.Starts[:20])
	}
	if q.Excused != 1 || q.Marks[0].Kind != "excused" || !v.Excused || !v.Near {
		t.Errorf("Quiet: %+v", q)
	}

	// The page draws it, and says it as a table; a caught incident has no
	// rule that nearly caught it, as its page has none.
	_, body, _ := get(t, s.Handler(), "/incidents/I-1")
	for _, want := range []string{`<section class="card timeline" id="timeline">`, `<rect class="span" x="101" y="0" width="198" height="16"/>`,
		`<circle class="caught" cx="68" cy="8" r="4"/>`, `<circle class="excused" cx="15.2" cy="8" r="4"/>`,
		`aria-label="60 firings: 0 caught something, 60 caught nothing, 0 excused; the same are in the table below"`,
		`<tr><th scope="row"><bdi>Busy</bdi></th><td class="n">60</td><td class="n">0</td><td class="n">60</td><td class="n">0</td><td>09:45 ●`,
		"and 10 more", "11 more rules fired around it, not drawn.", "caught it, 5m before"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s", want)
		}
	}
	if strings.Contains(body, `class="near"`) {
		t.Error("a caught incident drawn as nearly caught")
	}
	// One nothing caught, a rule nearly did: its diamond where it would
	// have fired, at the incident's start.
	_, body, _ = get(t, pagesServer(t).Handler(), "/incidents/impact-20261103T0600Z")
	for _, want := range []string{`<tr class="tl-near"><th scope="row"><a href="/rules/Errors"><bdi>Errors</bdi></a></th>`, `<path class="near" d="M146.9 2L151.9 8L146.9 14L141.9 8Z"/>`,
		"it would have fired at 06:00", "when it would have fired</li>"} {
		if !strings.Contains(body, want) {
			t.Errorf("impact: no %s", want)
		}
	}
}

// TestTimelineRead: the firings read for a timeline are those on in its
// span, the earliest first: the one that caught it is drawn however many
// came after it, and those over before the span aren't counted against
// what is read.
func TestTimelineRead(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	o := s.cfg.Options
	at := func(h, m int) time.Time { return time.Date(2026, 11, 1, h, m, 0, 0, time.UTC) }
	var fs []firing.Firing
	add := func(rule string, start, end time.Time) {
		fs = append(fs, firing.Firing{Rule: rule, Start: start, End: end, Labels: map[string]string{"service": "pay", "n": fmt.Sprint(len(fs))}})
	}
	add("Catch", at(9, 58), at(10, 2))
	for k := 0; k < 60; k++ {
		add("Late", at(10, 5).Add(time.Duration(k)*30*time.Second), at(10, 6).Add(time.Duration(k)*30*time.Second))
		add("Gone", at(9, 0).Add(time.Duration(k)*30*time.Second), at(9, 1).Add(time.Duration(k)*30*time.Second)) // over before 09:45
	}
	keep(t, s, at(0, 0), at(23, 0), fs)
	if _, err := s.l.Put(ctx, incident.Incident{ID: "I-1", Start: at(10, 0), End: at(10, 30), Labels: incident.Labels{"service": {"pay"}}}, "ana", o); err != nil {
		t.Fatal(err)
	}
	in, err := s.l.IncidentByID(ctx, "I-1")
	if err != nil {
		t.Fatal(err)
	}
	tl, err := s.tallies(ctx, weeks)
	if err != nil {
		t.Fatal(err)
	}
	was := timelineRead
	defer func() { timelineRead = was }()
	for _, c := range []struct {
		read   int
		capped bool
	}{{50, true}, {61, false}} {
		timelineRead = c.read
		v, err := s.timeline(ctx, "en", in, tl, nil)
		if err != nil {
			t.Fatal(err)
		}
		rows := map[string]timelineRow{}
		for _, r := range v.Rows {
			rows[r.Rule] = r
		}
		if r := rows["Catch"]; r.Kind != "caught" || r.White != 1 || v.Capped != c.capped || rows["Gone"].Firings != 0 || rows["Late"].Firings != min(60, c.read-1) {
			t.Errorf("%d read: Catch %+v, capped %v, %d late, %d gone", c.read, r, v.Capped, rows["Late"].Firings, rows["Gone"].Firings)
		}
	}
}

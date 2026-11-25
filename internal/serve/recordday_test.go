package serve

import (
	"context"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// An incident on Thursday of a week judges the firings of that day on, and
// the weeks after it: the days before it in its week are counted, not
// judged, by day; every day of the week is judged by week
// (--record-by-week). The tallies, the report and each firing say the same.
func TestTalliesJudgedByDay(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	mon := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	for _, byWeek := range []bool{false, true} {
		s, _ := server(t, func(c *Config) { c.Impact, c.Incidents, c.RecordByWeek = "", "", byWeek })
		var fs []firing.Firing
		for d := range 8 { // October 26 to November 2, at six
			at := mon.Add(time.Duration(d)*24*time.Hour + 6*time.Hour)
			fs = append(fs, firing.Firing{Rule: "Daily", Labels: map[string]string{"service": "a"}, Start: at, End: at.Add(time.Minute)})
		}
		keep(t, s, mon, mon.Add(9*24*time.Hour), fs)
		thu := mon.Add(3*24*time.Hour + 12*time.Hour)
		if _, err := s.l.Put(ctx, incident.Incident{ID: "I-1", Start: thu, End: thu.Add(time.Hour), Labels: incident.Labels{"service": {"b"}}}, "ana", s.cfg.Options); err != nil {
			t.Fatal(err)
		}
		tl, err := s.tallies(ctx, 2)
		if err != nil {
			t.Fatal(err)
		}
		id, w1, w2 := tl.id("Daily"), ledger.Week(s.l.Day(mon)), ledger.Week(s.l.Day(mon))+7
		unjudged := map[bool]int{false: 3, true: 0}[byWeek]
		if got := tl.unjudged(w1, id).Firings; got != unjudged || tl.by[w1][id].Firings != 7 || tl.judged(w1, id).Firings != 7-unjudged {
			t.Errorf("byWeek %v: the first week has %d judged and %d not, of %d", byWeek, tl.judged(w1, id).Firings, got, tl.by[w1][id].Firings)
		}
		if tl.unjudged(w2, id).Firings != 0 || !tl.kept[w1] || !tl.kept[w2] {
			t.Errorf("byWeek %v: the second week: %+v kept %v %v", byWeek, tl.unjudged(w2, id), tl.kept[w1], tl.kept[w2])
		}
		// Days are read apart only for the week the incident starts in.
		if split := tl.split[w1] && !tl.split[w2]; split == byWeek {
			t.Errorf("byWeek %v: split %v", byWeek, tl.split)
		}
		// As the report counts the same span, and each firing is asked.
		o, err := s.options(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := s.l.Report(ctx, mon, mon.Add(9*24*time.Hour), o, true)
		if err != nil || rep.Unjudged != unjudged {
			t.Errorf("byWeek %v: the report has %d not judged, %v", byWeek, rep.Unjudged, err)
		}
		judge, err := s.judgeOver(ctx, mon, mon.Add(9*24*time.Hour), o, nil)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, f := range fs {
			if !judge("Daily", f.Start, 0) {
				n++
			}
		}
		if n != unjudged {
			t.Errorf("byWeek %v: %d firings not judged one by one", byWeek, n)
		}
		if o.Settings().RecordBy != map[bool]string{false: "day", true: "week"}[byWeek] {
			t.Errorf("byWeek %v: settings say %q", byWeek, o.Settings().RecordBy)
		}
	}
}

// --record-by-week, given or taken away at a start, judges every firing
// again once, as --linked-only does: the ledger says what it was last
// judged by, with a w for the week; the default, by day, has none, so a
// ledger judged before it isn't judged again on the upgrade. The settings
// say which.
func TestRecordByWeekJudgesAgainOnce(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	n := 0
	was := judgeAll
	judgeAll = func(l *ledger.Ledger, ctx context.Context, o verdict.Options, done func(float64)) error {
		n++
		return was(l, ctx, o, done)
	}
	t.Cleanup(func() { judgeAll = was })
	// What the page and the API say, which a process's flag fixes.
	by := func() string {
		v, err := s.settingsView(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return v.RecordBy
	}
	if _, js, _ := get(t, s.Handler(), "/api/v1/settings"); !strings.Contains(js, `"record_by": "day"`) {
		t.Errorf("the API doesn't say record_by: %s", js)
	}
	start := func() {
		s.linkedKnown.Lock()
		s.linkedKnown.read = false
		s.linkedKnown.Unlock()
		s.jobs(ctx, false)
	}
	// A ledger judged before this release, by its flags alone (j2).
	if err := s.l.SetMeta(ctx, linkedMeta, judgedBy); err != nil {
		t.Fatal(err)
	}
	start()
	if v, _ := s.l.Meta(ctx, linkedMeta); n != 0 || v != judgedBy || by() != "day" {
		t.Fatalf("by day, as the ledger was: judged all %d times, it says %q, %s", n, v, by())
	}
	s.cfg.RecordByWeek = true
	if by() != "week" {
		t.Errorf("--record-by-week: the settings say %s", by())
	}
	start()
	start()
	if v, _ := s.l.Meta(ctx, linkedMeta); n != 1 || v != judgedBy+"w" {
		t.Fatalf("--record-by-week given: judged all %d times, it says %q; want once", n, v)
	}
	s.cfg.RecordByWeek = false
	start()
	start()
	if v, _ := s.l.Meta(ctx, linkedMeta); n != 2 || v != judgedBy || by() != "day" {
		t.Errorf("--record-by-week taken away: judged all %d times, it says %q, %s; want twice in all", n, v, by())
	}
}

// A team is judged by the incidents it owns: an incident of its own that
// another's long one outlasts still ends its two weeks, so a week whose
// days everyone's record judges alike has its days read apart for the team
// (tallies.only), and the team's rule is judged day by day.
func TestTeamJudgedByItsOwnDays(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Impact, c.Incidents = "", "" })
	s.now = func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) }
	mon := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	var fs []firing.Firing
	for d := -1; d < 6; d++ { // Sunday November 8 to Saturday the 14th, at six
		at := mon.Add(time.Duration(d)*24*time.Hour + 6*time.Hour)
		fs = append(fs, firing.Firing{Rule: "PayErrors", Labels: map[string]string{"service": "pay"}, Start: at, End: at.Add(time.Minute)})
	}
	keep(t, s, mon.Add(-48*time.Hour), mon.Add(10*24*time.Hour), fs)
	o := s.cfg.Options
	oct28 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, in := range []incident.Incident{
		{ID: "P-1", Start: oct28, End: oct28.Add(time.Hour), Labels: incident.Labels{"service": {"pay"}}},
		{ID: "L-1", Start: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), Labels: incident.Labels{"service": {"search"}}},
	} {
		if _, err := s.l.Put(ctx, in, "ana", o); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "^Pay", Labels: incident.Labels{"service": {"pay"}}}, "ana"); err != nil {
		t.Fatal(err)
	}
	tl, err := s.tallies(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	id, w := tl.id("PayErrors"), ledger.Week(s.l.Day(mon))
	if u := tl.unjudged(w, id); u.Firings != 0 || tl.by[w][id].Firings != 6 {
		t.Fatalf("everyone's: %d not judged of %d", u.Firings, tl.by[w][id].Firings)
	}
	own, err := s.owners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	team, _ := own.team("payments")
	if err := s.only(ctx, tl, own, team); err != nil {
		t.Fatal(err)
	}
	// P-1's two weeks end on the 11th at 13:00: the 9th to the 11th are
	// judged, the 12th to the 14th are not.
	if u := tl.unjudged(w, id); u.Firings != 3 || tl.judged(w, id).Firings != 3 || !tl.split[w] {
		t.Errorf("the team's: %d not judged, %d judged; split %v", u.Firings, tl.judged(w, id).Firings, tl.split)
	}
}

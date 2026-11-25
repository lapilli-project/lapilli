package serve

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
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

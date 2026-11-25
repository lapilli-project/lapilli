package serve

import (
	"context"
	"encoding/json"
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

// --record-by-week judges nothing again, given or taken away: no stone
// depends on it, so the ledger's meta says the same either way, and the
// settings say which a process runs by.
func TestRecordByWeekJudgesNothingAgain(t *testing.T) {
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
	by := func() string {
		v, err := s.settingsView(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return v.RecordBy
	}
	if _, js, _ := get(t, s.Handler(), "/api/v1/settings"); !strings.Contains(js, `"record_by": "day"`) || strings.Contains(js, `"linked_only_judging": true`) {
		t.Errorf("the API: %s", js)
	}
	meta, _ := s.l.Meta(ctx, linkedMeta)
	for _, week := range []bool{true, false} {
		s.cfg.RecordByWeek = week
		s.linkedKnown.Lock()
		s.linkedKnown.read = false
		s.linkedKnown.Unlock()
		s.jobs(ctx, false)
		v, _ := s.l.Meta(ctx, linkedMeta)
		if n != 0 || v != meta || by() != map[bool]string{true: "week", false: "day"}[week] {
			t.Errorf("by week %v: judged all %d times, meta %q (was %q), settings say %s", week, n, v, meta, by())
		}
		if view, err := s.settingsView(ctx); err != nil || view.LinkedOnlyJudging {
			t.Errorf("by week %v: the badge under --linked-only says it is judging again (%v)", week, err)
		}
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

// An incident that started before the ledger's first replayed time is not
// missed: no rule could have fired for it. It is shown apart, and kept out
// of every count that says missed: the overview's week, the summary, the
// incidents page, a report.
func TestIncidentBeforeReplaysNotMissed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Impact, c.Incidents = "", "" })
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	f := ledger.Found{From: from, To: now, Run: ledger.Run{Kind: "backfill", Started: now, Finished: now, From: from, To: now}}
	if err := s.l.Record(ctx, f, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	o := s.cfg.Options
	before := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	after := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, in := range []incident.Incident{
		{ID: "OLD-1", Start: before, End: before.Add(time.Hour), Labels: incident.Labels{"service": {"a"}}},
		{ID: "NEW-1", Start: after, End: after.Add(time.Hour), Labels: incident.Labels{"service": {"a"}}},
	} {
		if _, err := s.l.Put(ctx, in, "ana", o); err != nil {
			t.Fatal(err)
		}
	}
	old, err := s.l.IncidentByID(ctx, "OLD-1")
	if err != nil || !old.Unreplayed {
		t.Fatalf("OLD-1: unreplayed %v, %v", old.Unreplayed, err)
	}
	if nw, _ := s.l.IncidentByID(ctx, "NEW-1"); nw.Unreplayed {
		t.Error("NEW-1 started after the first replayed time")
	}
	h := s.Handler()
	_, body, _ := get(t, h, "/incidents?weeks=12")
	text := tagsOut(body)
	for _, want := range []string{"✗ missed (1)", "? before what was replayed (1)", "1 started before what was replayed"} {
		if !strings.Contains(text, want) {
			t.Errorf("/incidents doesn't say %q", want)
		}
	}
	// The summary says it apart: the week of the first, 26 October.
	_, js, _ := get(t, h, "/api/v1/summary")
	var sum struct {
		Rows []struct {
			From      time.Time
			Team      string
			Incidents struct{ Total, Missed, Unreplayed int }
		}
	}
	if err := json.Unmarshal([]byte(js), &sum); err != nil {
		t.Fatal(err, js)
	}
	var total, missed, unreplayed int
	for _, r := range sum.Rows {
		if r.Team == "" {
			total, missed, unreplayed = total+r.Incidents.Total, missed+r.Incidents.Missed, unreplayed+r.Incidents.Unreplayed
		}
	}
	if total != 2 || missed != 1 || unreplayed != 1 {
		t.Errorf("summary: %d incidents, %d missed, %d unreplayed", total, missed, unreplayed)
	}
	// /metrics: a fourth state, so that the four add up to the incidents.
	_, prom, _ := get(t, h, "/metrics")
	for _, want := range []string{`lapilli_incidents{state="unreplayed",source="written"} 1`, `lapilli_incidents{state="missed",source="written"} 1`} {
		if !strings.Contains(prom, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	// A report: the old one is not known, not missed.
	opt, _ := s.options(ctx)
	rep, err := s.l.Report(ctx, from.Add(-14*24*time.Hour), now, opt, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range rep.Incidents {
		if in.NotKnown != (in.ID == "OLD-1") {
			t.Errorf("%s: not known %v", in.ID, in.NotKnown)
		}
	}
	tl, err := s.tallies(ctx, 12)
	if err != nil {
		t.Fatal(err)
	}
	if st := tl.stateAll(old, ledger.Week(s.l.Day(before))); st != "unreplayed" {
		t.Errorf("state %s", st)
	}
}

// tagsOut is a page's text: its tags gone, its white space one space.
func tagsOut(b string) string {
	var out strings.Builder
	in := false
	for _, r := range b {
		switch {
		case r == '<':
			in = true
			out.WriteByte(' ')
		case r == '>':
			in = false
		case !in:
			out.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

// A week with an incident on Thursday and its rule's firings Monday to
// Wednesday: no day of it is without an incident's reach for the week to
// have "no incident written down"; its firings of the days before the
// incident are said unjudged, per firing, and the summary agrees with the
// report that the week has a record.
func TestPartKeptWeekNotices(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Impact, c.Incidents = "", "" })
	s.now = func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }
	mon := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	var fs []firing.Firing
	for d := range 3 {
		at := mon.Add(time.Duration(d)*24*time.Hour + 6*time.Hour)
		fs = append(fs, firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "a"}, Start: at, End: at.Add(time.Minute)})
	}
	keep(t, s, mon.Add(-24*time.Hour), mon.Add(9*24*time.Hour), fs)
	thu := mon.Add(3*24*time.Hour + 12*time.Hour)
	if _, err := s.l.Put(ctx, incident.Incident{ID: "I-1", Start: thu, End: thu.Add(time.Hour), Labels: incident.Labels{"service": {"b"}}}, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	_, body, _ := get(t, h, "/")
	text := tagsOut(body)
	if strings.Contains(text, "No incident written down for the week of 2026-11-09") ||
		!strings.Contains(text, "3 of the 3 firings of the week of 2026-11-09 came with no incident written down on their day") {
		t.Errorf("the overview: %s", text[strings.Index(text, "Your alerts"):][:400])
	}
	_, body, _ = get(t, h, "/rules?weeks=4")
	text = tagsOut(body)
	if strings.Contains(text, "No incident written down for these weeks") || strings.Contains(text, "weeks shown had no incident written down") ||
		!strings.Contains(text, "3 of the 3 firings of the weeks shown came with no incident written down on their day") {
		t.Errorf("the rules list: %.500s", text)
	}
	// The report and the summary agree: the week has a record.
	_, js, _ := get(t, h, "/api/v1/report?from=2026-11-09T00:00:00Z&to=2026-11-16T00:00:00Z")
	var rep verdict.Report
	if err := json.Unmarshal([]byte(js), &rep); err != nil || rep.NoRecord || rep.Unjudged != 3 {
		t.Fatalf("report: no record %v, %d unjudged, %v", rep.NoRecord, rep.Unjudged, err)
	}
	_, js, _ = get(t, h, "/api/v1/summary?from=2026-11-09T00:00:00Z&to=2026-11-16T00:00:00Z")
	var sum struct {
		Rows []struct {
			From     time.Time
			Team     string
			NoRecord bool `json:"no_record"`
			Unjudged int
		}
	}
	if err := json.Unmarshal([]byte(js), &sum); err != nil || len(sum.Rows) == 0 {
		t.Fatal(err, js)
	}
	for _, r := range sum.Rows {
		if r.Team == "" && r.From.Equal(mon) && (r.NoRecord || r.Unjudged != 3) {
			t.Errorf("summary: no record %v, %d unjudged", r.NoRecord, r.Unjudged)
		}
	}
}

// A week some of whose days an incident keeps: the rule that fired often on
// the days with no record is worked out by the digest all the same, from
// those days' firings, and shown while its firings are unjudged.
func TestDigestUnrecordedPartKeptWeek(t *testing.T) {
	s, _ := server(t, func(c *Config) { c.Backfill = 10 * 24 * time.Hour; c.Impact = "" })
	s.h = pastBound{s.h}
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	// October 15 and its two weeks keep October 26 to 29: Errors fired
	// fourteen times on Sunday November 1.
	oct15 := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	if _, err := s.l.Put(ctx, incident.Incident{ID: "EARLY-1", Start: oct15, End: oct15.Add(time.Hour), Labels: incident.Labels{"service": {"zzz"}}}, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	from := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	r, ok, err := s.l.ForPeriod(ctx, "unrecorded", from)
	if err != nil || !ok {
		t.Fatalf("kept %v %v", ok, err)
	}
	var us []verdict.Unrecorded
	if err := r.Decode(&us); err != nil || len(us) != 1 || us[0].Rule != "Errors" || us[0].Firings != 14 || us[0].Alerts != 1 {
		t.Fatalf("unrecorded: %+v %v", us, err)
	}
	tl, err := s.tallies(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	lines, _, err := s.ruleLines(ctx, "en", tl, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if l.Name == "Errors" && (l.Unjudged != 14 || l.Unrecord == nil) {
			t.Errorf("Errors: %d unjudged, digest's %v", l.Unjudged, l.Unrecord != nil)
		}
	}
}

// Woken for nothing is counted one way: by the day the firing started, the
// days with no record left out, whether notifications are kept or not. A
// repeat sent at 01:00 for a firing begun at 21:00 the day before is that
// day's.
func TestWokenByTheFiringsDay(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Impact, c.Incidents = "", ""; c.Options.Hours = verdict.DefaultHours(time.UTC) })
	s.now = func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }
	mon := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	at := func(day, h int) time.Time {
		return mon.Add(time.Duration(day)*24*time.Hour + time.Duration(h)*time.Hour)
	}
	labels := map[string]string{"service": "a"}
	fs := []firing.Firing{
		{Rule: "Errors", Labels: labels, Start: at(1, 2), End: at(1, 3)},                             // Tuesday 02:00: asleep, no record
		{Rule: "Errors", Labels: map[string]string{"service": "b"}, Start: at(2, 21), End: at(3, 3)}, // Wednesday 21:00, no record
		{Rule: "Errors", Labels: map[string]string{"service": "c"}, Start: at(4, 2), End: at(4, 3)},  // Friday 02:00: asleep, kept
	}
	keep(t, s, mon.Add(-24*time.Hour), mon.Add(9*24*time.Hour), fs)
	thu := at(3, 12)
	if _, err := s.l.Put(ctx, incident.Incident{ID: "I-1", Start: thu, End: thu.Add(time.Hour), Labels: incident.Labels{"service": {"z"}}}, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	// Without notifications: the hours of the days judged.
	tl, err := s.tallies(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	by, err := s.byHours(ctx, tl)
	if err != nil {
		t.Fatal(err)
	}
	w, id := ledger.Week(s.l.Day(mon)), tl.id("Errors")
	if got := by[w][id].AsleepBlack; got != 1 {
		t.Errorf("asleep for nothing, no notifications: %d, want the Friday's 1", got)
	}
	// With them: Wednesday's firing is told of at 01:00 on Thursday, a day
	// the incident keeps.
	for _, f := range fs {
		sent := f.Start.Add(time.Minute)
		if f.Start.Equal(at(2, 21)) {
			sent = at(3, 1)
		}
		if _, err := s.l.Notify(ctx, ledger.Notification{Source: "alertmanager", Receiver: "oncall", Group: f.Labels["service"], At: sent,
			Alerts: []ledger.Alert{{Rule: f.Rule, Labels: f.Labels, Start: f.Start}}}); err != nil {
			t.Fatal(err)
		}
	}
	own, err := s.owners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byWeek := map[int64]*weekView{w: {}}
	if _, ok, err := s.woken(ctx, tl, own, nil, byWeek); err != nil || !ok {
		t.Fatalf("woken: %v %v", ok, err)
	}
	// Tuesday's, and Wednesday's told of on Thursday, are of days with no
	// record; Friday's is counted.
	if got := byWeek[w].Asleep; got != 1 {
		t.Errorf("woken for nothing, with notifications: %d, want Friday's 1", got)
	}
}

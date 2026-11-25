package ledger

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// byDay is opts, judged by the day in l's zone, as lapilli serve does by
// default.
func byDay(l *Ledger) verdict.Options {
	o := opts
	o.RecordUnit, o.RecordDay = l.DayOf, true
	return o
}

// One firing a day at six in the morning, from Monday November 2 for n days.
func dailyFirings(from time.Time, n int) []firing.Firing {
	var fs []firing.Firing
	for d := range n {
		at := from.Add(time.Duration(d)*24*time.Hour + 6*time.Hour)
		fs = append(fs, firing.Firing{Rule: "Restarts", Labels: map[string]string{"service": "a"}, Start: at, End: at.Add(10 * time.Minute)})
	}
	return fs
}

// Judged by day, an incident on Thursday November 5 at noon judges Thursday
// on, to two weeks after it ended: Friday November 6 to the 19th, the day
// it ends on its own end plus the two weeks (13:00 on the 19th) among them;
// the days before it, and from the 20th, are counted, not judged. Judged
// by week, as before, every week of the three has its record kept.
func TestReportJudgedByDay(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	mon := t0
	record(t, l, mon, mon.Add(21*24*time.Hour), dailyFirings(mon, 21))
	nov5 := mon.Add(3*24*time.Hour + 12*time.Hour)
	if _, err := l.SyncFile(ctx, []incident.Incident{{ID: "INC-1", Start: nov5, End: nov5.Add(time.Hour), Labels: incident.Labels{"service": {"b"}}}}, opts); err != nil {
		t.Fatal(err)
	}
	to := mon.Add(21 * 24 * time.Hour)
	o := byDay(l)
	rep, err := l.Report(ctx, mon, to, o, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.NoRecord || rep.Unjudged != 6 || rep.Rules[0].Firings != 21 || rep.Rules[0].Unjudged != 6 {
		t.Errorf("by day: %v %d %+v", rep.NoRecord, rep.Unjudged, rep.Rules[0])
	}
	// Each firing says so the same: November 2 to 4, and the 20th to the
	// 22nd, are not judged.
	for _, j := range rep.Firings {
		d := int(j.Start.Sub(mon) / (24 * time.Hour))
		if want := d < 3 || d > 17; j.NoRecord != want {
			t.Errorf("day %d: no record %v", d, j.NoRecord)
		}
		rec, err := l.RecordOf(ctx, mon, to, o, nil)
		if err != nil {
			t.Fatal(err)
		}
		if judged := l.FiringJudged(rec, o, "Restarts", j.Start, 0); judged == j.NoRecord {
			t.Errorf("day %d: FiringJudged %v, the report's no record %v", d, judged, j.NoRecord)
		}
	}
	// As the stones are counted from a span's days.
	if n, wh, un, err := l.StonesJudged(ctx, mon, to, o); err != nil || n != 21 || wh != 0 || un != 6 {
		t.Errorf("StonesJudged: %d %d %d %v", n, wh, un, err)
	}
	// Part of a week: Thursday to Saturday, all judged; Monday to Wednesday
	// none, though the week has an incident.
	if rep, err := l.Report(ctx, mon.Add(3*24*time.Hour), mon.Add(6*24*time.Hour), o, false); err != nil || rep.NoRecord || rep.Unjudged != 0 {
		t.Errorf("Thursday to Saturday: %v %+v %v", rep.NoRecord, rep, err)
	}
	if rep, err := l.Report(ctx, mon, mon.Add(3*24*time.Hour), o, false); err != nil || !rep.NoRecord || rep.Unjudged != 3 {
		t.Errorf("Monday to Wednesday: %v %d %v", rep.NoRecord, rep.Unjudged, err)
	}
	if kept, err := l.RecordKept(ctx, mon, mon.Add(3*24*time.Hour), o); err != nil || kept {
		t.Errorf("RecordKept Monday to Wednesday: %v %v", kept, err)
	}
	if kept, err := l.RecordKept(ctx, mon, mon.Add(4*24*time.Hour), o); err != nil || !kept {
		t.Errorf("RecordKept Monday to Thursday: %v %v", kept, err)
	}
	// By week, as before: every week is kept.
	rep, err = l.Report(ctx, mon, to, opts, true)
	if err != nil || rep.NoRecord || rep.Unjudged != 0 {
		t.Errorf("by week: %v %d %v", rep.NoRecord, rep.Unjudged, err)
	}
}

// Days next to one another judged alike are counted together: a span of
// weeks with the same record on every day reads as many statements as one
// day does, whatever its length.
func TestRunsByDay(t *testing.T) {
	l := open(t, time.UTC)
	o := byDay(l)
	rec := verdict.RecordOf([]incident.Incident{{ID: "INC-1", Start: t0.Add(10 * 24 * time.Hour), End: t0.Add(10*24*time.Hour + time.Hour)}}, o)
	runs := l.runs(rec, o, t0, t0.Add(56*24*time.Hour))
	// Not kept before the 12th, kept to two weeks after it, not kept after.
	if len(runs) != 3 {
		t.Fatalf("%d runs: %+v", len(runs), runs)
	}
	if runs[0].from != t0.Unix() || runs[2].to != t0.Add(56*24*time.Hour).Unix() || runs[0].to != runs[1].from || runs[1].to != runs[2].from {
		t.Errorf("runs don't join up: %+v", runs)
	}
	if l.UnitKept(rec, o, l.day(runs[0].from)) || !l.UnitKept(rec, o, l.day(runs[1].from)) || l.UnitKept(rec, o, l.day(runs[2].from)) {
		t.Errorf("runs: %+v", runs)
	}
	// By week, the span's eight weeks are three runs as well.
	w := opts
	w.RecordUnit = l.WeekOf
	if n := len(l.runs(verdict.RecordOf([]incident.Incident{{ID: "INC-1", Start: t0.Add(10 * 24 * time.Hour), End: t0.Add(10*24*time.Hour + time.Hour)}}, w), w, t0, t0.Add(56*24*time.Hour))); n != 3 {
		t.Errorf("by week: %d runs", n)
	}
}

// lapilli verdict judges a firing by the record at its own time, lapilli
// serve by its day's: they say the same of every firing but those the
// design says (docs/design/judging.md, "The record, by day"): one that
// started before an incident on the same day, which keeps the day's
// record, and the one on the day an incident's two weeks end on, before
// them. Days are of --tz: a firing at 00:30 on a day in Seoul is judged by
// that day, though it was the day before in UTC.
func TestVerdictAndServeJudgeAlike(t *testing.T) {
	ctx := context.Background()
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skip("no tz database:", err)
	}
	for _, loc := range []*time.Location{time.UTC, seoul} {
		l := open(t, loc)
		hour := time.Hour
		// Local midnight of the incident's day.
		d0 := time.Date(2026, 11, 5, 0, 0, 0, 0, loc)
		start := d0.Add(12 * hour)
		inc := incident.Incident{ID: "INC-1", Start: start, End: start.Add(hour), Labels: incident.Labels{"service": {"b"}}}
		ends := inc.End.Add(verdict.RecordLookback)
		fireAt := []struct {
			name string
			at   time.Time
			// differs says the two don't agree, and the design says why.
			differs bool
		}{
			{"the day before, late", d0.Add(-time.Minute), false},
			{"the day before, just after midnight", d0.Add(-23*hour - 59*time.Minute), false},
			{"the same day, before the incident", d0.Add(time.Minute), true},
			{"the same day, an hour before the incident", start.Add(-hour), true},
			{"the same day, after the incident began", start.Add(time.Minute), false},
			{"the day after", d0.Add(25 * hour), false},
			{"two weeks to the hour after it ended", ends, false},
			{"the day it ends its two weeks, after them", ends.Add(30 * time.Minute), true},
			{"the next day", d0.AddDate(0, 0, 15).Add(time.Hour), false},
		}
		var fs []firing.Firing
		for _, f := range fireAt {
			fs = append(fs, firing.Firing{Rule: "Restarts", Labels: map[string]string{"service": "a"}, Start: f.at, End: f.at.Add(5 * time.Minute)})
		}
		from, to := d0.AddDate(0, 0, -3), d0.AddDate(0, 0, 20)
		record(t, l, from, to, fs)
		if _, err := l.SyncFile(ctx, []incident.Incident{inc}, opts); err != nil {
			t.Fatal(err)
		}
		vo := opts
		vo.From, vo.To, vo.Zone = from, to, loc
		vrep := verdict.Judge(nil, fs, []incident.Incident{inc}, vo)
		o := byDay(l)
		srep, err := l.Report(ctx, from, to, o, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(vrep.Firings) != len(fs) || len(srep.Firings) != len(fs) {
			t.Fatalf("%v: %d and %d firings", loc, len(vrep.Firings), len(srep.Firings))
		}
		byStart := func(js []verdict.Judged) map[int64]bool {
			m := map[int64]bool{}
			for _, j := range js {
				m[j.Start.Unix()] = j.NoRecord
			}
			return m
		}
		v, s := byStart(vrep.Firings), byStart(srep.Firings)
		for _, f := range fireAt {
			if (v[f.at.Unix()] != s[f.at.Unix()]) != f.differs {
				t.Errorf("%v, %s: verdict says no record %v, serve %v; they should differ: %v", loc, f.name, v[f.at.Unix()], s[f.at.Unix()], f.differs)
			}
		}
	}
}

// The jq docs/numbers.md gives to count a serve report's no record again
// says it by day and by week, as the report has it.
func TestNumbersNoRecordByDay(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	doc, err := os.ReadFile("../../docs/numbers.md")
	if err != nil {
		t.Fatal(err)
	}
	var again string
	for _, b := range regexp.MustCompile("(?s)```jq\n(.*?)```").FindAllStringSubmatch(string(doc), -1) {
		if strings.HasPrefix(b[1], "# no record (lapilli serve): counted again\n") {
			again = b[1]
		}
	}
	if again == "" {
		t.Fatal("numbers.md has no no-record recount for serve")
	}
	ctx := context.Background()
	l := open(t, time.UTC)
	mon := t0
	record(t, l, mon, mon.Add(21*24*time.Hour), dailyFirings(mon, 21))
	nov5 := mon.Add(3*24*time.Hour + 12*time.Hour)
	if _, err := l.SyncFile(ctx, []incident.Incident{{ID: "INC-1", Start: nov5, End: nov5.Add(time.Hour), Labels: incident.Labels{"service": {"b"}}}}, opts); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]verdict.Options{"day": byDay(l), "week": func() verdict.Options { w := opts; w.RecordUnit = l.WeekOf; return w }()} {
		rep, err := l.Report(ctx, mon, mon.Add(21*24*time.Hour), o, true)
		if err != nil {
			t.Fatal(err)
		}
		rep.Settings = o.Settings()
		var buf bytes.Buffer
		if err := verdict.JSON(&buf, rep); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(t.TempDir(), "report.json")
		if err := os.WriteFile(file, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		run := func(prog string) string {
			out, err := exec.Command("jq", "-c", "-S", "--argjson", "offset", "0", prog, file).CombinedOutput()
			if err != nil {
				t.Fatalf("jq: %v: %s", err, out)
			}
			return strings.TrimSpace(string(out))
		}
		have, want := run("[.firings[] | .no_record // false]"), run(again)
		if have != want {
			t.Errorf("by %s: the report has %s, the recount %s", name, have, want)
		}
		if strings.Count(have, "true") != map[string]int{"day": 6, "week": 0}[name] {
			t.Errorf("by %s: %s", name, have)
		}
	}
}

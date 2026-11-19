package ledger

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// slow stretches the budgets under the race detector, which slows SQLite
// many times over (race_test.go), and on CI runners. race says the race
// detector is on: the tests that hold big runs to a budget skip under it,
// and only under it.
var (
	slow = 1.0
	race bool
)

// GitHub's runners write SQLite at less than half the speed of a laptop,
// and unevenly, while the other packages' tests run beside these: a week of
// 100,000 firings copied in takes 2 s on a laptop and up to 5 s there, on
// macOS and, since the recipe tests made internal/serve's run twice as long,
// on Linux too. The budgets hold as they are on a laptop; on those runners
// they are doubled.
func init() {
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		slow = max(slow, 2)
	}
}

// TestScale fills a ledger with twelve weeks of a busy set of rules, as
// backfill and hourly runs would, and holds what a person waits on to a
// budget: an hourly run, writing an incident, reading twelve weeks, a
// week's report. LAPILLI_SCALE multiplies the firings (default 1: about
// 100,000) and makes the budgets fail the test; without it they are
// logged.
func TestScale(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	k := 1.0
	if v := os.Getenv("LAPILLI_SCALE"); v != "" {
		k, _ = strconv.ParseFloat(v, 64)
	}
	ctx := context.Background()
	l := open(t, time.UTC)
	r := rand.New(rand.NewPCG(1, 2))
	const rules, weeks = 400, 12
	perHour := int(100 * k)
	start := t0.Add(-weeks * 7 * 24 * time.Hour)
	services := []string{"checkout", "search", "cart", "auth", "billing", "media"}
	hour := func(at time.Time) []firing.Firing {
		var fs []firing.Firing
		for i := 0; i < perHour; i++ {
			s := at.Add(time.Duration(r.IntN(3600)) * time.Second).Truncate(time.Minute)
			fs = append(fs, firing.Firing{
				Rule:   fmt.Sprintf("Rule%03d", r.IntN(rules)),
				Labels: map[string]string{"service": services[r.IntN(len(services))], "pod": fmt.Sprintf("p%d", r.IntN(50)), "severity": "page"},
				Start:  s, End: s.Add(time.Duration(1+r.IntN(60)) * time.Minute),
			})
		}
		return fs
	}
	var incs []incident.Incident
	for i := 0; i < weeks*40; i++ {
		s := start.Add(time.Duration(r.IntN(weeks*7*24*60)) * time.Minute)
		incs = append(incs, incident.Incident{ID: fmt.Sprintf("INC-%d", i), Start: s, End: s.Add(45 * time.Minute),
			Labels: incident.Labels{"service": {services[r.IntN(len(services))]}}})
	}
	if _, err := l.SyncFile(ctx, incs, opts); err != nil {
		t.Fatal(err)
	}

	// Backfill, a day at a time.
	began := time.Now()
	total := 0
	for d := start; d.Before(t0); d = d.Add(24 * time.Hour) {
		var fs []firing.Firing
		for h := 0; h < 24; h++ {
			fs = append(fs, hour(d.Add(time.Duration(h)*time.Hour))...)
		}
		total += len(fs)
		f := Found{From: d, To: d.Add(24*time.Hour - time.Second), Firings: fs, Run: Run{Kind: "backfill", Started: d, Finished: d}}
		if err := l.Record(ctx, f, opts); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("backfill: %d firings in %s", total, time.Since(began).Round(time.Millisecond))

	budget := func(what string, max time.Duration, f func()) {
		t.Helper()
		began := time.Now()
		f()
		took := time.Since(began)
		t.Logf("%s: %s", what, took.Round(time.Microsecond))
		if max = time.Duration(float64(max) * slow); took > max {
			// A shared runner's clock is no measure: the budget fails
			// the test only when LAPILLI_SCALE is set, on a machine kept
			// for measuring; otherwise it is logged.
			if os.Getenv("LAPILLI_SCALE") != "" {
				t.Errorf("%s took %s, over %s", what, took, max)
			} else {
				t.Logf("over budget, not timed: %s took %s, over %s", what, took, max)
			}
		}
	}
	budget("an hourly run", 2*time.Second, func() {
		fs := append(hour(t0.Add(-2*time.Hour)), hour(t0.Add(-time.Hour))...)
		f := Found{From: t0.Add(-2 * time.Hour), To: t0, Firings: fs, Run: Run{Kind: "hourly", Started: t0, Finished: t0}}
		if err := l.Record(ctx, f, opts); err != nil {
			t.Fatal(err)
		}
	})
	budget("writing an incident", time.Second, func() {
		if _, err := l.Put(ctx, incident.Incident{Start: t0.Add(-30 * time.Hour), End: t0.Add(-28 * time.Hour),
			Labels: incident.Labels{"service": {"checkout"}}}, "ana", opts); err != nil {
			t.Fatal(err)
		}
	})
	budget("twelve weeks by rule", time.Second, func() {
		w := Week(l.Day(t0))
		ws, err := l.Weeks(ctx, w-7*weeks, w)
		if err != nil || len(ws) == 0 {
			t.Fatal(ws, err)
		}
	})
	budget("a week's incidents", time.Second, func() {
		if _, err := l.Incidents(ctx, t0.Add(-7*24*time.Hour), t0, true); err != nil {
			t.Fatal(err)
		}
	})
	budget("a page of firings", 500*time.Millisecond, func() {
		fs, err := l.Firings(ctx, FiringQuery{From: start, To: t0, Rule: "Rule007", Limit: 100})
		if err != nil || len(fs) == 0 {
			t.Fatal(err)
		}
		if _, err := l.Firings(ctx, FiringQuery{From: start, To: t0, Rule: "Rule007", Limit: 100, Before: fs[len(fs)-1].ID}); err != nil {
			t.Fatal(err)
		}
	})
	budget("a week's report, without firings", 3*time.Second, func() {
		if _, err := l.Report(ctx, t0.Add(-7*24*time.Hour), t0, opts, false); err != nil {
			t.Fatal(err)
		}
	})
	// Marked windows of hours: a sale each Friday evening, a launch each
	// morning, a campaign of three days. Each firing is set beside the
	// few occurrences that started within the longest before it
	// (docs/design/marked.md), counted, not timed.
	fri := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)
	if _, err := l.SyncMarked(ctx, []Marked{
		{Kind: "sale", Title: "Friday sale", Start: fri, End: fri.Add(4 * time.Hour), Every: "week"},
		{Kind: "launch", Title: "Morning launch", Start: fri.Add(-9 * time.Hour), End: fri.Add(-7 * time.Hour), Every: "day"},
		{Kind: "campaign", Title: "Campaign", Start: t0.Add(-20 * 24 * time.Hour), End: t0.Add(-17 * 24 * time.Hour), Labels: map[string]string{"service": "search"}},
	}, opts, MarkedRead{}); err != nil {
		t.Fatal(err)
	}
	most, looked := 0, 0
	markLooked = func(n int) { most, looked = max(most, n), looked+1 }
	defer func() { markLooked = nil }()
	budget("judging everything again", time.Duration(60*k)*time.Second, func() {
		o := opts
		o.Lead = 10 * time.Minute
		if err := l.JudgeAll(ctx, o, nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("marked windows: %d firings set beside %d occurrences at most", looked, most)
	if looked < total || most > 5 {
		t.Errorf("marked windows: %d firings set beside occurrences, %d at most; want every firing, beside 5 at most: the three days of the campaign hold three mornings and a Friday", looked, most)
	}
	if n, err := l.Firings(ctx, FiringQuery{From: start, To: t0, Kind: "sale", Limit: 10}); err != nil || len(n) == 0 {
		t.Errorf("no firing marked with the sale: %v", err)
	}
	var size int64
	l.r.QueryRow("SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()").Scan(&size)
	t.Logf("ledger: %.1f MB, %d bytes a firing", float64(size)/1e6, size/int64(total))
}

// TestRecordedScale holds copying in a week of a busy source, 100,000
// firings, to what the design asks of it (docs/design/recorded.md), by
// what the write does rather than how long it takes on the machine it
// runs on: written, each firing its row, its alert's and its stones once,
// judged over the period once; sent again unchanged, nothing written but
// the source's own rows, and nothing judged. Postgres counts the
// statements too: a thousand rows at a time, and none for each firing.
// How long each took is logged.
func TestRecordedScale(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	if race {
		// Some thirty times slower here, and counted the same: the
		// recorded tests run under it all the same.
		t.Skip("the race detector")
	}
	ctx := context.Background()
	l := open(t, time.UTC)
	r := rand.New(rand.NewPCG(3, 4))
	services := []string{"checkout", "search", "cart", "auth", "billing", "media"}
	week := 7 * 24 * time.Hour
	var fs []firing.Firing
	for len(fs) < MaxRecorded {
		s := t0.Add(time.Duration(r.Int64N(int64(week))) / time.Second * time.Second)
		f := firing.Firing{
			Rule:   fmt.Sprintf("alarm-%03d", r.IntN(300)),
			Labels: map[string]string{"service": services[r.IntN(len(services))], "instance": fmt.Sprintf("i-%04d", r.IntN(2000))},
			Start:  s,
		}
		if r.IntN(50) > 0 {
			f.End = s.Add(time.Duration(1+r.IntN(90)) * time.Minute)
		}
		fs = append(fs, f)
	}
	var incs []incident.Incident
	for i := 0; i < 40; i++ {
		s := t0.Add(time.Duration(r.IntN(7*24*60)) * time.Minute)
		incs = append(incs, incident.Incident{ID: fmt.Sprintf("INC-%d", i), Start: s, End: s.Add(45 * time.Minute),
			Labels: incident.Labels{"service": {services[r.IntN(len(services))]}}})
	}
	if _, err := l.SyncFile(ctx, incs, opts); err != nil {
		t.Fatal(err)
	}
	var spans [][2]int64
	judging = func(lo, hi int64) { spans = append(spans, [2]int64{lo, hi}) }
	defer func() { judging = nil }()
	// What a write did: the rows it wrote, as SQLite counts them on its one
	// writer's connection, and the statements sent to Postgres.
	type did struct{ rows, statements int64 }
	doing := func() did {
		if l.d == postgres {
			return did{statements: sent.Load()}
		}
		var n int64
		if err := l.w.QueryRowContext(ctx, "SELECT total_changes()").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return did{rows: n}
	}
	put := func(what string) (Recorded, did) {
		t.Helper()
		spans = nil
		was, began := doing(), time.Now()
		got, err := l.PutFirings(ctx, "busy", t0, t0.Add(week), fs, FiringsCopier, opts)
		took, is := time.Since(began), doing()
		if err != nil {
			t.Fatal(err)
		}
		d := did{is.rows - was.rows, is.statements - was.statements}
		t.Logf("%s: %d added, %d rows written, %d statements, judged %d times, in %s", what, got.Added, d.rows, d.statements, len(spans), took.Round(time.Millisecond))
		return got, d
	}

	got, d := put("a week of 100,000 firings")
	if got.Added == 0 || got.Removed != 0 {
		t.Fatalf("written: %+v, want the firings added", got)
	}
	// Judged once, over the period: from a lead before it to as far as a
	// firing reached, and no further than a period past it.
	if lo, hi := t0.Add(-opts.Lead), t0.Add(week+MaxRecordedPeriod+opts.RefireWithin); len(spans) != 1 || spans[0][0] < lo.Unix() || spans[0][1] > hi.Unix() {
		t.Errorf("judged %v, want once, within [%d, %d]", spans, lo.Unix(), hi.Unix())
	}
	// Each firing its row, its alert's when it is new (nearly every one
	// here: 300 rules on 2,000 instances), and its stones once; each rule
	// its row, and its days and weeks counted again, deleted and written
	// (16 at most). Writing the firings twice, or judging them twice, is
	// past it.
	rules := map[string]bool{}
	for _, f := range fs {
		rules[f.Rule] = true
	}
	if most := 3*int64(got.Added) + 16*int64(len(rules)); l.d != postgres && d.rows > most {
		t.Errorf("written: %d rows, want %d at most", d.rows, most)
	}
	// In Postgres, a few statements for each rule made (looked up, its name
	// checked, made: 4), and the firings, their alerts and stones a
	// thousand rows a statement. One for each firing is past it.
	if most := 4*int64(len(rules)) + 4*int64(got.Added)/1000 + 50; l.d == postgres && d.statements > most {
		t.Errorf("written: %d statements, want %d at most: a few for each rule, a thousand rows each", d.statements, most)
	}

	got, d = put("the same again")
	if got.Changed() {
		t.Fatalf("sent again: %+v, want nothing changed", got)
	}
	if len(spans) != 0 {
		t.Errorf("sent again: judged %v, want nothing judged", spans)
	}
	// The source's row, and its spans of copies (copied); nothing of its
	// firings. In Postgres, each rule looked up once, and a few.
	if l.d != postgres && d.rows > 4 {
		t.Errorf("sent again: %d rows written, want the source's own at most (4)", d.rows)
	}
	if most := int64(len(rules)) + 20; l.d == postgres && d.statements > most {
		t.Errorf("sent again: %d statements, want %d at most: one for each rule, none for each firing", d.statements, most)
	}
}

// At the bounds, each firing is set beside the occurrences of its week at
// most: 256, however long the longest is (a window of 31 days beside 255
// of an hour, over five weeks).
func TestMarkedScale(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := Week(l.Day(t0)) // a Monday
	from := l.Midnight(start - 28)
	var ms []Marked
	ms = append(ms, Marked{Kind: "sale", Title: "Month of sales", Start: from, End: from.Add(31*24*time.Hour - time.Second)})
	for w := range 5 {
		for i := range MarkedWeek - 1 {
			s := l.Midnight(start - 28 + int64(7*w)).Add(time.Duration(i) * 39 * time.Minute)
			ms = append(ms, Marked{Kind: fmt.Sprint("k", i%4), Title: fmt.Sprint("w", w, " ", i), Start: s, End: s.Add(time.Hour)})
		}
	}
	if _, err := l.SyncMarked(ctx, ms, opts, MarkedRead{}); err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(3, 4))
	var fs []firing.Firing
	for range 5000 {
		s := from.Add(time.Duration(r.IntN(35*24*60)) * time.Minute)
		fs = append(fs, firing.Firing{Rule: fmt.Sprint("Rule", r.IntN(20)), Labels: map[string]string{"service": "checkout"}, Start: s, End: s.Add(time.Minute)})
	}
	most, looked := 0, 0
	markLooked = func(n int) { most, looked = max(most, n), looked+1 }
	defer func() { markLooked = nil }()
	record(t, l, from, from.Add(35*24*time.Hour), fs)
	if looked < len(fs) || most > MarkedWeek {
		t.Errorf("%d firings set beside occurrences, %d at most; want every firing, beside %d at most", looked, most, MarkedWeek)
	}
	t.Logf("%d firings, beside %d occurrences at most", looked, most)
}

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

// TestScale fills a ledger with twelve weeks of a busy set of rules, as
// backfill and hourly runs would, and holds what a person waits on to a
// budget: an hourly run, writing an incident, reading twelve weeks, a
// week's report. LAPILLI_SCALE multiplies the firings (default 1: about
// 100,000); budgets are generous, for a slow CI runner.
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
		if took > max {
			t.Errorf("%s took %s, over %s", what, took, max)
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
	budget("judging everything again", time.Duration(60*k)*time.Second, func() {
		o := opts
		o.Lead = 10 * time.Minute
		if err := l.JudgeAll(ctx, o, nil); err != nil {
			t.Fatal(err)
		}
	})
	var size int64
	l.r.QueryRow("SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()").Scan(&size)
	t.Logf("ledger: %.1f MB, %d bytes a firing", float64(size)/1e6, size/int64(total))
}

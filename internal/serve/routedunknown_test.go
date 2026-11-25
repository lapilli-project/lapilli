package serve

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/routing"
	"lapilli.dev/internal/verdict"
)

// pagesOver reads the pages of the rule Errors in [lo, hi) as the pages
// show them, with whether they are known.
func pagesOver(t *testing.T, s *Server, lo, hi time.Time) (bool, int) {
	t.Helper()
	s.paged.Lock()
	s.paged.p = nil
	s.paged.Unlock()
	p, err := s.notified(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range p.events {
		if ev.Rule == "Errors" && !ev.At.Before(lo) && ev.At.Before(hi) {
			n++
		}
	}
	return p.known(lo, hi), n
}

// One hour not worked out on the first days of a firing that goes on for
// weeks makes the pages of that hour's week not known, and leaves the weeks
// after it as they were: known, with their pages, not none.
func TestUnknownHourKeepsLaterWeeksOfALongFiring(t *testing.T) {
	am := routedFile(t, routedRepeatAM)
	s, _ := server(t, func(c *Config) { c.Notifications, c.AlertmanagerConfig = true, am })
	ctx := context.Background()
	base := now.Add(-20 * 24 * time.Hour).Truncate(24 * time.Hour)
	end := base.Add(14 * 24 * time.Hour)
	f := firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "old"}, Start: base}
	if err := s.l.Record(ctx, ledger.Found{From: base, To: end, Run: ledger.Run{Kind: "test", Started: base, Finished: base, From: base, To: end}, Firings: []firing.Firing{f}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	for at := base; at.Before(end); at = at.Add(time.Hour) {
		s.workOutRouted(ctx, at, at.Add(time.Hour))
	}
	lo, hi := base.Add(7*24*time.Hour), base.Add(14*24*time.Hour)
	k, n := pagesOver(t, s, lo, hi)
	if !k || n < 20 {
		t.Fatalf("the second week before: known %v, %d pages", k, n)
	}
	bad := base.Add(26 * time.Hour)
	if err := s.l.MarkRoutedUnknown(ctx, bad, bad.Add(time.Hour), "stopped"); err != nil {
		t.Fatal(err)
	}
	// The hours worked out after it are worked out again.
	for at := lo; at.Before(lo.Add(3 * time.Hour)); at = at.Add(time.Hour) {
		s.workOutRouted(ctx, at, at.Add(time.Hour))
	}
	if k2, n2 := pagesOver(t, s, lo, hi); !k2 || n2 != n {
		t.Errorf("the second week after one hour of the first wasn't worked out: known %v, %d pages, was %d", k2, n2, n)
	}
	if k1, _ := pagesOver(t, s, base, lo); k1 {
		t.Error("the first week, with an hour not worked out, reads known")
	}
}

// Hourly runs with one hour in the middle that failed: that hour's week is
// not known and no other, and every other week is what it is when every hour
// is good.
func TestHourlyRunsWithOneFailedHour(t *testing.T) {
	am := routedFile(t, routedRepeatAM)
	ctx := context.Background()
	base := now.Add(-24 * 24 * time.Hour).Truncate(24 * time.Hour)
	end := base.Add(21 * 24 * time.Hour)
	run := func(failed time.Time) (known [3]bool, pages [3]int) {
		s, _ := server(t, func(c *Config) { c.Notifications, c.AlertmanagerConfig = true, am })
		f := firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "old"}, Start: base.Add(30 * time.Minute)}
		if err := s.l.Record(ctx, ledger.Found{From: base, To: end, Run: ledger.Run{Kind: "test", Started: base, Finished: base, From: base, To: end}, Firings: []firing.Firing{f}}, s.cfg.Options); err != nil {
			t.Fatal(err)
		}
		for at := base; at.Before(end); at = at.Add(time.Hour) {
			if at.Equal(failed) {
				if err := s.l.MarkRoutedUnknown(ctx, at, at.Add(time.Hour), "failed"); err != nil {
					t.Fatal(err)
				}
				continue
			}
			s.workOutRouted(ctx, at, at.Add(time.Hour))
		}
		for w := range 3 {
			known[w], pages[w] = pagesOver(t, s, base.Add(time.Duration(w)*7*24*time.Hour), base.Add(time.Duration(w+1)*7*24*time.Hour))
		}
		return
	}
	goodK, goodN := run(time.Time{})
	for w := range 3 {
		if !goodK[w] || goodN[w] < 20 {
			t.Fatalf("week %d of the good run: known %v, %d pages", w, goodK[w], goodN[w])
		}
	}
	k, n := run(base.Add(10*24*time.Hour + 5*time.Hour))
	for w := range 3 {
		if w == 1 {
			if k[w] {
				t.Error("the week of the failed hour reads known")
			}
			continue
		}
		if !k[w] || n[w] != goodN[w] {
			t.Errorf("week %d: known %v, %d pages; with every hour good %d", w, k[w], n[w], goodN[w])
		}
	}
}

// A receiver's recordings make the hours it has them in known for it alone:
// a Watchdog's receiver recorded every hour doesn't make an hour not worked
// out known for the receivers that page for the firings.
func TestRecordedByAnotherReceiverDoesNotMakeAnHourKnown(t *testing.T) {
	am := routedFile(t, routedRepeatAM)
	s, _ := server(t, func(c *Config) { c.Notifications, c.AlertmanagerConfig = true, am })
	ctx := context.Background()
	base := now.Add(-3 * 24 * time.Hour).Truncate(time.Hour)
	h := base.Add(5 * time.Hour)
	f := firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "old"}, Start: h.Add(time.Minute), End: h.Add(40 * time.Minute)}
	if err := s.l.Record(ctx, ledger.Found{From: base, To: base.Add(24 * time.Hour), Run: ledger.Run{Kind: "test", Started: base, Finished: base, From: base, To: base.Add(24 * time.Hour)}, Firings: []firing.Firing{f}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	for at := base.Add(-2 * time.Hour); at.Before(base.Add(24 * time.Hour)); at = at.Add(time.Hour) {
		if _, err := s.l.Notify(ctx, ledger.Notification{At: at.Add(2 * time.Minute), Source: "webhook", Receiver: "deadman", Group: "w", Reason: ledger.ReasonRepeat,
			Alerts: []ledger.Alert{{Rule: "Watchdog", Labels: map[string]string{"alertname": "Watchdog"}, Start: base.Add(-48 * time.Hour)}}}); err != nil {
			t.Fatal(err)
		}
	}
	for at := base; at.Before(base.Add(24 * time.Hour)); at = at.Add(time.Hour) {
		s.workOutRouted(ctx, at, at.Add(time.Hour))
	}
	day := base.Add(24 * time.Hour)
	if k, n := pagesOver(t, s, base, day); !k || n == 0 {
		t.Fatalf("worked out: known %v, %d pages", k, n)
	}
	if err := s.l.MarkRoutedUnknown(ctx, h, h.Add(time.Hour), "it spent its budget"); err != nil {
		t.Fatal(err)
	}
	if k, n := pagesOver(t, s, base, day); k {
		t.Errorf("an hour not worked out, a Watchdog recorded in it: known with %d pages", n)
	}
}

// For a receiver, the hour is known as recorded where it has recordings, as
// worked out where the hour is covered, and otherwise not known if it pages.
func TestKnownPerReceiverAndHour(t *testing.T) {
	h := func(n int) time.Time {
		return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Hour)
	}
	cover := ledger.RoutedCover{{From: h(0), To: h(5), Worked: true}, {From: h(5), To: h(6), Worked: false, Why: "stopped"}, {From: h(6), To: h(10), Worked: true}}
	p := &paged{routed: true, routedFrom: h(0), upTo: h(10), first: h(0), cover: cover,
		pagers:   map[string]bool{"ring": true, "deadman": true},
		recorded: map[string]map[int64]bool{"deadman": {h(5).Unix(): true}}}
	if p.known(h(0), h(10)) {
		t.Error("ring doesn't record the hour not worked out: known")
	}
	p.recorded["ring"] = map[int64]bool{h(5).Unix(): true}
	if !p.known(h(0), h(10)) {
		t.Error("both record the hour: not known")
	}
	delete(p.recorded, "deadman")
	if p.known(h(0), h(10)) {
		t.Error("only ring records it: known")
	}
	p.pagers = map[string]bool{"ring": true}
	if !p.known(h(0), h(10)) {
		t.Error("deadman doesn't page: it needs nothing")
	}
	p.pagers = nil
	if p.known(h(0), h(10)) {
		t.Error("the configuration isn't read, and an hour isn't worked out: known")
	}
}

// A firing that began within the hour before a run's lead-in, and whose
// group first flushes after the lead-in begins, is not carried: its first
// notification and its repeats are where one run over its whole life puts
// them.
func TestFiringJustBeforeTheLeadInIsNotCarried(t *testing.T) {
	am := routedFile(t, `route:
  receiver: ring
  group_by: [alertname]
  group_wait: 45m
  group_interval: 5m
  repeat_interval: 4h
receivers:
  - name: ring
    pushover_configs: [{user_key: u, token: t}]
`)
	s, _ := server(t, func(c *Config) { c.Notifications, c.AlertmanagerConfig = true, am })
	ctx := context.Background()
	base := now.Add(-5 * 24 * time.Hour).Truncate(24 * time.Hour)
	end := base.Add(14 * time.Hour)
	f := firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "old"}, Start: base.Add(30 * time.Minute), End: end}
	if err := s.l.Record(ctx, ledger.Found{From: base, To: end, Run: ledger.Run{Kind: "test", Started: base, Finished: base, From: base, To: end}, Firings: []firing.Firing{f}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	cfg := s.routingConfig(ctx)
	if cfg == nil {
		t.Fatal("no configuration")
	}
	var truth []time.Time
	res, err := cfg.Run(verdict.NotificationAlerts([]verdict.Judged{{Firing: f}}, nil), routing.Options{Until: end})
	if err != nil || res.Stopped != "" {
		t.Fatalf("at once: %q %v", res.Stopped, err)
	}
	for _, nt := range res.Notifications {
		if nt.Page {
			truth = append(truth, nt.At)
		}
	}
	for at := base; at.Before(end); at = at.Add(time.Hour) {
		s.workOutRouted(ctx, at, at.Add(time.Hour))
	}
	rows, _, err := s.l.RoutedPages(ctx, base, end, 1000)
	if err != nil || len(rows) != len(truth) || len(truth) < 3 {
		t.Fatalf("pages: %d, at once %d: %v", len(rows), len(truth), err)
	}
	for i, r := range rows {
		if !r.At.Equal(truth[i]) {
			t.Errorf("page %d at %v, at once %v", i, r.At, truth[i])
		}
	}
}

// A piece the catch-up's shared time cut off isn't a failure to back off
// from: it is tried again at the next round, not after an hour, two, four.
func TestCatchUpCutOffByTheSharedTimeIsNotAFailure(t *testing.T) {
	parallel(t)
	s, _ := workedServer(t, nil)
	ctx := context.Background()
	_, to, _, _ := routedSpan(s.l, ctx)
	fs, _, _ := s.l.FiringsOverlapping(ctx, now.Add(-4*24*time.Hour), now, now.Add(-4*24*time.Hour), 1000)
	var d1 time.Time
	for _, x := range fs {
		if x.Rule == "Errors" {
			d1 = x.Start.Truncate(time.Hour)
			break
		}
	}
	if d1.IsZero() {
		t.Fatal("no firing")
	}
	if err := s.l.MarkRoutedUnknown(ctx, d1, d1.Add(time.Hour), "a test's failure"); err != nil {
		t.Fatal(err)
	}
	// The round's time is over as its first piece begins.
	s.routed.limits = routedLimits{catchUp: 10 * time.Millisecond, inPiece: func() { time.Sleep(30 * time.Millisecond) }}
	s.catchUpRouted(ctx, to)
	if c, _ := s.l.RoutedCover(ctx); c.Covers(d1, d1.Add(time.Hour)) {
		t.Fatal("the piece came out in no time")
	}
	if len(s.routed.tried) != 0 {
		t.Errorf("a piece cut off by the round's time counts as a failure: %+v", s.routed.tried)
	}
	// One that fails by itself does, and waits.
	s.routed.limits = routedLimits{budget: 1}
	s.catchUpRouted(ctx, to)
	if len(s.routed.tried) == 0 {
		t.Error("a piece that stopped isn't one to wait for")
	}
	for _, x := range s.routed.tried {
		if x.failed != 1 {
			t.Errorf("failed %d", x.failed)
		}
	}
}
